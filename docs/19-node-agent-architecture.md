# Node Agent Architecture

> **Design principle:** The Control Plane is the brain. The Node Agent is the executor.
> The agent NEVER makes placement or scheduling decisions.

---

## Overview

```
┌─────────────────────────────────────────────────────────────────┐
│                    NEXUS CONTROL PLANE                          │
│                                                                 │
│  Scheduler ──► Placement Engine ──► Task Manager               │
│                                          │                      │
│  Model Registry ◄──────────────────── Runtime Tracker          │
│  Runtime Watcher ◄─── health state ─── (via agent reports)     │
└──────────────────────────────┬──────────────────────────────────┘
                               │  HTTP/HTTPS REST
                               │  JWT auth (per-node token)
                    ┌──────────▼──────────┐
                    │    NODE AGENT        │
                    │   (each AI server)   │
                    │                     │
                    │  Task Executor      │
                    │  Telemetry Pusher   │
                    │  Hardware Collector │
                    └──────────┬──────────┘
                               │
                    ┌──────────▼──────────┐
                    │   LOCAL RUNTIMES    │
                    │                     │
                    │  Docker / Podman    │
                    │  vLLM containers    │
                    │  Ollama             │
                    │  TGI                │
                    │  CPU services       │
                    └─────────────────────┘
```

---

## Agent Responsibilities

### What the agent DOES
- Collect hardware metrics (CPU, RAM, disk, GPU via nvidia-smi)
- Push telemetry and heartbeat to control plane
- Receive and execute tasks from control plane
- Start/stop/restart Docker containers
- Report runtime state back to control plane
- Expose Prometheus metrics on `:9092/metrics`

### What the agent NEVER does
- Select which GPU to use (placement engine decides)
- Choose which node to deploy to (scheduler decides)
- Make routing decisions (gateway does this)
- Access the PostgreSQL database directly
- Communicate with other agents

---

## Agent Lifecycle

### Sequence: Registration

```
Agent starts
    │
    ├── Read /var/lib/nexus-agent/node-id  ── exists? ──► use stored ID
    │                                         no?
    │
    ├── POST /agent/v1/register
    │   body: {hostname, ip, total_cpu, total_ram_mb, total_vram_mb,
    │           agent_version, capabilities, labels}
    │
    │   Control Plane:
    │     1. Find or create node row in nodes table
    │     2. Upsert node_capabilities row
    │     3. Issue JWT token (1 year TTL, stored as SHA-256 hash)
    │     4. Return {node_id, token}
    │
    ├── Save node_id → /var/lib/nexus-agent/node-id
    ├── Save token   → /var/lib/nexus-agent/token  (mode 0600)
    │
    └── Begin heartbeat + telemetry + task poll loops
```

### Sequence: Heartbeat

```
Every 15s:
    Agent ──POST /agent/v1/heartbeat──► Control Plane
           {agent_version, status}
                                       UPDATE nodes SET
                                         status='online',
                                         last_heartbeat_at=NOW()
                                       WHERE id=$node_id
```

### Sequence: Task Execution (e.g., DEPLOY_RUNTIME)

```
Control Plane (Scheduler):              Agent:
  1. Placement engine picks node/GPU
  2. Create agent_runtime row (state=pending)
  3. Enqueue DEPLOY_RUNTIME task
     agent_tasks INSERT (status=pending)
                                         4. Long-poll GET /agent/v1/tasks/pending
                                            ◄── [{id, task_type, payload}]
                                         5. POST /agent/v1/tasks/:id/claim
                                         6. POST /agent/v1/tasks/:id/running
                                         7. Execute: docker run ...
                                            (GPU assignment from payload — no choice)
                                         8. POST /agent/v1/tasks/:id/complete
                                            {container_id, runtime_state: "loading"}
                                         9. PUT /agent/v1/runtimes/:id
                                            {state: "loading", container_id: "abc123"}

  10. Gateway watcher detects healthy endpoint
  11. Update agent_runtimes state → active
  12. Registry adds endpoint to pool
  13. Traffic begins routing to this runtime
```

### Sequence: Long-Poll Task Queue

```
Agent                          Control Plane
  │                                │
  ├──GET /tasks/pending?wait=25──►│
  │                                │ (holds connection up to 25s)
  │                                │ checks agent_tasks every 1s
  │                                │
  │◄──── [] empty ─────────────────┤ (after 25s, no tasks)
  │                                │
  ├──GET /tasks/pending?wait=25──►│
  │                                │
  │  (operator dispatches task)    │ INSERT agent_tasks
  │                                │
  │◄── [{task}] ───────────────────┤ (returns immediately)
  │                                │
  ├── claim + execute ────────────►│
```

This achieves near-real-time task delivery (~1s latency) without WebSocket or gRPC streaming.

---

## Task Types

| Task | Payload fields | What agent does |
|---|---|---|
| `DEPLOY_RUNTIME` | runtime_id, backend, image, model_name, gpu_devices, cpuset_cpus, numa_node, bind_port, ... | Runs `docker run` with exact params from control plane |
| `STOP_RUNTIME` | runtime_id, container_id, drain_secs | `docker stop -t <secs> <container>` |
| `RESTART_RUNTIME` | runtime_id, container_id | `docker restart <container>` |
| `DELETE_RUNTIME` | runtime_id, container_id | `docker rm -f <container>` |
| `WARM_RUNTIME` | runtime_id, container_id | `docker start <container>` |
| `UNLOAD_RUNTIME` | runtime_id, container_id | `docker stop` (keeps weights on disk) |
| `PULL_MODEL` | hf_repo, backend, hf_token | `ollama pull` or notes for vLLM |
| `DELETE_MODEL` | hf_repo, backend | `ollama rm` |
| `COLLECT_INVENTORY` | — | Returns current inventory via heartbeat |
| `HEALTH_CHECK` | runtime_ids[] | `docker inspect` each container |

---

## Authentication

Every agent gets a unique JWT token issued on first registration:

```
POST /agent/v1/register  ← no auth (bootstrapping)
    Response: {node_id, token}

All other agent routes:
    Authorization: Bearer <token>

Token claims:
    {node_id, hostname, iss: "nexus-control-plane",
     exp: <now + 1 year>, jti: <uuid>}

Validation:
    1. Verify HMAC-SHA256 signature
    2. Check token not expired
    3. Look up SHA-256 hash in node_tokens table
    4. Verify revoked = FALSE
```

To revoke a node:
```bash
curl -X DELETE http://localhost:8081/admin/v1/nodes/NODE_ID/token
# sets node_tokens.revoked = TRUE → next request returns 401
```

---

## Agent Capabilities

On registration and each heartbeat the agent reports its capabilities:

```json
{
  "docker":    true,
  "vllm":      false,
  "ollama":    true,
  "tgi":       false,
  "whisper":   false,
  "tts":       false,
  "embedding": false,
  "gpu":       false,
  "gpu_count": 0
}
```

The scheduler uses `node_capabilities` when selecting a node for a task. For example, an `EMBEDDING` service requiring `has_embedding: true` will never be scheduled to a node that doesn't support it.

---

## Runtime State Machine

```
pending ──► pulling ──► starting ──► loading ──► warm ──► active
                                         │              │
                                         ▼              ▼
                                       failed          idle ──► stopping ──► stopped
                                                                                │
                                                                           unloaded ──► deleted
```

States reported by agent via `PUT /agent/v1/runtimes/:id`:
- Control plane writes the `state` to `agent_runtimes` table
- Gateway watcher reads `health_status` from `model_endpoints` (synced from runtime state)

---

## Database Schema

```sql
-- Agent authentication
node_tokens (id, node_id, token_hash, issued_at, expires_at, revoked)

-- Capabilities
node_capabilities (node_id, has_docker, has_vllm, has_ollama, has_tgi,
                   has_whisper, has_tts, has_embedding, has_gpu, gpu_count)

-- Task queue
agent_tasks (id, node_id, task_type, payload JSONB, status, priority,
             created_by, created_at, claimed_at, started_at, completed_at,
             timeout_at, result JSONB, error_msg, runtime_id, idempotency_key)

-- First-class runtime entity
agent_runtimes (id, node_id, endpoint_id, model_id, runtime_name, backend,
                container_id, state, gpu_ids JSONB, cpu_affinity, memory_limit,
                numa_node, bind_host, bind_port, health_status, last_health_at)
```

---

## Prometheus Metrics

All metrics are exported at `:9092/metrics` with `node_id` and `hostname` as const labels.

| Metric | Type | Description |
|---|---|---|
| `nexus_agent_up` | Gauge | 1 = agent running and connected |
| `nexus_agent_tasks_total` | Counter | task_type, status labels |
| `nexus_agent_task_duration_seconds` | Histogram | task execution time |
| `nexus_agent_runtime_count` | Gauge | backend, state labels |
| `nexus_agent_cpu_usage` | Gauge | CPU utilization % |
| `nexus_agent_memory_usage` | Gauge | RAM used MB |
| `nexus_agent_memory_total` | Gauge | RAM total MB |
| `nexus_agent_gpu_memory_used` | Gauge | device_index, device_name labels |
| `nexus_agent_gpu_memory_total` | Gauge | device_index, device_name labels |
| `nexus_agent_gpu_utilization` | Gauge | GPU compute % |
| `nexus_agent_gpu_temperature_celsius` | Gauge | GPU temp °C |
| `nexus_agent_gpu_power_draw_watts` | Gauge | GPU power W |

---

## API Reference

### Agent API (called by agents)

| Method | Path | Auth | Description |
|---|---|---|---|
| POST | `/agent/v1/register` | none | Register node, get JWT token |
| POST | `/agent/v1/heartbeat` | JWT | Update liveness + capabilities |
| GET | `/agent/v1/tasks/pending?wait=25` | JWT | Long-poll for pending tasks |
| POST | `/agent/v1/tasks/:id/claim` | JWT | Atomically claim a task |
| POST | `/agent/v1/tasks/:id/running` | JWT | Mark task as running |
| POST | `/agent/v1/tasks/:id/complete` | JWT | Report task success + result |
| POST | `/agent/v1/tasks/:id/fail` | JWT | Report task failure |
| POST | `/agent/v1/inventory` | JWT | Push hardware inventory |
| POST | `/agent/v1/telemetry` | JWT | Push metrics snapshot |
| PUT | `/agent/v1/runtimes/:id` | JWT | Update runtime state |
| GET | `/agent/v1/runtimes` | JWT | List this node's runtimes |

### Admin API (task management)

| Method | Path | Description |
|---|---|---|
| POST | `/admin/v1/nodes/:id/tasks` | Dispatch a task to a node |
| GET | `/admin/v1/nodes/:id/tasks` | List recent tasks for a node |
| GET | `/admin/v1/tasks/:id` | Get task detail |
| DELETE | `/admin/v1/tasks/:id` | Cancel a pending task |
| GET | `/admin/v1/nodes/:id/runtimes` | List runtimes on a node |

### Example: Dispatch DEPLOY_RUNTIME

```bash
curl -X POST http://localhost:8081/admin/v1/nodes/NODE_ID/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "task_type": "DEPLOY_RUNTIME",
    "priority":  80,
    "payload": {
      "runtime_id":      "uuid-...",
      "endpoint_id":     "uuid-...",
      "model_id":        "uuid-...",
      "runtime_name":    "nexus-qwen3-32b",
      "backend":         "vllm",
      "image":           "vllm/vllm-openai:latest",
      "model_name":      "Qwen/Qwen3-32B-Instruct",
      "served_as":       "qwen3-32b",
      "bind_host":       "localhost",
      "bind_port":       8010,
      "gpu_devices":     [0],
      "cpuset_cpus":     "0-95",
      "numa_node":       0,
      "memory_limit":    "120g",
      "tensor_parallel": 1,
      "gpu_memory_util": 0.90,
      "dtype":           "bfloat16",
      "hf_token":        "hf_..."
    }
  }'
```

---

## Deployment

### Single server (same machine as control plane)

```bash
make run-nodeagent
# or:
NEXUS_ADMIN_URL=http://localhost:8081 ./bin/nexus-nodeagent
```

### Remote AI server

```bash
# Copy binary to each server
scp bin/nexus-nodeagent ai-server-01:/usr/local/bin/

# Run on each server
ssh ai-server-01
NEXUS_ADMIN_URL=http://10.0.0.1:8081 nexus-nodeagent

# Or as systemd service (see docs/09-nodes.md)
```

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `NEXUS_ADMIN_URL` | `http://localhost:8081` | Control plane URL |
| `NEXUS_NODE_ID` | _(auto)_ | Skip registration, use this node ID |
| `NEXUS_AGENT_TOKEN` | _(auto)_ | Use this JWT token |
| `NEXUS_AGENT_INTERVAL` | `30s` | Telemetry push interval |
| `NEXUS_HEARTBEAT_INTERVAL` | `15s` | Heartbeat interval |
| `NEXUS_TASK_WORKERS` | `4` | Concurrent task executors |
| `NEXUS_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` |
| `NEXUS_AGENT_METRICS_ADDR` | `:9092` | Prometheus metrics address |

---

## Multi-Node Expansion

Current: 1 node. Future: 100+ nodes. **No code changes required.**

Adding a new node:
1. Copy `nexus-nodeagent` binary to the new server
2. Set `NEXUS_ADMIN_URL=http://<control-plane>:8081`
3. Run it — it auto-registers and appears in the Nodes page
4. The scheduler immediately considers it for new deployments

The control plane's placement engine scores all online nodes and picks the best one. The agent on that node receives the task and executes it. The entire cluster state lives in PostgreSQL and is visible from the single Admin UI.
