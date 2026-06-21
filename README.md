# NexusLLM

**Enterprise AI Resource Orchestrator & Multi-Tenant Platform**

A self-hosted AI platform that orchestrates LLMs, embeddings, rerankers, speech services, OCR, and agent runtimes on bare-metal GPU servers. Teams get a unified OpenAI-compatible API with full policy enforcement, resource-aware placement, usage tracking, and model lifecycle management.

**Hardware target:** Single AI Server — 2× NVIDIA H200 NVL (288 GB VRAM), 384 vCPUs, 1 TB RAM.
Cluster-ready: adding nodes requires only inserting rows into the `nodes` table.

---

## ✅ What's Built

| Category | Features |
|---|---|
| **Service Types** | CHAT, EMBEDDING, RERANK, STT, TTS, OCR, AGENT, MCP |
| **Runtime Types** | GPU_RUNTIME (vLLM, Ollama, TGI), CPU_RUNTIME (cpu_native) |
| **Placement Engine** | Auto GPU/CPU selection by VRAM, util, temp, NUMA locality |
| **Resource Reservations** | min/max VRAM, CPU cores, NUMA affinity, priority tiers |
| **Cluster Nodes** | `nodes` table + Node Agent (metrics, heartbeat, inventory) |
| **Multi-Service API** | `/v1/chat`, `/v1/embeddings`, `/v1/rerank`, `/v1/audio/*`, `/v1/ocr` |
| **Policy Engine** | Rate limit, quota, ACL, concurrency — all Redis, zero DB on hot path |
| **Model Aliases** | Virtual names scoped to org/team/global |
| **Prompt Policies** | System prompt injection, PII detection, content filtering |
| **Usage Tracking** | Per-team/org token usage, hourly/daily rollups, cost estimation |
| **Admin API** | Full CRUD for orgs, teams, models, services, nodes, policies |

---

## Quick Start

```bash
# 1. Start postgres + redis + run all migrations (001–006)
make dev-up

# 2. Run services (3 terminals)
make run-gateway     # inference API  → :8080
make run-admin       # management API → :8081
make run-scheduler   # queue dispatcher

# 3. Start the web admin UI (4th terminal)
make run-web         # → http://localhost:3001

# First time only — install Node dependencies
make web-install
```

---

## Project Structure

```
nexusllm/
├── cmd/
│   ├── gateway/       ← inference API :8080
│   ├── admin/         ← management API :8081
│   └── scheduler/     ← Redis Streams queue dispatcher
│
├── internal/
│   ├── alias/         ← virtual model name resolver
│   ├── auth/          ← API key (SHA-256) + JWT
│   ├── config/        ← viper config (NEXUS_ env vars)
│   ├── controller/    ← model runtime lifecycle (Docker)
│   ├── gatewaypolicy/ ← temperature/tool/model restrictions
│   ├── gpu/           ← GPU inventory + bin-packing
│   ├── lifecycle/     ← state machine + idle eviction
│   ├── middleware/    ← auth, Prometheus metrics, request ID
│   ├── models/        ← OpenAI-compatible types + multi-service types
│   ├── nodeagent/     ← hardware metrics collection + heartbeat
│   ├── placement/     ← resource-aware placement engine
│   ├── policy/        ← rate limit, quota (Redis hot path)
│   ├── promptpolicy/  ← system prompt injection, PII, filters
│   ├── proxy/         ← full enterprise request pipeline + multi-service
│   ├── runtime/       ← vLLM/Ollama/TGI/CPU-native backends + registry
│   ├── scheduler/     ← priority queue + GPU watcher
│   ├── services/      ← AI Service Registry (all service types)
│   └── usage/         ← async usage tracking + billing
│
├── migrations/
│   ├── 001_initial.sql              ← orgs, teams, policies
│   ├── 002_seed_data.sql            ← dev org + 3 teams
│   ├── 003_runtime_layer.sql        ← models, endpoints, configs
│   ├── 004_single_gpu_runtime_seed.sql
│   ├── 005_ai_platform.sql          ← nodes, service registry, reservations,
│   │                                   placement, CPU alloc, telemetry
│   └── 006_h200_platform_seed.sql   ← nexus-h200-01 node + 2x H200 GPU devices
│
├── web/                             ← Next.js 14 Admin UI
│   └── app/ ...
│
├── Dockerfile.{gateway,admin,scheduler}
├── docker-compose.yml
├── docker-compose.single-gpu.yml
├── Makefile
└── ROADMAP.md
```

---

## Architecture

```
Teams / Clients  (OpenAI SDK — zero code changes)
       │  Bearer nxs_...
       ▼
nexus-gateway :8080
  Auth → Alias Resolution → Gateway Policy → Infra Policy (Redis)
  → Prompt Policy → Endpoint Selection → Backend → Usage Tracking
       │
       ▼
vLLM / Ollama / TGI  (any OpenAI-compatible backend)

nexus-admin :8081  ←──  Web UI :3001
  Organizations, Teams, API Keys, Models, GPU, Usage, Policies, Aliases

PostgreSQL :5432    Redis :6379    Prometheus :9100    Grafana :3000
```

---

## How to Register a Model

### Already-running model (Ollama, TGI, existing vLLM)
```bash
curl -X POST http://localhost:8081/admin/v1/models \
  -H "Content-Type: application/json" \
  -d '{
    "name":         "llama3.2",
    "display_name": "LLaMA 3.2",
    "backend_type": "ollama",
    "host":         "localhost",
    "port":         11434
  }'
```

### Deploy vLLM via Docker (NexusLLM manages the container)
```bash
curl -X POST http://localhost:8081/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name":            "gemma-3-27b",
    "display_name":    "Gemma 3 27B",
    "backend_type":    "vllm",
    "image":           "vllm/vllm-openai:v0.4.3",
    "hf_model_id":     "google/gemma-3-27b-it",
    "host":            "localhost",
    "port":            8000,
    "gpu_devices":     [0, 1],
    "tensor_parallel": 2,
    "gpu_memory_util": 0.90,
    "hf_token":        "hf_...",
    "start_now":       true
  }'
```

Or use the **Web UI** → Models → Deploy vLLM Model.

---

## Web Admin UI Pages

| Page | URL | What you can do |
|---|---|---|
| Dashboard | `/` | Platform overview, model health status |
| Organizations | `/orgs` | Create/delete orgs |
| Teams | `/teams` | Create teams, edit policies (RPM/TPD/concurrency), manage API keys |
| API Keys | `/api-keys` | Create keys (shown once), revoke |
| Models | `/models` | Deploy vLLM, register external models, enable/disable, view health |
| GPU Inventory | `/gpu` | Register GPU servers and devices, view utilization |
| Usage | `/usage` | Daily token breakdown, cost per team/model |
| Settings | `/settings` | Service URLs, env var reference, quick start guide |

---

## API Quick Reference

### Inference API `:8080`
```
POST /v1/chat/completions        → LLM inference (stream or sync)
POST /v1/embeddings              → Text embeddings
POST /v1/rerank                  → Cross-encoder reranking
POST /v1/audio/transcriptions    → Speech-to-text (multipart/form-data)
POST /v1/audio/speech            → Text-to-speech
POST /v1/ocr                     → Optical character recognition
GET  /v1/models                  → Models allowed for your API key
GET  /healthz                    → Liveness
GET  /readyz                     → Readiness + live model list
```

### Admin API `:8081`
```
# Orgs & Teams
POST   /admin/v1/orgs
GET    /admin/v1/teams
POST   /admin/v1/teams              {"org_id":..., "name":..., "slug":..., "priority":5}
GET    /admin/v1/teams/:id/policy
PUT    /admin/v1/teams/:id/policy   {"rpm":500, "tpd":10000000}
POST   /admin/v1/teams/:id/models   {"model_name":"gemma-3-27b"}

# API Keys
POST   /admin/v1/teams/:id/api-keys {"name":"my-app"}
GET    /admin/v1/teams/:id/api-keys
DELETE /admin/v1/api-keys/:id

# LLM Models (GPU_RUNTIME)
POST   /admin/v1/models/deploy      ← register + auto-place + start vLLM
POST   /admin/v1/models             ← register external model
GET    /admin/v1/models
DELETE /admin/v1/models/:id
GET    /admin/v1/models/:id/health
GET    /admin/v1/models/:id/deploy-status
POST   /admin/v1/models/:id/enable
POST   /admin/v1/models/:id/disable
POST   /admin/v1/models/:id/start?endpoint_id=...
POST   /admin/v1/models/:id/stop?endpoint_id=...
POST   /admin/v1/models/:id/restart?endpoint_id=...
POST   /admin/v1/models/:id/upgrade {"image":"vllm/vllm-openai:v0.5.0"}
POST   /admin/v1/models/:id/rollback {"previous_image":"..."}
GET    /admin/v1/models/:id/logs?endpoint_id=...

# AI Service Registry (all service types incl. CPU_RUNTIME)
POST   /admin/v1/services/deploy    ← register + auto-place + deploy
POST   /admin/v1/services           ← register existing external service
GET    /admin/v1/services[?type=EMBEDDING]
GET    /admin/v1/services/:id/reservation
PUT    /admin/v1/services/:id/reservation  {"min_vram_mb":65536,"priority":"critical"}

# GPU Inventory
POST   /admin/v1/gpu/nodes
POST   /admin/v1/gpu/nodes/:id/devices
GET    /admin/v1/gpu/nodes
GET    /admin/v1/gpu/nodes/:id/devices
POST   /admin/v1/gpu/pack           ← packing simulation

# Cluster Nodes (multi-server ready)
POST   /admin/v1/nodes              {"hostname":"h200-02","total_cpu":384,...}
GET    /admin/v1/nodes
GET    /admin/v1/nodes/:id
PUT    /admin/v1/nodes/:id/labels
POST   /admin/v1/nodes/:id/heartbeat
POST   /admin/v1/nodes/:id/inventory  ← pushed by node agent
GET    /admin/v1/nodes/:id/telemetry
GET    /admin/v1/nodes/:id/inventory

# Placement Engine
POST   /admin/v1/placement/simulate  ← dry-run, no resources committed
GET    /admin/v1/placement/decisions ← audit log of all placement decisions

# Usage
GET    /admin/v1/usage/teams/:id?from=2026-01-01&to=2026-01-31
GET    /admin/v1/usage/orgs/:id/monthly-spend

# Aliases
POST   /admin/v1/aliases
GET    /admin/v1/aliases
GET    /admin/v1/aliases/resolve?alias=gpt-4o&team_id=...&org_id=...
```

### Deploy with Auto-Placement
```bash
# GPU model — let the engine pick the best GPU(s)
curl -X POST http://localhost:8081/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name":          "qwen3-32b",
    "display_name":  "Qwen3 32B",
    "backend_type":  "vllm",
    "image":         "vllm/vllm-openai:latest",
    "hf_model_id":   "Qwen/Qwen3-32B-Instruct",
    "host":          "localhost",
    "port":          8010,
    "auto_place":    true,
    "min_vram_mb":   65536,
    "max_vram_mb":   122880,
    "priority":      "critical",
    "gpu_count":     1,
    "start_now":     true
  }'

# CPU service — embedding model on CPU
curl -X POST http://localhost:8081/admin/v1/services/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name":         "bge-m3",
    "display_name": "BGE-M3 Embeddings",
    "service_type": "EMBEDDING",
    "runtime_type": "CPU_RUNTIME",
    "image":        "michaelf34/infinity:latest",
    "host":         "localhost",
    "port":         7997,
    "cpu_cores":    32,
    "numa_node":    0,
    "ram_mb":       65536,
    "priority":     "normal",
    "start_now":    true
  }'

# Simulate placement before committing
curl -X POST http://localhost:8081/admin/v1/placement/simulate \
  -H "Content-Type: application/json" \
  -d '{"model_name":"deepseek-coder","service_type":"CHAT","runtime_type":"GPU_RUNTIME","min_vram_mb":16384,"gpu_count":1}'
```

---

## All Make Targets

```bash
make build              # compile all 3 binaries → bin/
make run-gateway        # inference API :8080
make run-admin          # admin API :8081
make run-scheduler      # queue dispatcher
make run-web            # web UI :3001
make web-install        # npm install (first time only)
make test               # go test ./... -race
make docker-build       # build Docker images
make migrate            # run all migrations (DB must be running)
make dev-up             # start postgres+redis + migrate
make dev-down           # stop all containers
make generate-key       # generate a new API key
make clean              # remove binaries
```

---

## Redis Key Design

```
nexus:apikey:<sha256>               → TeamClaims JSON        TTL 5m
nexus:ratelimit:<team>:rpm          → sorted set (sliding window)
nexus:quota:<team>:daily:<date>     → token counter           TTL 48h
nexus:inflight:<team>               → active request count    TTL 10m
nexus:pool:<model>:at_capacity      → "0"|"1"                 TTL 30s
nexus:ep:<id>:health                → health status           TTL 30s
nexus:ep:<id>:failures              → circuit breaker count   TTL 10m
nexus:ep:<id>:last_used             → unix timestamp          TTL 60m
nexus:team:<id>:models              → set of allowed models
nexus:alias:<scope>:<id>:<alias>    → real model name         TTL 5m
nexus:usage:events                  → Redis Stream (pipeline)
nexus:lifecycle:events              → pub/sub channel
nexus:queue:high / med / low        → Redis Streams (scheduler)
```

---

## Production Checklist

- [ ] `NEXUS_AUTH_JWTSECRET` — set to `$(openssl rand -hex 32)`, never the default
- [ ] PostgreSQL SSL (`sslmode=require`) + daily backups
- [ ] Redis AUTH + TLS
- [ ] Restrict Admin API + Web UI to VPN/internal network only
- [ ] Set resource limits on all containers
- [ ] Configure Prometheus alerts: endpoint down, high error rate, quota exhausted
- [ ] Load test before first production traffic
- [ ] Set idle timeout per-model to free VRAM when unused
