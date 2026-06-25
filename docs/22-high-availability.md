# High Availability & Self-Healing

NexusLLM automatically survives node failures without manual intervention. When a node goes offline, the platform detects it, marks affected runtimes as LOST, and autonomously redeploys them on healthy nodes.

---

## Correct Relationship: Model → Runtime(s) → Endpoint

```
Model
  └── Runtime replica-0  (node-1, 10.0.0.11:8100)  → Endpoint
  └── Runtime replica-1  (node-2, 10.0.0.12:8100)  → Endpoint
  └── Runtime replica-2  (node-1, 10.0.0.11:8101)  → Endpoint
```

- One model can have **N runtime replicas**, each on its own node/port.
- Each runtime owns exactly one endpoint (its `bind_host:bind_port`).
- The gateway maintains a **Pool per model** containing all healthy replicas.
- Traffic is routed round-robin across all healthy endpoints in the pool.

## Port Allocation

Each replica gets a unique port via `allocate_node_port()` — a PostgreSQL
advisory-locked function that scans `node_port_leases` and returns the next
free port in the range `[8100, 8999]`. Ports are released automatically when
a runtime transitions to a terminal state via a DB trigger.

```sql
-- Allocate a port on a node
SELECT allocate_node_port('node-id', 'model-id');  -- returns e.g. 8102

-- Release when runtime stops
SELECT release_node_port('node-id', 8102);
-- (also fires automatically via trigger on terminal state transitions)
```

## How the Reconciler Builds a Task

The reconciler pre-creates the full `agent_runtimes` row and sends a
completely populated `StartModelPayload` — identical to what the admin
deploy handler sends. Required fields:

- `runtime_name` — unique per replica: `nexus-<model>-r<idx>-<short-id>`
- `image` — loaded from `model_endpoints.runtime_image`
- `bind_port` — allocated uniquely via `allocate_node_port()`
- `bind_host` — node IP from `nodes.ip_address / hostname`
- `backend`, `gguf_path`, `hf_repo`, etc. — from `model_runtime_configs`


                │
                │  (no heartbeat for 90s)
                ▼
Node Health Monitor → node status = UNHEALTHY
                │
                │  (no heartbeat for 5 min)
                ▼
              OFFLINE → all runtimes → LOST
              endpoints → health_status = 'down'
              gateway stops routing
                │
                ▼
HA Reconciler (runs every 30s)
  → compares desired_replicas vs actual
  → selects best healthy node
  → enqueues START_MODEL task
  → logs to runtime_recovery_log
                │
                ▼
Node Agent executes START_MODEL pipeline
  pending → validating → downloading →
  starting → loading_model → ready
                │
                ▼
Gateway Registry reloads → routes requests again
```

**No administrator action required at any step.**

---

## Replica Specs

Every model has a `model_replica_specs` row defining its desired state:

| Field | Default | Description |
|---|---|---|
| `desired_replicas` | 1 | Target number of running replicas |
| `min_available` | 1 | SLA floor — below this = degraded |
| `placement_policy` | `spread` | How to distribute across nodes |
| `auto_recover` | `true` | Automatically recover lost replicas |
| `recovery_delay_s` | 30 | Seconds to wait before recovering |
| `max_surge` | 1 | Extra replicas allowed during failover |

---

## Placement Policies

| Policy | Behaviour | Best for |
|---|---|---|
| `spread` | Prefer different nodes per replica | Production HA — resilience against single node failure |
| `pack` | Prefer same node | Development — minimise resource usage |
| `anti_affinity` | Hard rule: never two replicas on same node | Critical workloads requiring strict isolation |

---

## HA Status Values

| Status | Meaning |
|---|---|
| `healthy` | `active + idle >= min_available` |
| `degraded` | At least 1 replica running but below `min_available` |
| `unavailable` | Zero replicas running |

---

## Runtime States (HA-aware)

| State | Description |
|---|---|
| `active` / `warm` / `ready` | Serving requests — counted toward `active_replicas` |
| `loading_model` / `starting` / `recovering` | In pipeline — counted toward `starting_replicas` |
| `idle` | Loaded, no recent traffic — still available |
| `lost` | Node went offline — recovery pending |
| `stopped` | Cleanly stopped — not counted |

---

## API Reference

### Set replica spec for a model

```bash
curl -X PUT http://localhost:8081/admin/v1/ha/models/MODEL_ID/replicas \
  -H 'Content-Type: application/json' \
  -d '{
    "desired_replicas": 2,
    "min_available": 1,
    "placement_policy": "spread",
    "auto_recover": true,
    "recovery_delay_s": 30
  }'
```

### Get cluster-wide HA status

```bash
curl http://localhost:8081/admin/v1/ha/status
```

Response:
```json
{
  "total": 5,
  "healthy": 4,
  "degraded": 1,
  "unavailable": 0,
  "reconciler_last_sweep": "2026-06-25T12:00:00Z",
  "recoveries_triggered": 3,
  "models": [
    {
      "model_id": "...",
      "model_name": "gemma-2-9b",
      "desired_replicas": 2,
      "min_available": 1,
      "active_replicas": 1,
      "starting_replicas": 1,
      "lost_replicas": 1,
      "node_count": 2,
      "ha_status": "degraded"
    }
  ]
}
```

### Get per-model HA status

```bash
curl http://localhost:8081/admin/v1/ha/status/MODEL_ID
```

### Get recovery log

```bash
curl http://localhost:8081/admin/v1/ha/recovery-log?limit=50
curl http://localhost:8081/admin/v1/ha/recovery-log/MODEL_ID
```

---

## Configuration Example

### 2 replicas, spread across nodes (recommended for production)

```bash
curl -X PUT http://localhost:8081/admin/v1/ha/models/GEMMA_ID/replicas \
  -d '{"desired_replicas":2,"min_available":1,"placement_policy":"spread"}'
```

### 3 replicas with anti-affinity (maximum isolation)

```bash
curl -X PUT http://localhost:8081/admin/v1/ha/models/MODEL_ID/replicas \
  -d '{"desired_replicas":3,"min_available":2,"placement_policy":"anti_affinity"}'
```

### Disable auto-recovery (manual control)

```bash
curl -X PUT http://localhost:8081/admin/v1/ha/models/MODEL_ID/replicas \
  -d '{"auto_recover":false}'
```

---

## Database Tables

| Table | Purpose |
|---|---|
| `model_replica_specs` | Desired state per model |
| `runtime_replica_status` | View: live desired vs actual |
| `runtime_recovery_log` | Audit trail for every recovery action |
| `reconciler_state` | Last sweep timestamp and counters |
| `node_health_events` | Node state transition history |

---

## Prometheus Metrics (planned)

| Metric | Description |
|---|---|
| `nexus_model_desired_replicas` | Configured desired replicas per model |
| `nexus_model_active_replicas` | Currently active replicas per model |
| `nexus_model_lost_replicas` | Replicas in LOST state |
| `nexus_recovery_total` | Total recovery events triggered |
| `nexus_node_status` | Per-node status (online/offline/degraded) |

---

## Web UI

Navigate to **High Avail.** in the sidebar to see:

- Cluster-wide summary (healthy / degraded / unavailable counts)
- Per-model replica status table with visual replica count bars
- Node distribution count per model
- Last reconciler sweep time
- Recovery log with trigger, status, and reason for each event
- **Edit** button per model to configure `desired_replicas`, `min_available`, placement policy, and auto-recovery settings

---

*Related: [Automatic Scheduler](20-automatic-scheduler.md) | [Node Agent Architecture](19-node-agent-architecture.md) | [Cluster Nodes](09-nodes.md)*

## Gateway Routing with Multiple Replicas

The gateway registry maintains one `Pool` per model. Each pool contains all
healthy endpoints (replicas). Routing uses round-robin by default but supports
weighted, least-connections, and active/passive strategies.

```
Request: POST /v1/chat/completions {"model":"gemma-2"}
                │
                ▼
  Registry.ResolveWithFailover("gemma-2", 3)
                │
                ├─► Pool["gemma-2"]
                │     ├── Endpoint: 10.0.0.11:8100  (replica-0, healthy)
                │     ├── Endpoint: 10.0.0.12:8100  (replica-1, healthy)
                │     └── Endpoint: 10.0.0.11:8101  (replica-2, loading)
                │
                ▼ (round-robin, skip non-healthy)
           10.0.0.11:8100  ← forwards request
```

The registry is reloaded every **10 seconds** to pick up new replicas started
by the HA reconciler. Replicas in `ready/active/warm/idle` state are
automatically added to the pool; lost/failed/stopped replicas are removed.

## Database Tables Added

| Table | Purpose |
|---|---|
| `node_port_leases` | Per-node port lease registry — prevents port conflicts |
| `model_replica_specs` | Desired state per model (desired_replicas, min_available, placement_policy) |
| `runtime_replica_status` | View: live desired vs actual replica counts |
| `runtime_recovery_log` | Audit trail for every recovery action |
| `reconciler_state` | Last sweep timestamp and counters |

## Migrations Applied

| Migration | What it adds |
|---|---|
| `019_ha_replicas.sql` | Replica specs, recovery log, reconciler state, replica_index on agent_runtimes |
| `020_port_allocator.sql` | `node_port_leases` table, `allocate_node_port()`, `release_node_port()`, auto-release trigger |
