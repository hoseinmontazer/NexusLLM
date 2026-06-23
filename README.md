# NexusLLM

**Enterprise AI Resource Orchestrator & Multi-Tenant Platform**

A self-hosted AI platform that orchestrates LLMs, embeddings, rerankers, speech services, OCR, and agent runtimes on bare-metal GPU servers. Teams and projects get a unified OpenAI-compatible API with full policy enforcement, resource-aware placement, priority scheduling, preemption, usage tracking, and model lifecycle management.

**Cluster-ready:** adding nodes requires only running the node agent on a new machine — no code changes needed.

---

## What's Built

| Category | Features |
|---|---|
| **Service Types** | CHAT, EMBEDDING, RERANK, STT, TTS, OCR, AGENT, MCP |
| **Runtime Types** | GPU_RUNTIME (vLLM, Ollama, TGI, llama.cpp), CPU_RUNTIME |
| **Hierarchy** | Organization → Team → **Project** → Models → Runtimes |
| **Project Priority** | CRITICAL / HIGH / NORMAL / LOW / BEST_EFFORT with preemption |
| **Resource Reservations** | Per-project VRAM / CPU / memory guarantees |
| **Preemption Engine** | Auto-evicts lower-priority runtimes under GPU pressure |
| **Placement Engine** | Auto GPU/CPU selection by VRAM, utilization, temperature, NUMA |
| **Cluster Nodes** | Auto-registered by node agent (CPU, RAM, GPU via nvidia-smi) |
| **Lazy-Load Runtimes** | Models start on first request, stop after idle timeout |
| **Policy Engine** | RPM, TPD, max_concurrent, max_context — all Redis, zero DB on hot path |
| **Live Policy Updates** | Policy changes apply instantly without gateway restart |
| **Model Aliases** | Virtual names scoped to org/team/global |
| **Prompt Policies** | System prompt injection, PII detection, content filtering |
| **Usage Tracking** | Per-team/project token usage, hourly/daily rollups, cost estimation |
| **Admin API** | Full CRUD for orgs, teams, projects, models, nodes, policies |
| **Web Admin UI** | Full management UI — projects, nodes (live GPU data), teams |

---

## Documentation

Full documentation is in the [`docs/`](docs/README.md) folder:

| Page | Description |
|---|---|
| [What is NexusLLM?](docs/01-what-is-nexusllm.md) | Platform overview, service types, hardware targets |
| [Quick Start](docs/02-quick-start.md) | Get running in 5 minutes |
| [Architecture](docs/03-architecture.md) | Request flow, design decisions, schema overview |
| [Organizations & Teams](docs/04-orgs-and-teams.md) | Multi-tenant setup, policies, model permissions |
| [API Keys & Auth](docs/05-api-keys-and-auth.md) | Create keys, SDK usage, security |
| [Model Registry](docs/06-models.md) | Import Ollama, deploy vLLM, lazy-load llama.cpp |
| [AI Service Registry](docs/07-ai-services.md) | Embeddings, STT, TTS, OCR, rerankers, MCP |
| [Placement Engine](docs/08-placement.md) | Auto GPU/CPU placement, simulation, NUMA |
| [Cluster Nodes](docs/09-nodes.md) | Node agent, auto GPU registration, telemetry |
| [GPU Inventory](docs/10-gpu-inventory.md) | GPU devices auto-populated by node agent |
| [Gateway API](docs/11-gateway-api.md) | Full inference API reference with examples |
| [Policies](docs/12-policies.md) | RPM, TPD (tokens/day), quotas, priority queuing |
| [Model Aliases](docs/13-aliases.md) | Virtual model names, OpenAI compatibility layer |
| [Prompt Policies](docs/14-prompt-policies.md) | System prompt injection, PII, content filtering |
| [Usage & Billing](docs/15-usage.md) | Token tracking, cost estimation, per-project reporting |
| [Web Admin UI](docs/16-web-ui.md) | All UI pages explained |
| [Configuration](docs/17-configuration.md) | All environment variables, Prometheus metrics |
| [Troubleshooting](docs/18-troubleshooting.md) | Common errors and fixes |
| [Node Agent Architecture](docs/19-node-agent-architecture.md) | Task system, auth, sequence diagrams |

---

## Quick Start

### Option A — Local dev with Ollama (no GPU needed)

```bash
# 1. Pull a model into Ollama
ollama pull gemma2:2b

# 2. Start postgres + redis + run all migrations
make dev-up

# 3. Run services (3 separate terminals)
make run-gateway     # inference API  → http://localhost:8080
make run-admin       # management API → http://localhost:8081

# 4. Start the web admin UI
make web-install     # first time only
make run-web         # → http://localhost:3001

# 5. Create org, team, project, and API key (web UI or curl)
ADMIN=http://localhost:8081/admin/v1
ORG_ID=$(curl -s -X POST $ADMIN/orgs \
  -H 'Content-Type: application/json' \
  -d '{"name":"My Org","slug":"my-org"}' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

TEAM_ID=$(curl -s -X POST $ADMIN/teams \
  -H 'Content-Type: application/json' \
  -d "{\"org_id\":\"$ORG_ID\",\"name\":\"My Team\",\"slug\":\"my-team\",\"priority\":80}" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

# 6. Import Ollama models
curl -X POST $ADMIN/models/import-ollama -H 'Content-Type: application/json' \
  -d '{"host":"localhost","port":11434}'

# 7. Grant model access to the team
curl -X POST $ADMIN/teams/$TEAM_ID/models \
  -H 'Content-Type: application/json' -d '{"model_name":"gemma2:2b"}'

# 8. Create an API key
API_KEY=$(curl -s -X POST $ADMIN/teams/$TEAM_ID/api-keys \
  -H 'Content-Type: application/json' -d '{"name":"dev-key"}' \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['key'])")

# 9. Make your first inference request
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gemma2:2b","messages":[{"role":"user","content":"Hello!"}]}'
```

### Option B — GPU server with vLLM

```bash
make dev-up && make run-gateway && make run-admin

# Deploy a vLLM model (requires NVIDIA GPU + Docker)
curl -X POST http://localhost:8081/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name":         "llama3-8b",
    "display_name": "LLaMA 3 8B",
    "backend_type": "vllm",
    "image":        "vllm/vllm-openai:v0.4.3",
    "hf_model_id":  "meta-llama/Meta-Llama-3-8B-Instruct",
    "host":         "localhost",
    "port":         8000,
    "auto_place":   true,
    "min_vram_mb":  16384,
    "hf_token":     "hf_...",
    "start_now":    true
  }'
```

---

## Architecture

```
Teams / Clients  (OpenAI SDK — zero code changes)
       │  Bearer nxs_...
       ▼
┌──────────────────────────────────────────────────────┐
│                 nexus-gateway :8080                   │
│  Auth → Alias → GW Policy → Infra Policy (Redis)     │
│  → Prompt Policy → Registry → Activator → Backend    │
│  → Project Context → Usage Tracker                   │
└──────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────┐
│                 nexus-admin :8081                     │
│  Orgs · Teams · Projects · Models · Nodes · Placement│
│  Preemption Engine · Node Health Monitor             │
└──────────────────────────────────────────────────────┘
       ↑
  Web UI :3001  (Next.js)

Node Agent ─────────────────→ auto-registers hardware
  (nvidia-smi, /proc, df)      updates GPU devices dynamically

PostgreSQL :5432   Redis :6379   Prometheus :9090/:9091
```

---

## Policy Reference

**TPD = Tokens Per Day** — the total LLM token budget (input + output) per team per UTC day.

| Field | Type | Description |
|---|---|---|
| `rpm` | int | Max requests per minute (sliding window) |
| `tpd` | int | Max tokens per day (input + output combined). `0` = unlimited |
| `max_concurrent` | int | Max simultaneous in-flight requests |
| `max_context_tokens` | int | Max prompt tokens per request |

```bash
# Set limits (takes effect immediately — no gateway restart needed)
curl -X PUT http://localhost:8081/admin/v1/teams/TEAM_ID/policy \
  -H 'Content-Type: application/json' \
  -d '{"rpm":100,"tpd":500000,"max_concurrent":10,"max_context_tokens":8192}'
```

See [docs/12-policies.md](docs/12-policies.md) for full details on how each limit works.

---

## Project Priority & Preemption

Projects are first-class entities between Teams and Models. Each project has a priority tier:

```
CRITICAL (100) → HIGH (75) → NORMAL (50) → LOW (25) → BEST_EFFORT (10)
```

When GPU resources are under pressure:
- The Preemption Engine automatically stops lower-priority runtimes
- CRITICAL projects always get resources before HIGH, HIGH before NORMAL, etc.
- Protected runtimes (`always_running: true`) are never evicted

```bash
# Create a project with CRITICAL priority and 80GB VRAM reservation
curl -X POST http://localhost:8081/admin/v1/projects \
  -H 'Content-Type: application/json' \
  -d '{"organization_id":"ORG_ID","team_id":"TEAM_ID",
       "name":"Fraud Detection","priority":"CRITICAL"}'

curl -X POST http://localhost:8081/admin/v1/projects/PROJECT_ID/reserve \
  -H 'Content-Type: application/json' \
  -d '{"reserved_vram_mb":81920}'
```

---

## Cluster Node Management

Nodes register themselves automatically — just run the node agent:

```bash
# On any machine with Docker (and optionally NVIDIA GPU)
NEXUS_ADMIN_URL=http://<control-plane>:8081 make run-nodeagent
```

The agent:
- Auto-registers the machine using its real hostname
- Collects CPU, RAM, disk telemetry every 30 seconds
- Runs `nvidia-smi` to discover and update GPU devices dynamically
- Updates `total_vram_mb` on the node row as GPUs are found

No manual GPU registration needed — GPU inventory is populated live.

To delete a node from the Admin UI: **Nodes → Details → Delete**  
(refused if the node has active runtimes — drain first)

---

## Admin API Quick Reference

**Base URL:** `http://localhost:8081/admin/v1`

```
# Organizations
POST   /orgs                          Create org
GET    /orgs                          List orgs
DELETE /orgs/:id                      Hard delete org + all teams

# Teams
POST   /teams                         {org_id, name, slug, priority}
GET    /teams[?org_id=]
PUT    /teams/:id                     Update name/slug/priority
DELETE /teams/:id                     Hard delete team
PUT    /teams/:id/policy              {rpm, tpd, max_concurrent, max_context_tokens}
GET    /teams/:id/policy
POST   /teams/:id/models              Grant model access
POST   /teams/:id/api-keys            Create API key (shown once)

# Projects
POST   /projects                      {organization_id, team_id, name, priority}
GET    /projects[?team_id=&priority=]
GET    /projects/:id
PUT    /projects/:id                  Update name/description/priority/status
DELETE /projects/:id
POST   /projects/:id/priority         Change priority (audited)
POST   /projects/:id/reserve          {reserved_vram_mb, reserved_cpu_cores, reserved_memory_mb}
PUT    /projects/:id/protection       {always_running, protected, minimum_replicas, admission_policy}
GET    /projects/:id/runtimes
GET    /projects/:id/usage[?from=&to=&breakdown=model]
GET    /projects/:id/preemptions
GET    /projects/:id/queue

# Models
POST   /models/deploy                 Full deploy with optional auto_place
POST   /models/import-ollama          Bulk import from running Ollama
POST   /models                        Register external model
GET    /models
DELETE /models/:id
POST   /models/:id/enable|disable|drain|archive|restore
GET    /models/:id/runtime-status     Container state per node

# Cluster Nodes
POST   /nodes                         Register manually
GET    /nodes                         List all nodes
GET    /nodes/:id                     Node + latest telemetry
GET    /nodes/:id/gpus                Live GPU data (updated by node agent)
POST   /nodes/:id/drain               Stop new deploys, finish existing
DELETE /nodes/:id                     Hard delete (fails if active runtimes)
GET    /nodes/:id/health-events
GET    /nodes/:id/telemetry

# Usage
GET    /usage/teams/:id?from=&to=
GET    /usage/orgs/:id/monthly-spend
```

---

## Web Admin UI

`http://localhost:3001`

| Page | URL | Description |
|---|---|---|
| Dashboard | `/` | Platform overview |
| Organizations | `/orgs` | Create, delete orgs |
| Teams | `/teams` | Edit teams, set rate limits, manage API keys |
| **Projects** | `/projects` | Create/manage projects, set priority, reserve VRAM |
| **Project Detail** | `/projects/:id` | Usage, preemption history, runtime list |
| API Keys | `/api-keys` | Create (shown once), revoke |
| Models | `/models` | Import Ollama, deploy vLLM, lazy-load config |
| Cluster Nodes | `/nodes` | Live CPU/RAM bars, **live GPU data** (auto-populated) |
| Placement | `/placement` | Resource placement simulator |
| GPU Inventory | `/gpu` | Manual GPU registration (supplement to auto-discovery) |
| Usage | `/usage` | Daily token usage per team |

---

## Make Targets

```bash
make build              # compile all binaries → bin/
make run-gateway        # inference API :8080
make run-admin          # management API :8081
make run-web            # web UI :3001
make web-install        # npm install (first time only)
make test               # go test ./... -race
make migrate            # run all migrations (001–011)
make dev-up             # start postgres+redis + migrate
make dev-down           # stop all containers

# Project management shortcuts
make project-list
make project-create ORG_ID=... TEAM_ID=... NAME="My Project" PRIORITY=CRITICAL
make project-priority ID=... PRIORITY=HIGH
make project-reserve ID=... VRAM_MB=81920
make project-preemptions ID=...

# AI Platform shortcuts
make placement-simulate MODEL=llama3-8b VRAM=16384 GPUS=1
make node-status
```

---

## Redis Key Reference

```
nexus:apikey:<sha256>                   → TeamClaims JSON          TTL 5m
nexus:ratelimit:<team>:rpm              → sorted set (sliding window)
nexus:quota:<team>:daily:<YYYY-MM-DD>   → token counter            TTL 48h
nexus:policy:<team>                     → hash {rpm,tpd,max_concurrent,max_context_tokens}
nexus:inflight:<team>                   → active request count     TTL 10m
nexus:pool:<model>:at_capacity          → "0"|"1"                  TTL 30s
nexus:team:<id>:models                  → Set of allowed model names
nexus:alias:<scope>:<id>:<name>         → real model name          TTL 5m
nexus:usage:events                      → Redis Stream (async usage pipeline)
nexus:queue:high / med / low            → Redis Streams (scheduler)
```

---

## Database Migrations

All migrations are idempotent (safe to re-run):

| File | What it creates |
|---|---|
| `001_initial.sql` | orgs, teams, policies, api_keys, models, audit_logs |
| `002_seed_data.sql` | *(empty — no hardcoded data)* |
| `003_runtime_layer.sql` | model_endpoints, model_versions, runtime_configs |
| `004_single_gpu_runtime_seed.sql` | *(empty — no hardcoded data)* |
| `005_ai_platform.sql` | nodes, node_telemetry, gpu_telemetry, placement_decisions |
| `006_h200_platform_seed.sql` | *(empty — no hardcoded data)* |
| `007_agent_tasks.sql` | agent_tasks, agent_runtimes, node_capabilities, node_tokens |
| `008_node_model_cache.sql` | node_model_cache |
| `009_resilience.sql` | runtime_requirements, node_health_events, model_lifecycle |
| `010_lazy_runtime.sql` | lazy-load config columns, last_used_at |
| `011_projects.sql` | projects, project_reservations, project_configurations, preemption_events, deployment_queue |

---

## Production Checklist

- [ ] `NEXUS_AUTH_JWTSECRET` → `$(openssl rand -hex 32)` — never use the default
- [ ] PostgreSQL: `sslmode=require` + daily backups
- [ ] Redis: AUTH password + TLS
- [ ] Firewall: ports 8081 (admin) and 3001 (web) internal-only
- [ ] `NEXUS_SERVER_MODE=release` in production
- [ ] Set `rpm` and `tpd` limits on every team before going live
- [ ] Create CRITICAL projects for production workloads with VRAM reservations
- [ ] Run `make test` before every deployment
- [ ] Prometheus alert: `nexus_project_active_runtimes` drops to 0 for protected projects
