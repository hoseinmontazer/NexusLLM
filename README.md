# NexusLLM

**Enterprise AI Resource Orchestrator & Multi-Tenant Platform**

A self-hosted AI platform that orchestrates LLMs, embeddings, rerankers, speech services, OCR, and agent runtimes on bare-metal GPU servers. Teams get a unified OpenAI-compatible API with full policy enforcement, resource-aware placement, usage tracking, and model lifecycle management.

**Hardware target:** Single AI Server — 2× NVIDIA H200 NVL (288 GB VRAM), 384 vCPUs, 1 TB RAM.  
**Cluster-ready:** adding nodes requires only inserting rows into the `nodes` table — no code changes.

---

## What's Built

| Category | Features |
|---|---|
| **Service Types** | CHAT, EMBEDDING, RERANK, STT, TTS, OCR, AGENT, MCP |
| **Runtime Types** | GPU_RUNTIME (vLLM, Ollama, TGI), CPU_RUNTIME (cpu_native) |
| **Placement Engine** | Auto GPU/CPU selection by VRAM, utilization, temperature, NUMA locality |
| **Resource Reservations** | min/max VRAM, CPU cores, NUMA affinity, priority tiers |
| **Cluster Nodes** | `nodes` table + Node Agent (metrics, heartbeat, inventory) |
| **Multi-Service API** | `/v1/chat`, `/v1/embeddings`, `/v1/rerank`, `/v1/audio/*`, `/v1/ocr` |
| **Policy Engine** | Rate limit, quota, ACL, concurrency — all Redis, zero DB on hot path |
| **Model Aliases** | Virtual names scoped to org/team/global |
| **Prompt Policies** | System prompt injection, PII detection, content filtering |
| **Usage Tracking** | Per-team/org token usage, hourly/daily rollups, cost estimation |
| **Admin API** | Full CRUD for orgs, teams, models, services, nodes, policies |

---

## Documentation

Full documentation is in the [`docs/`](docs/README.md) folder:

| Page | Description |
|---|---|
| [What is NexusLLM?](docs/01-what-is-nexusllm.md) | Platform overview, service types, hardware targets |
| [Quick Start](docs/02-quick-start.md) | Get running in 5 minutes — Ollama or vLLM |
| [Architecture](docs/03-architecture.md) | Request flow, design decisions, schema overview |
| [Organizations & Teams](docs/04-orgs-and-teams.md) | Multi-tenant setup, policies, model permissions |
| [API Keys & Auth](docs/05-api-keys-and-auth.md) | Create keys, SDK usage, security |
| [Model Registry](docs/06-models.md) | Import Ollama, deploy vLLM, lifecycle management |
| [AI Service Registry](docs/07-ai-services.md) | Embeddings, STT, TTS, OCR, rerankers, MCP |
| [Placement Engine](docs/08-placement.md) | Auto GPU/CPU placement, simulation, NUMA |
| [Cluster Nodes](docs/09-nodes.md) | Node agent, telemetry, multi-server expansion |
| [GPU Inventory](docs/10-gpu-inventory.md) | GPU registration, allocation, packing |
| [Gateway API](docs/11-gateway-api.md) | Full inference API reference with examples |
| [Policies](docs/12-policies.md) | Rate limits, quotas, priority queuing |
| [Model Aliases](docs/13-aliases.md) | Virtual model names, OpenAI compatibility layer |
| [Prompt Policies](docs/14-prompt-policies.md) | System prompt injection, PII, content filtering |
| [Usage & Billing](docs/15-usage.md) | Token tracking, cost estimation, reporting |
| [Web Admin UI](docs/16-web-ui.md) | All UI pages explained |
| [Configuration](docs/17-configuration.md) | All environment variables, Prometheus metrics |
| [Troubleshooting](docs/18-troubleshooting.md) | Common errors and fixes |

---

## Quick Start

### Option A — Local dev with Ollama (no GPU needed)

```bash
# 1. Pull a model into Ollama
ollama pull gemma2:2b

# 2. Start postgres + redis + run all migrations (001–006)
make dev-up

# 3. Run services (3 separate terminals)
make run-gateway     # inference API  → http://localhost:8080
make run-admin       # management API → http://localhost:8081
make run-scheduler   # queue dispatcher

# 4. Start the web admin UI
make web-install     # first time only
make run-web         # → http://localhost:3001

# 5. Import your Ollama models (web UI)
# → Models → Import from Ollama → click Import All

# 6. Create a team + API key (web UI)
# → Teams → Create Team → click team → API Keys → Create Key

# 7. Grant model access to the team
# → Teams → click team → click Add Model → select gemma2:2b

# 8. Make your first inference request
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer nxs_YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gemma2:2b","messages":[{"role":"user","content":"Hello!"}]}'
```

### Option B — GPU server with vLLM

```bash
# 1. Start infrastructure
make dev-up

# 2. Start services
make run-gateway && make run-admin && make run-scheduler

# 3. Deploy a vLLM model (requires NVIDIA GPU + Docker)
curl -X POST http://localhost:8081/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name":            "llama3-8b",
    "display_name":    "LLaMA 3 8B",
    "backend_type":    "vllm",
    "image":           "vllm/vllm-openai:v0.4.3",
    "hf_model_id":     "meta-llama/Meta-Llama-3-8B-Instruct",
    "host":            "localhost",
    "port":            8000,
    "auto_place":      true,
    "min_vram_mb":     16384,
    "hf_token":        "hf_...",
    "start_now":       true
  }'
```

> **No GPU?** vLLM requires a physical NVIDIA GPU. On dev machines, use Ollama (Option A).

---

## Architecture

```
Teams / Clients  (OpenAI SDK — zero code changes)
       │  Bearer nxs_...
       ▼
┌──────────────────────────────────────────────────────┐
│                 nexus-gateway :8080                   │
│                                                      │
│  Auth → Alias → Gateway Policy → Infra Policy (Redis)│
│  → Prompt Policy → Endpoint Pool → Backend → Usage   │
└──────────────────────────────────────────────────────┘
       │
       ▼
  vLLM / Ollama / TGI / Whisper / any OpenAI-compat service

┌──────────────────────────────────────────────────────┐
│                 nexus-admin :8081                     │
│  Models · Services · Nodes · Placement · Teams · GPU │
└──────────────────────────────────────────────────────┘
       ↑
  Web UI :3001  (Next.js)

PostgreSQL :5432   Redis :6379   Prometheus :9090/:9091
```

---

## Project Structure

```
nexusllm/
├── cmd/
│   ├── gateway/        ← inference API :8080
│   ├── admin/          ← management API :8081
│   └── scheduler/      ← Redis Streams queue dispatcher
│
├── internal/
│   ├── alias/          ← virtual model name resolver
│   ├── auth/           ← API key (SHA-256) + JWT
│   ├── config/         ← viper config (NEXUS_ env vars)
│   ├── controller/     ← Docker container lifecycle
│   ├── gatewaypolicy/  ← temperature/tool/model restrictions
│   ├── gpu/            ← GPU inventory + bin-packing
│   ├── lifecycle/      ← state machine + idle eviction
│   ├── middleware/     ← auth, Prometheus metrics, request ID
│   ├── models/         ← OpenAI-compatible + multi-service types
│   ├── nodeagent/      ← hardware metrics collection + heartbeat
│   ├── placement/      ← resource-aware placement engine
│   ├── policy/         ← rate limit, quota (Redis hot path)
│   ├── promptpolicy/   ← system prompt injection, PII, filters
│   ├── proxy/          ← inference pipeline + multi-service handlers
│   ├── runtime/        ← vLLM/Ollama/TGI/CPU-native + registry + watcher
│   ├── scheduler/      ← priority queue + GPU watcher
│   ├── services/       ← AI Service Registry
│   └── usage/          ← async token tracking + billing rollups
│
├── migrations/
│   ├── 001_initial.sql              ← orgs, teams, policies
│   ├── 002_seed_data.sql            ← dev org + 3 teams + seed keys
│   ├── 003_runtime_layer.sql        ← models, endpoints, runtime configs
│   ├── 004_single_gpu_runtime_seed.sql
│   ├── 005_ai_platform.sql          ← nodes, service types, reservations,
│   │                                   placement, CPU alloc, telemetry
│   └── 006_h200_platform_seed.sql   ← nexus-h200-01 node + 2× H200 GPUs
│
├── docs/                            ← Full documentation (18 pages)
│   └── README.md                    ← Documentation index
│
├── web/                             ← Next.js 14 Admin UI
│
├── Dockerfile.{gateway,admin,scheduler}
├── docker-compose.yml
├── docker-compose.single-gpu.yml
├── Makefile
└── ROADMAP.md
```

---

## Gateway API Reference

**Base URL:** `http://localhost:8080` | **Auth:** `Authorization: Bearer nxs_...`

```
POST /v1/chat/completions        LLM inference (streaming + sync)
POST /v1/embeddings              Text embeddings
POST /v1/rerank                  Cross-encoder reranking
POST /v1/audio/transcriptions    Speech-to-text (multipart/form-data)
POST /v1/audio/speech            Text-to-speech → audio binary
POST /v1/ocr                     Optical character recognition
GET  /v1/models                  Models allowed for your API key
GET  /healthz                    Liveness probe
GET  /readyz                     Readiness + live model list
```

Works with any OpenAI-compatible SDK — just change `base_url` and `api_key`:

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="nxs_...")
response = client.chat.completions.create(
    model="gemma2:2b",
    messages=[{"role": "user", "content": "Hello!"}]
)
```

---

## Admin API Reference

**Base URL:** `http://localhost:8081/admin/v1`

```
# Orgs & Teams
POST   /orgs
GET    /teams
POST   /teams                          body: {org_id, name, slug, priority}
PUT    /teams/:id/policy               body: {rpm, tpd, max_concurrent, max_context_tokens}
POST   /teams/:id/models               body: {model_name}

# API Keys
POST   /teams/:id/api-keys             body: {name, expires_at?}
GET    /teams/:id/api-keys
DELETE /api-keys/:id

# LLM Models
POST   /models/import-ollama           body: {host, port}   ← bulk import from Ollama
POST   /models/deploy                  body: full deploy spec with optional auto_place
POST   /models                         register external model
GET    /models
GET    /models/:id/health
POST   /models/:id/reset-health        clears failed state, watcher re-checks in 5s
POST   /models/:id/enable
POST   /models/:id/disable
DELETE /models/:id

# AI Services (embeddings, STT, TTS, OCR, rerankers, agents, MCP)
POST   /services/deploy                auto-place + start container
POST   /services                       register existing service
GET    /services[?type=EMBEDDING]
PUT    /services/:id/reservation       body: {min_vram_mb, cpu_cores, priority, ...}

# GPU Inventory
POST   /gpu/nodes
POST   /gpu/nodes/:id/devices
GET    /gpu/nodes
POST   /gpu/pack                       packing simulation

# Cluster Nodes
POST   /nodes
GET    /nodes
GET    /nodes/:id                      includes latest telemetry
GET    /nodes/:id/telemetry            last 60 snapshots
POST   /nodes/:id/heartbeat
POST   /nodes/:id/inventory

# Placement Engine
POST   /placement/simulate             dry-run — no resources committed
GET    /placement/decisions            placement audit log

# Usage
GET    /usage/teams/:id?from=&to=
GET    /usage/orgs/:id/monthly-spend

# Aliases
POST   /aliases
GET    /aliases
GET    /aliases/resolve?alias=gpt-4o&team_id=...
```

---

## Web Admin UI

`http://localhost:3001` — visual interface for all admin operations.

| Page | URL | Description |
|---|---|---|
| Dashboard | `/` | Platform overview, model health, cluster nodes, quick actions |
| Organizations | `/orgs` | Create and manage organizations |
| Teams | `/teams` | Teams, rate limits, model permissions, API keys |
| API Keys | `/api-keys` | Create (shown once), revoke, list |
| Models | `/models` | Import from Ollama, deploy vLLM, health + reset, enable/disable |
| AI Services | `/services` | Embeddings, STT, TTS, OCR, rerankers — all service types |
| Cluster Nodes | `/nodes` | Node status, CPU/RAM telemetry bars, hardware inventory |
| Placement | `/placement` | Resource placement simulator + decision history |
| GPU Inventory | `/gpu` | GPU nodes, devices, packing simulation |
| Usage | `/usage` | Daily token usage, cost per team/model |
| Settings | `/settings` | API reference, env vars, quick start |

---

## Make Targets

```bash
make build              # compile all 3 binaries → bin/
make run-gateway        # inference API :8080
make run-admin          # management API :8081
make run-scheduler      # queue dispatcher
make run-web            # web UI :3001
make web-install        # npm install (first time only)
make test               # go test ./... -race
make docker-build       # build all Docker images
make migrate            # run migrations 001–006 (postgres must be running)
make dev-up             # start postgres+redis + migrate
make dev-down           # stop all containers
make generate-key       # generate a new API key
make clean              # remove compiled binaries

# AI Platform shortcuts (admin must be running)
make placement-simulate MODEL=qwen3-32b VRAM=65536 GPUS=1
make node-status
make service-list
```

---

## Redis Key Reference

```
nexus:apikey:<sha256>           → TeamClaims JSON                TTL 5m
nexus:ratelimit:<team>:rpm      → sorted set (sliding window)
nexus:quota:<team>:daily:<date> → token counter                  TTL 48h
nexus:inflight:<team>           → active request count           TTL 10m
nexus:pool:<model>:at_capacity  → "0"|"1" GPU capacity flag      TTL 30s
nexus:ep:<id>:health            → health status string           TTL 30s
nexus:ep:<id>:failures          → circuit breaker counter        TTL 10m
nexus:ep:<id>:last_used         → unix timestamp                 TTL 60m
nexus:team:<id>:models          → Set of allowed model names
nexus:alias:<scope>:<id>:<name> → real model name               TTL 5m
nexus:usage:events              → Redis Stream (usage pipeline)
nexus:lifecycle:events          → pub/sub channel
nexus:queue:high / med / low    → Redis Streams (scheduler)
```

---

## Production Checklist

- [ ] `NEXUS_AUTH_JWTSECRET` → `$(openssl rand -hex 32)` — never use the default
- [ ] PostgreSQL: `sslmode=require` + daily automated backups
- [ ] Redis: AUTH password + TLS
- [ ] Firewall: restrict ports 8081 (admin) and 3001 (web) to internal network only
- [ ] `NEXUS_SERVER_MODE=release` in production
- [ ] Set `--memory` and `--cpus` limits on all Docker containers
- [ ] Prometheus alert: `nexus_runtime_endpoint_up == 0` for any model
- [ ] Run `make test` before every deployment
