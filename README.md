# NexusLLM

**Enterprise Multi-Tenant LLM Management Platform**

> A self-hosted internal AI platform. Teams get OpenAI-compatible API access to shared GPU infrastructure with full policy enforcement, usage tracking, and model lifecycle management — no Kubernetes required.

---

## What NexusLLM Does

NexusLLM sits between your teams and your GPU servers:

```
Team A App  ─────────────────────────────────────────────────────────┐
Team B App  ──► POST /v1/chat/completions  Bearer nxs_...            │
Team C App  ─────────────────────────────────────────────────────────┘
                                │
                    ┌───────────▼────────────┐
                    │     nexus-gateway      │  :8080
                    │                        │
                    │  Auth                  │  API key / JWT
                    │  Alias Resolution      │  "gpt-4o" → "gemma-3-27b"
                    │  Gateway Policy        │  temp cap, tool restrictions
                    │  Prompt Policy         │  system prompt injection, PII
                    │  Rate Limit / Quota    │  per-team Redis counters
                    │  Endpoint Selection    │  round-robin / weighted / AP
                    │  Failover              │  3 attempts, circuit breaker
                    │  Usage Tracking        │  async → PostgreSQL
                    └───────────┬────────────┘
                                │
               ┌────────────────┼────────────────┐
               ▼                ▼                 ▼
        vllm-gemma       vllm-llama          vllm-qwen
        :8000            :8001               :8002
        GPU 0,1          GPU 2,3,4,5         GPU 6,7
```

Teams point their OpenAI SDK at `http://your-server:8080/v1` — zero code changes required.

---

## Private Checkpoint — Complete Implementation Status

### Phase 1 — Gateway & Policy ✅
| Component | Files |
|---|---|
| Viper config (`NEXUS_` env prefix) | `internal/config/` |
| API key (SHA-256) + JWT, Redis cache | `internal/auth/` |
| OpenAI-compatible request/response types | `internal/models/` |
| Infrastructure policy engine (rate limit, quota, ACL, concurrency) | `internal/policy/` |
| Auth + Prometheus metrics + request-ID middleware | `internal/middleware/` |
| Priority queue scheduler (Redis Streams, starvation prevention) | `internal/scheduler/` |
| Base schema + dev seed | `migrations/001, 002` |

### Phase 2 — Runtime Layer ✅
| Component | Files |
|---|---|
| `Backend` interface (Health/Models/Chat/Embeddings) | `internal/runtime/backend.go` |
| vLLM backend (health + Prometheus scrape + SSE relay) | `internal/runtime/vllm.go` |
| Ollama backend | `internal/runtime/ollama.go` |
| TGI (HuggingFace) backend | `internal/runtime/tgi.go` |
| OpenAI-compatible backend | `internal/runtime/openai_compat.go` |
| Backend factory (plugin registration) | `internal/runtime/factory.go` |
| Endpoint pool — round-robin / weighted / active-passive / least-conn | `internal/runtime/pool.go` |
| Runtime registry — DB-backed, in-process, Redis health cache | `internal/runtime/registry.go` |
| Runtime watcher — health checks, circuit breaker, Prometheus | `internal/runtime/watcher.go` |
| Runtime schema v2 (models/versions/endpoints/configs/health log) | `migrations/003, 004` |

### Phase 3 — Enterprise Platform ✅
| Component | Files |
|---|---|
| **Model Controller** — start/stop/restart/upgrade/rollback via Docker | `internal/controller/` |
| Docker driver (shells `docker run/stop/rm`) | `internal/controller/docker_driver.go` |
| Controller operations log in PostgreSQL | `migrations/005` |
| **GPU Inventory** — nodes, devices, VRAM, utilization, temperature | `internal/gpu/inventory.go` |
| GPU packing algorithm (FFD bin-packing, contiguous + non-contiguous) | `internal/gpu/packing.go` |
| **Dynamic Model Lifecycle** — state machine (registered→loading→active→idle→unloaded) | `internal/lifecycle/state_machine.go` |
| Idle timeout + LRU eviction (Redis last-used timestamp) | `internal/lifecycle/state_machine.go` |
| **Usage Tracking** — async Redis Stream → PostgreSQL pipeline | `internal/usage/tracker.go` |
| Hourly + daily aggregation, team spend query | `internal/usage/tracker.go` |
| Partitioned `usage_events` table | `migrations/005` |
| **Prompt Policy Engine** — org→team→model execution order | `internal/promptpolicy/engine.go` |
| System prompt injection (prepend/append/replace) | |
| PII detection stub, content filtering, deny/allow lists | |
| Temperature clamp, max_tokens override | |
| Output regex + keyword filters | |
| **Model Aliases (Virtual Models)** — team/org/global scope | `internal/alias/resolver.go` |
| Redis cache, DB fallback, identity passthrough | |
| **AI Gateway Policy Layer** — per org/team/api_key | `internal/gatewaypolicy/engine.go` |
| Max temperature, context/output caps, tool restrictions | |
| Stream allowed, function call allowed | |
| **Proxy rewrite** — full enterprise pipeline | `internal/proxy/handler.go` |
| Auth → Alias → Gateway Policy → Infra Policy → Prompt Policy → Backend | |
| Usage event emitted on every request | |
| Lifecycle activity recorded per request | |
| **Admin API** — complete REST management surface | `internal/admin/handlers/` |
| Org, Team, Policy, API Key CRUD | |
| Runtime model/endpoint/pool/drain/enable/disable | |
| Controller start/stop/restart/upgrade/rollback/logs | |
| GPU node/device registration, packing simulation | |
| Usage queries (team daily, org monthly spend) | |
| Alias CRUD + resolve endpoint | |
| Prompt policy CRUD | |
| **Migration 005** — GPU inventory, lifecycle states, usage events, prompt policies, aliases, gateway policies | `migrations/005` |
| **Migration 006** — controller columns (container_id, runtime_image) | `migrations/006` |
| **genkey tool** | `tools/genkey/main.go` |
| **Dev stack** — Docker Compose (no GPU) | `docker-compose.yml` |
| **GPU stack** — Docker Compose single-server (3 vLLM instances) | `docker-compose.single-gpu.yml` |
| **Makefile** — build/run/test/migrate/docker/dev-up targets | `Makefile` |

---

## Project Structure

```
nexusllm/
├── cmd/
│   ├── gateway/main.go          ← inference API :8080
│   ├── admin/main.go            ← management API :8081
│   └── scheduler/main.go        ← queue dispatcher
│
├── internal/
│   ├── alias/                   ← virtual model name resolver
│   ├── auth/                    ← API key (SHA-256) + JWT
│   ├── config/                  ← viper config (NEXUS_ env vars)
│   ├── controller/              ← model runtime lifecycle (Docker)
│   │   ├── driver.go            ← Driver interface
│   │   ├── docker_driver.go     ← Docker CLI driver
│   │   └── model_controller.go  ← start/stop/restart/upgrade/rollback
│   ├── gatewaypolicy/           ← AI gateway policy (temp, tools, models)
│   ├── gpu/
│   │   ├── inventory.go         ← GPU node/device/allocation management
│   │   └── packing.go           ← bin-packing placement algorithm
│   ├── lifecycle/               ← state machine + idle eviction
│   ├── middleware/              ← auth, Prometheus metrics, request ID
│   ├── models/                  ← OpenAI-compatible types
│   ├── policy/                  ← infra policy engine (Redis hot path)
│   ├── promptpolicy/            ← prompt injection, PII, content filter
│   ├── proxy/                   ← full enterprise request pipeline
│   ├── runtime/
│   │   ├── backend.go           ← Backend interface
│   │   ├── vllm.go              ← vLLM
│   │   ├── ollama.go            ← Ollama
│   │   ├── tgi.go               ← HuggingFace TGI
│   │   ├── openai_compat.go     ← Any OpenAI-compatible API
│   │   ├── factory.go           ← backend factory
│   │   ├── pool.go              ← endpoint pool routing
│   │   ├── registry.go          ← in-process registry + DB sync
│   │   └── watcher.go           ← health watcher + circuit breaker
│   ├── scheduler/               ← Redis Streams priority queue
│   └── usage/                   ← async usage tracking + billing
│       └── tracker.go
│
├── migrations/
│   ├── 001_initial.sql          ← orgs, teams, policies, api_keys
│   ├── 002_seed_data.sql        ← 3 teams, 3 models, dev API keys
│   ├── 003_runtime_layer.sql    ← models, versions, endpoints, configs
│   ├── 004_single_gpu_runtime_seed.sql
│   ├── 005_enterprise_platform.sql  ← GPU inventory, lifecycle, usage,
│   │                                   prompt policies, aliases, gateway policies
│   └── 006_controller_columns.sql   ← container_id, runtime_image
│
├── monitoring/
│   ├── prometheus.yml           ← dev stack scrape config
│   ├── prometheus.single-gpu.yml ← GPU stack scrape config (includes vLLM)
│   └── datasources/prometheus.yml ← Grafana auto-provisioning
│
├── tools/genkey/main.go         ← API key generator
├── Dockerfile.gateway
├── Dockerfile.admin
├── Dockerfile.scheduler
├── docker-compose.yml           ← dev stack (no GPU)
├── docker-compose.single-gpu.yml ← single GPU server (3 vLLM instances)
├── Makefile
└── .env.example
```

---

## Quick Start

### Option A — No GPU (dev / testing)

```bash
cp .env.example .env
make dev-up           # starts postgres + redis, runs all migrations
make run-gateway      # terminal 1: inference API on :8080
make run-admin        # terminal 2: admin API on :8081
make run-scheduler    # terminal 3: priority queue
```

### Option B — Real GPU server

```bash
export HF_TOKEN=hf_your_token_here
export NEXUS_JWT_SECRET=$(openssl rand -hex 32)
make dev-up-gpu       # starts everything including 3 vLLM instances
```

Wait ~3 minutes for models to load, then:
```bash
curl http://localhost:8080/readyz | jq   # shows live model list
```

---

## Complete API Reference

### Inference API  `http://localhost:8080`

```bash
# Drop-in OpenAI replacement — zero code changes needed
client = openai.OpenAI(base_url="http://localhost:8080/v1", api_key="nxs_...")

# Or with curl:
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer nxs_your_key" \
  -H "Content-Type: application/json" \
  -d '{"model":"gemma-3-27b","messages":[{"role":"user","content":"Hello"}]}'

curl http://localhost:8080/v1/models          # lists models allowed for your key
curl http://localhost:8080/v1/embeddings      # embeddings
curl http://localhost:8080/healthz            # liveness
curl http://localhost:8080/readyz             # readiness + live model list
```

### Admin API  `http://localhost:8081`

#### Organizations & Teams
```bash
POST   /admin/v1/orgs
GET    /admin/v1/orgs
POST   /admin/v1/orgs/:org_id/teams
GET    /admin/v1/orgs/:org_id/teams
GET    /admin/v1/teams/:id
PUT    /admin/v1/teams/:id/policy
POST   /admin/v1/teams/:id/models         # grant model access
DELETE /admin/v1/teams/:id/models/:model  # revoke model access
```

#### API Keys
```bash
POST   /admin/v1/teams/:id/api-keys   # returns raw key once
GET    /admin/v1/teams/:id/api-keys
DELETE /admin/v1/api-keys/:id
```

#### Model Registry & Runtime
```bash
POST   /admin/v1/models                          # register model
GET    /admin/v1/models                          # list with health summary
POST   /admin/v1/models/:id/endpoints            # add endpoint to pool
DELETE /admin/v1/models/:id/endpoints/:ep_id
POST   /admin/v1/models/:id/drain                # stop new traffic
POST   /admin/v1/models/:id/enable
POST   /admin/v1/models/:id/disable
PUT    /admin/v1/models/:id/runtime-config       # vLLM tuning
PUT    /admin/v1/models/:id/pool-strategy        # routing algorithm
GET    /admin/v1/models/:id/health               # live endpoint status
```

#### Model Controller (start/stop/upgrade)
```bash
POST   /admin/v1/models/:id/start?endpoint_id=...
POST   /admin/v1/models/:id/stop?endpoint_id=...
POST   /admin/v1/models/:id/restart?endpoint_id=...
POST   /admin/v1/models/:id/upgrade    {"image":"vllm/vllm-openai:v0.5.0"}
POST   /admin/v1/models/:id/rollback   {"previous_image":"vllm/vllm-openai:v0.4.3"}
GET    /admin/v1/models/:id/logs?endpoint_id=...
```

#### GPU Inventory
```bash
POST   /admin/v1/gpu/nodes                        # register GPU server
GET    /admin/v1/gpu/nodes
POST   /admin/v1/gpu/nodes/:node_id/devices       # register GPU device
GET    /admin/v1/gpu/nodes/:node_id/devices
POST   /admin/v1/gpu/pack                         # simulate placement
```

#### Usage & Billing
```bash
GET    /admin/v1/usage/teams/:team_id?from=2026-01-01&to=2026-01-31
GET    /admin/v1/usage/orgs/:org_id/monthly-spend
POST   /admin/v1/usage/aggregate                  # manual rollup trigger
```

#### Model Aliases (Virtual Models)
```bash
POST   /admin/v1/aliases           {"alias":"gpt-4o","model_id":"...","scope":"global"}
GET    /admin/v1/aliases?team_id=...&org_id=...
DELETE /admin/v1/aliases
GET    /admin/v1/aliases/resolve?alias=gpt-4o&team_id=...&org_id=...
```

#### Prompt Policies
```bash
POST   /admin/v1/prompt-policies
# {
#   "scope": "team",
#   "scope_id": "<team_id>",
#   "name": "safety-filter",
#   "system_prompt": "You are a helpful assistant. Do not discuss harmful topics.",
#   "system_prompt_mode": "prepend",
#   "enable_pii_detection": true,
#   "input_deny_list": ["jailbreak", "ignore previous"]
# }
```

---

## Enterprise Request Pipeline

Every request flows through this ordered pipeline:

```
1. Auth middleware
   └── API key (Redis cache → PostgreSQL) or JWT
   └── Extracts: OrgID, TeamID, Priority, Permissions

2. Alias Resolution
   └── "gpt-4o" → team alias → org alias → global alias → identity
   └── Redis cache (5 min TTL)

3. Gateway Policy  (per org/team/api_key)
   └── Model allowed/denied list
   └── Max temperature enforcement
   └── Tool/function-call restrictions
   └── Stream permission check
   └── Context/output token caps

4. Infrastructure Policy  (Redis hot path, <2ms)
   └── Model ACL check (Redis set)
   └── Rate limit (sliding window Lua script)
   └── Daily token quota (Redis counter)
   └── Concurrency limit (Redis INCR/DECR)
   └── GPU capacity flag (set by Runtime Watcher)

5. Prompt Policy  (org → team → model, priority-ordered)
   └── System prompt injection
   └── PII detection
   └── Input deny list / allow list
   └── Temperature and token clamping
   └── Tool name filtering

6. Registry resolution  (in-process, < 0.1ms)
   └── ResolveWithFailover(model, maxAttempts=3)
   └── Pool.Pick() → round-robin / weighted / active-passive / least-conn

7. Backend execution
   └── vLLM / Ollama / TGI / OpenAI-compat
   └── SSE stream relay (line-by-line, flushed)
   └── Automatic retry on next healthy endpoint

8. Post-request (async)
   └── RecordTokenUsage → Redis quota counter
   └── RecordActivity  → Redis lifecycle timestamp
   └── Usage event     → Redis Stream → PostgreSQL (async consumer)
   └── Prometheus metrics updated
```

---

## Policy System

### Infrastructure Policy (Redis, <2ms)

| Field | Default | Description |
|---|---|---|
| `rpm` | 100 | Requests per minute (sliding window) |
| `tpd` | 1,000,000 | Tokens per day |
| `max_concurrent` | 10 | Simultaneous in-flight requests |
| `max_context_tokens` | 8192 | Max input token estimate |

### Prompt Policy (PostgreSQL, cached)

Execution order: **Org → Team → Model** (sorted by `priority ASC` within each scope)

| Capability | Description |
|---|---|
| System prompt injection | prepend / append / replace |
| PII detection | blocks SSN, credit card, email patterns |
| Input deny list | blocks requests containing specific terms |
| Input allow list | restricts to approved content only |
| Temperature cap | clamps `temperature` to `max_temperature` |
| Token override | reduces `max_tokens` to policy limit |
| Tool name filtering | deny/allow specific function names |
| Output regex filter | redact or block matching output patterns |

### AI Gateway Policy (PostgreSQL, per scope)

Per-org / per-team / per-API-key:

| Capability | Description |
|---|---|
| Model whitelist/blacklist | restrict which models a scope can use |
| Max temperature | hard cap on temperature parameter |
| Max context tokens | hard cap on input length |
| Max output tokens | hard cap on completion length |
| Stream allowed | enable/disable streaming |
| Function call allowed | enable/disable tool use |
| Tool name restrictions | allowed/denied tool names |

---

## GPU Management

### Register a GPU server

```bash
# Register the server
curl -X POST http://localhost:8081/admin/v1/gpu/nodes \
  -H "Content-Type: application/json" \
  -d '{"name":"gpu-server-01","host":"192.168.1.10","driver_type":"docker"}'

# Register each GPU device
curl -X POST http://localhost:8081/admin/v1/gpu/nodes/<NODE_ID>/devices \
  -d '{"device_index":0,"name":"NVIDIA A100 80GB","vram_mb":81920}'
```

### Simulate GPU packing

```bash
curl -X POST http://localhost:8081/admin/v1/gpu/pack \
  -H "Content-Type: application/json" \
  -d '{
    "node_id": "<NODE_ID>",
    "models": [
      {"model_name":"gemma-3-27b",   "required_vram_mb":55000, "gpu_count":2},
      {"model_name":"llama-3.3-70b", "required_vram_mb":75000, "gpu_count":4},
      {"model_name":"qwen-3-32b",    "required_vram_mb":55000, "gpu_count":2}
    ]
  }'

# Response:
# {
#   "assignments": {
#     "gemma-3-27b":   [0, 1],
#     "llama-3.3-70b": [2, 3, 4, 5],
#     "qwen-3-32b":    [6, 7]
#   },
#   "unscheduled": [],
#   "explanation": "GPU Packing Plan:\n  gemma-3-27b  → GPU [0 1]\n ..."
# }
```

---

## Virtual Models (Aliases)

Map friendly names to real models. Teams use the alias, routing resolves transparently:

```bash
# Global alias: "gpt-4o" → gemma-3-27b (all teams)
curl -X POST http://localhost:8081/admin/v1/aliases \
  -d '{"alias":"gpt-4o","model_id":"<GEMMA_MODEL_ID>","scope":"global"}'

# Team-specific alias: "reasoning" → qwen-3-32b (only for Team C)
curl -X POST http://localhost:8081/admin/v1/aliases \
  -d '{"alias":"reasoning","model_id":"<QWEN_MODEL_ID>","scope":"team","scope_id":"<TEAM_C_ID>"}'
```

Then Team C can call:
```python
client.chat.completions.create(model="reasoning", messages=[...])
# NexusLLM resolves "reasoning" → "qwen-3-32b" transparently
```

---

## Model Lifecycle

Endpoints transition through states automatically:

```
registered → downloading → loading → warm → active ─────┐
                                              │           │
                                           idle ──────► unloading → unloaded
                                              │
                                           failed
```

- **warm** — loaded, ready to serve
- **active** — currently serving requests
- **idle** — no requests for `idle_timeout` (default 30 min)
- **unloaded** — container stopped, VRAM freed
- Idle eviction runs every 30s, frees GPU memory for other models

---

## Usage & Billing

All requests are tracked asynchronously (Redis Stream → PostgreSQL consumer):

```bash
# Team daily usage breakdown
GET /admin/v1/usage/teams/<team_id>?from=2026-06-01&to=2026-06-30

# Response includes per-model, per-day:
# request_count, error_count, prompt_tokens, completion_tokens, cost_usd

# Org monthly spend
GET /admin/v1/usage/orgs/<org_id>/monthly-spend
```

Cost is computed as: `(prompt_tokens / 1M) × $0.50 + (completion_tokens / 1M) × $1.50`
(configurable per-model in `usage.Tracker`).

---

## Prometheus Metrics

All exposed on `:9090/metrics`:

| Metric | Labels |
|---|---|
| `nexus_gateway_requests_total` | team, model, status |
| `nexus_gateway_request_duration_seconds` | team, model |
| `nexus_gateway_tokens_input_total` | team, model |
| `nexus_gateway_tokens_output_total` | team, model |
| `nexus_gateway_active_requests` | team, model |
| `nexus_gateway_rejected_requests_total` | team, reason |
| `nexus_gateway_time_to_first_token_seconds` | team, model |
| `nexus_scheduler_queue_depth` | priority |
| `nexus_runtime_endpoint_up` | model, endpoint_id, host |
| `nexus_runtime_endpoint_health_latency_ms` | model, endpoint_id |
| `nexus_runtime_health_checks_total` | model, endpoint_id, status |
| `nexus_runtime_endpoint_consecutive_failures` | model, endpoint_id |
| `nexus_runtime_endpoint_active_connections` | model, endpoint_id |
| `nexus_runtime_endpoint_gpu_cache_utilization` | model, endpoint_id |

---

## Redis Key Design

```
nexus:apikey:<sha256>               → JSON TeamClaims         TTL 5m
nexus:ratelimit:<team>:rpm          → sorted set (sliding window)
nexus:quota:<team>:daily:<date>     → integer token counter   TTL 48h
nexus:inflight:<team>               → integer inflight count  TTL 10m
nexus:pool:<model>:at_capacity      → "0" | "1"               TTL 30s
nexus:ep:<id>:health                → health status string    TTL 30s
nexus:ep:<id>:failures              → integer failure count   TTL 10m
nexus:ep:<id>:last_used             → unix timestamp          TTL 2×idle
nexus:team:<id>:models              → Redis set of model names
nexus:alias:<scope>:<scope_id>:<alias> → real model name      TTL 5m
nexus:usage:events                  → Redis Stream (usage pipeline)
nexus:lifecycle:events              → Redis pub/sub channel
```

---

## Configuration Reference

All via environment variables with `NEXUS_` prefix:

| Variable | Default | Description |
|---|---|---|
| `NEXUS_SERVER_PORT` | `8080` | Gateway port |
| `NEXUS_SERVER_METRICSPORT` | `9090` | Prometheus metrics port |
| `NEXUS_SERVER_MODE` | `release` | `debug` or `release` |
| `NEXUS_DATABASE_DSN` | — | PostgreSQL connection string |
| `NEXUS_REDIS_ADDR` | `localhost:6379` | Redis address |
| `NEXUS_REDIS_PASSWORD` | — | Redis password |
| `NEXUS_AUTH_JWTSECRET` | — | JWT signing secret (use 32+ random bytes) |
| `NEXUS_AUTH_APIKEYCACHETTL` | `5m` | API key cache TTL |
| `NEXUS_VLLM_POLLINTERVAL` | `5s` | Runtime watcher poll frequency |
| `NEXUS_SCHEDULER_QUEUEHIGHSTREAM` | `nexus:queue:high` | |

---

## Make Targets

```bash
make build              # compile all 3 binaries → bin/
make run-gateway        # run gateway locally
make run-admin          # run admin API locally
make run-scheduler      # run scheduler locally
make test               # go test ./... -race
make lint               # golangci-lint
make docker-build       # build Docker images
make docker-push        # push to registry
make migrate            # run all migrations (local DB)
make dev-up             # start postgres+redis + migrate (no GPU)
make dev-up-gpu         # start full GPU stack (requires HF_TOKEN)
make dev-down           # stop all containers
make generate-key       # generate a new API key + print hash
make clean              # remove binaries
```

---

## Roadmap

The platform is production-ready for a single GPU server. Upcoming phases are documented in **[ROADMAP.md](./ROADMAP.md)**:

| Phase | Capability | Est. Effort |
|---|---|---|
| Phase 4 | RBAC + SSO/OIDC (Keycloak, Azure AD, Google) | 2 weeks |
| Phase 5 | Audit logging — immutable trail, before/after state | 3–4 days |
| Phase 6 | Web Admin UI (Next.js, shadcn/ui) | 2 weeks |
| Phase 7 | Budget & cost management (hard/soft limits, email alerts) | 3–4 days |
| Phase 8 | ClickHouse analytics layer (dual-write, Grafana dashboards) | 1 week |
| Phase 9 | MCP / Tool Gateway (GitHub, Jira, internal APIs) | 1.5 weeks |

---

## Production Checklist

- [ ] `NEXUS_AUTH_JWTSECRET` — use `openssl rand -hex 32`, never the default
- [ ] PostgreSQL — enable SSL (`sslmode=require`), set up daily backups
- [ ] Redis — enable AUTH + TLS, use Redis Sentinel for HA
- [ ] Set resource limits on all containers (CPU + memory)
- [ ] Configure Prometheus alerts: endpoint down, high error rate, quota exhausted
- [ ] Set up Grafana dashboards for per-team token usage and cost
- [ ] Restrict Admin API to internal network only (firewall / VPN)
- [ ] Rotate API keys regularly; revoke on team member departure
- [ ] Load test before first production traffic (`k6` or `locust`)
- [ ] Set `idle_timeout` per-model to free VRAM when models are unused
- [ ] Monitor `nexus_runtime_endpoint_gpu_cache_utilization` — alert at >90%
