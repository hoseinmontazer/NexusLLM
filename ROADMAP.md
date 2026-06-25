# NexusLLM — AI Platform Roadmap

## Current Status: v0.7 — Weighted Priority Scheduler

NexusLLM has been transformed from an LLM gateway into a full AI Resource
Orchestrator. The sections below describe what is implemented and what comes next.

---

## Hardware Target

| Resource | Specification |
|---|---|
| Server | Single AI Server (expandable to cluster) |
| GPUs | 2× NVIDIA H200 NVL (144 GB VRAM each = 288 GB total) |
| vCPUs | 384 logical cores |
| RAM | 1 TB |
| SSD | 10 TB NVMe |
| HDD | 54 TB |
| NUMA | 2 nodes (GPU 0 → NUMA 0, GPU 1 → NUMA 1) |

---

## ✅ Implemented (v0.5)

### Core Platform (pre-existing)
- Runtime Registry — per-model endpoint pool with health tracking
- Runtime Controller — async container lifecycle (start / stop / restart / upgrade / rollback)
- Runtime Watcher — background health checks with circuit breaker
- GPU Inventory — device registration, allocation, telemetry
- GPU Packing — FFD bin-packing for multi-model placement
- Dynamic Model Loading — on-demand container deployment via Docker
- Usage Tracking — per-team/org token usage via Redis Streams → PostgreSQL
- Prompt Policies — system prompt injection, PII detection, content moderation
- Team Policies — RPM, TPD, concurrency, context window limits (Redis hot path)
- Model Aliases — virtual model names scoped to org/team/global
- OpenAI Compatible API — `/v1/chat/completions`, `/v1/embeddings`, `/v1/models`
- Multi-tenant Auth — API keys (SHA-256 hash), JWT, Redis cache
- Admin API — full CRUD for orgs, teams, models, policies, aliases

### Resource-Aware Runtime Placement (v0.5 — new)
- **Placement Engine** (`internal/placement/`) — scores nodes by VRAM headroom,
  GPU temperature, GPU utilisation, NUMA locality, and RAM availability.
  Separate scoring paths for `GPU_RUNTIME` and `CPU_RUNTIME` workloads.
- **Auto-placement** — `POST /admin/v1/models/deploy` with `auto_place: true`
  automatically selects GPU(s) from inventory. Manual assignment still works.
- **Placement Simulation** — `POST /admin/v1/placement/simulate` for dry-runs
  without committing resources. Useful for capacity planning.
- **Placement Audit Log** — every decision is written to `placement_decisions`.

### CPU Runtime Support (v0.5 — new)
- **CPU_RUNTIME type** — all services declare `runtime_type: GPU_RUNTIME | CPU_RUNTIME`.
- **cpu_native backend** — wraps OpenAI-compatible HTTP for CPU services.
- **Docker cpu affinity** — `--cpuset-cpus` and `--cpuset-mems` passed when
  `CPUSetCPUs` / `NUMANode` are set on the RuntimeSpec.
- **CPU Allocations** — `cpu_allocations` table tracks core and RAM reservations.

### AI Service Registry (v0.5 — new)
Extends the model registry to cover all AI service types:

| service_type | Example Runtimes | Backend |
|---|---|---|
| `CHAT` | vLLM, Ollama, TGI | `vllm` / `ollama` / `tgi` |
| `EMBEDDING` | Infinity, TEI, FastEmbed | `cpu_native` / `openai_compat` |
| `RERANK` | TEI rerank, Cohere-compat | `cpu_native` |
| `STT` | faster-whisper-server, whisper.cpp | `cpu_native` |
| `TTS` | Kokoro TTS, Coqui TTS | `cpu_native` |
| `OCR` | EasyOCR REST, Tesseract API | `cpu_native` |
| `AGENT` | LangChain serve, custom agents | `openai_compat` |
| `MCP` | MCP HTTP bridges | `openai_compat` |

Admin API:
- `POST /admin/v1/services` — register existing service
- `POST /admin/v1/services/deploy` — register + auto-place + deploy
- `GET  /admin/v1/services[?type=EMBEDDING]` — list services
- `GET/PUT /admin/v1/services/:id/reservation` — resource reservations

### OpenAI Compatible Multi-Service Gateway (v0.5 — new)
- `POST /v1/chat/completions` — LLM chat (existing)
- `POST /v1/embeddings` — embedding models (existing)
- `POST /v1/rerank` — reranker models (new)
- `POST /v1/audio/transcriptions` — STT/Whisper (new)
- `POST /v1/audio/speech` — TTS engines (new)
- `POST /v1/ocr` — OCR services (new)

All endpoints follow the same auth → policy → alias → failover pipeline.

### Project Priority & Scheduling (v0.6 — weighted system)
- **Weighted priority** — `priority_weight INTEGER [0–1000]` replaces the 5-tier enum
  (`CRITICAL/HIGH/NORMAL/LOW/BEST_EFFORT`). Multiple projects at the same weight are
  differentiated by effective priority (base + aging + reservation bonus − quota penalty).
- **Effective priority** — computed per-scheduler-cycle by `compute_effective_priority()`;
  stored in `project_effective_priority` for audit and display.
- **Anti-starvation aging** — waiting jobs gain +1 per 60 s in queue, capped at +200.
  A Best Effort job waiting 3+ hours can displace a Standard job.
- **Reservation bonus** — projects with declared resource reservations receive +50 bonus.
- **Quota penalty** — projects over their `max_gpu_vram_mb` / `max_cpu` ceiling get −100.
- **Preemption gap rule** — `effective_priority(requester) − priority_weight(victim) ≥ 50`.
  Prevents thrashing between near-equal projects.
- **Preemptible flag** — `projects.preemptible=false` marks a project as safe from eviction
  regardless of priority gap.
- **Non-preemptible projects** — weight ≥ 900 projects are seeded `preemptible=false` on migration.
- **Deployment queue** — ordered by `effective_priority DESC, waiting_since ASC`; exponential
  backoff on retry (30s × 2^attempts, cap 30 min).
- **Scheduler decisions table** — every placement decision logged with full priority trace,
  node score, and alternatives.
- **Scheduler API** — `GET /scheduler/queue`, `GET /scheduler/decisions`,
  `GET /scheduler/priority-presets`.
- **UI** — numeric priority input with preset picker, priority bar, `EffectivePriorityCard`
  breakdown, queue panel, preemption history with numeric weights.
- **Migration 018** — idempotent, back-fills existing projects from old enum values.

### Automatic Scheduler (v0.7 — new)
- **Scheduler engine** (`internal/scheduler/`) — node loading, filtering, multi-factor scoring,
  tie-breaking, decision building with full `DecisionTrace`.
- **Node scoring** — capacity (VRAM/RAM/CPU free ratio, 0–400) + load (GPU util, runtime density,
  health, 0–300) + locality (model cached on node, 0–200) + priority bonus (0–200).
- **Cold start** — `RuntimeActivator.EnsureRunning` calls scheduler when `node_id` is empty.
- **Queue processor** — background loop every 30 s retries queued deployments.
- **Capacity management** — queue on exhaustion; preempt when admission_policy allows.
- **Node capabilities** — `node_capabilities` table; scheduler filters by GPU availability.
- **Model requirements** — `model_requirements` table caches computed VRAM/CPU/RAM needs.



### Cluster-Ready Multi-Server Architecture (v0.5 — new)
- **`nodes` table** — hostname, total_cpu, total_ram_mb, total_vram_mb, status, labels
- `model_endpoints` and `cpu_allocations` reference `node_id`
- Placement engine queries `nodes` first, then narrows to GPU/CPU devices on that node
- Current deployment: 1 node (`nexus-h200-01`). Adding nodes = inserting rows.
- No Kubernetes, no distributed scheduler — pure PostgreSQL coordination.

### Node Agent (v0.5 — new)
`internal/nodeagent/` runs in-process (single server) or standalone (multi-server):
- Collects: CPU util, RAM, disk, NUMA topology via `/proc`
- Collects: GPU metrics via `nvidia-smi` (util, VRAM, temp, power, fan, PCIe bus)
- Reports: inventory snapshot on startup, telemetry every 30 s
- Writes: `node_telemetry`, `gpu_telemetry`, `node_inventory_snapshots`
- Heartbeat: `UPDATE nodes SET status='online', last_heartbeat_at=NOW()`

Admin API:
- `POST /admin/v1/nodes/:id/heartbeat` — agent liveness
- `POST /admin/v1/nodes/:id/inventory` — push full inventory
- `GET  /admin/v1/nodes/:id/telemetry` — last 60 telemetry snapshots
- `GET  /admin/v1/nodes/:id/inventory` — latest inventory snapshot

---

## Recommended H200 Service Layout

```
GPU 0 (144 GB VRAM, NUMA 0)
├── Qwen3-32B          — 65 GB  (vLLM, tensor_parallel=1)
└── DeepSeek-Coder-7B  — 16 GB  (vLLM, tensor_parallel=1)
    Reserve: 63 GB for on-demand loading

GPU 1 (144 GB VRAM, NUMA 1)
├── Llama-3.3-70B      — 140 GB (vLLM, tensor_parallel=1, fp8)
    Reserve: 4 GB headroom

CPU Pool (384 vCPUs, 1 TB RAM)
├── NUMA 0 (cores 0-191, ~512 GB RAM)
│   ├── Embedding Service (Infinity/TEI) — 32 cores, 64 GB
│   ├── Reranker Service (TEI rerank)    — 16 cores, 32 GB
│   └── MCP Bridge Services             — 8 cores,  8 GB
└── NUMA 1 (cores 192-383, ~512 GB RAM)
    ├── Whisper STT (faster-whisper)     — 32 cores, 16 GB
    ├── Kokoro TTS                       — 16 cores, 8 GB
    ├── OCR Service (EasyOCR)            — 16 cores, 16 GB
    └── Agent Runtimes                   — 64 cores, 128 GB
```

### GPU Allocation Examples

| Model | VRAM | GPU | tensor_parallel | Placement |
|---|---|---|---|---|
| Qwen3-32B | ~65 GB | GPU 0 | 1 | `auto_place: true, min_vram_mb: 65536` |
| DeepSeek-Coder-7B | ~16 GB | GPU 0 | 1 | fits on GPU 0 with Qwen3-32B |
| Llama-3.3-70B | ~140 GB | GPU 1 | 1 | fp8 quantization |
| DeepSeek-V3 | 288 GB | GPU 0+1 | 2 | `gpu_count: 2, min_vram_mb: 144000` |

### CPU Workload Placement

| Service | Cores | RAM | NUMA | Priority weight |
|---|---|---|---|---|
| Embedding (Infinity) | 32 | 64 GB | 0 | 500 (Standard) |
| Reranker (TEI) | 16 | 32 GB | 0 | 500 (Standard) |
| Whisper STT | 32 | 16 GB | 1 | 500 (Standard) |
| Kokoro TTS | 16 | 8 GB | 1 | 300 (Batch) |
| OCR (EasyOCR) | 16 | 16 GB | 1 | 300 (Batch) |
| Agent Runtimes | 64 | 128 GB | 1 | 700 (Core Internal) |
| MCP Bridges | 8 | 8 GB | 0 | 0 (Best Effort) |

---

## Roadmap

### v0.6 — Observability & Web UI
- [ ] Prometheus metrics for all new service types (rerank, STT, TTS, OCR)
- [ ] GPU telemetry Prometheus gauges (temperature, power, fan from node agent)
- [ ] Node agent dashboard in web UI
- [ ] AI Service Registry UI (list, deploy, manage all service types)
- [ ] Placement decisions history page
- [ ] Real-time resource utilisation gauges (VRAM, CPU, RAM per node)

### v0.7 — Dynamic Load & Eviction
- [ ] Idle eviction for CPU_RUNTIME services (lifecycle manager extension)
- [ ] On-demand model loading triggered by first request (cold start)
- [ ] VRAM pressure eviction — evict LRU model when GPU is full
- [ ] Swap-to-RAM support for idle models (vLLM `--cpu-offload-gb`)
- [ ] Priority-aware eviction: `best_effort` evicted before `critical`

### v0.8 — Multi-Server Expansion
- [ ] Node Agent standalone binary (`cmd/nodeagent`)
- [ ] Node Agent → Admin API push protocol (gRPC or HTTP)
- [ ] Cross-node placement scoring (network bandwidth between nodes)
- [ ] Distributed GPU allocation with lease-based concurrency control
- [ ] Node health monitoring with automatic failover

### v0.9 — Production Hardening
- [ ] TLS termination on gateway and admin
- [ ] Admin API authentication (API key or mTLS)
- [ ] Rolling deployments (blue-green endpoint swap)
- [ ] Backup / restore for model registry state
- [ ] Rate limiting on admin API
- [ ] Audit log retention policy

### v1.0 — GA
- [ ] Stable API (no breaking changes after this)
- [ ] Full integration test suite
- [ ] Load test benchmarks (requests/sec per service type on H200)
- [ ] Production runbook and on-call guide
- [ ] Helm chart for containerised deployment (single-node)
