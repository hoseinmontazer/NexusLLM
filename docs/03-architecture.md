# Architecture

---

## Request flow

```
Your App / OpenAI SDK
        │
        │  Bearer nxs_...
        ▼
┌─────────────────────────────────────────────────────────┐
│                    nexus-gateway :8080                   │
│                                                         │
│  1. Auth          validate API key (Redis cache → DB)   │
│  2. Alias         resolve virtual model names           │
│  3. Gateway policy  temperature caps, tool restrictions │
│  4. Infra policy  rate limit, quota, ACL, concurrency   │
│                   (all Redis — zero DB on hot path)     │
│  5. Prompt policy  system prompt injection, PII filter  │
│  6. Routing       pick healthy endpoint from pool       │
│  7. Dispatch      forward to backend (vLLM/Ollama/etc.) │
│  8. Usage         record tokens via Redis Stream        │
└─────────────────────────────────────────────────────────┘
        │
        ▼
  vLLM / Ollama / TGI / Whisper / any OpenAI-compat service


┌─────────────────────────────────────────────────────────┐
│                   nexus-admin :8081                      │
│                                                         │
│  Model Registry    register / deploy / delete models    │
│  AI Service Reg.   embeddings, STT, TTS, OCR, MCP       │
│  Placement Engine  auto GPU/CPU assignment              │
│  Node Management   cluster nodes + heartbeat            │
│  GPU Inventory     device registration + allocation     │
│  Team Management   orgs, teams, policies, API keys      │
│  Usage API         reports and billing                  │
└─────────────────────────────────────────────────────────┘
        │
        ▼
  Web Admin UI :3001   (Next.js, calls Admin API)


┌─────────────────────────────────────────────────────────┐
│                  nexus-scheduler                         │
│                                                         │
│  3 Redis Streams: high / med / low priority             │
│  GPU Watcher: polls vLLM /metrics for capacity          │
│  Job aging: promotes stale low→med→high to avoid        │
│             starvation                                  │
└─────────────────────────────────────────────────────────┘

Infrastructure:
  PostgreSQL :5432   persistent state (models, teams, usage)
  Redis :6379        hot path (auth cache, rate limits, streams)
  Prometheus :9090   gateway metrics
  Prometheus :9091   admin metrics
```

---

## Key design decisions

### Hot path is DB-free
Every policy check on the inference hot path (rate limit, quota, ACL, concurrency, GPU capacity) is a Redis operation with Lua scripts for atomicity. PostgreSQL is only hit for writes (usage events) and slow-path operations (admin API, registry reload).

### Registry pattern
The gateway holds an in-memory `Registry` with one `Pool` per model. Each Pool has one or more `Endpoint`s. The watcher updates endpoint health every 5 seconds. The gateway never queries the DB during inference.

### Backend abstraction
All AI backends (vLLM, Ollama, TGI, OpenAI-compat, cpu_native) implement the same `Backend` interface. The gateway and watcher never know which backend they're talking to.

### Async everything expensive
Usage events → Redis Stream → background consumer → PostgreSQL batch inserts. Container lifecycle operations → async goroutine → lifecycle state transitions. Neither blocks the request pipeline.

### Circuit breaker
An endpoint must fail 3 consecutive health checks before being marked `down`. Single failures produce `degraded` status and still receive traffic. This prevents brief network hiccups from taking endpoints offline.

---

## Database schema overview

```
organizations → teams → api_keys
                    ↓
                 policies          (rate limits, quotas)
                    ↓
         team_model_permissions    (which models a team can use)

models → model_endpoints → endpoint_health_log
      ↓                 ↓
  model_versions    model_lifecycle_events
      ↓
  model_runtime_configs
  resource_reservations

nodes → gpu_nodes → gpu_devices → gpu_allocations
     ↓           ↓
  node_telemetry  gpu_telemetry
  cpu_allocations
  placement_decisions

usage_events → usage_hourly → usage_daily
```

---

## Package layout

```
internal/
├── alias/          virtual model name resolver
├── auth/           API key (SHA-256) + JWT validation
├── config/         viper config loader
├── controller/     Docker-based container lifecycle
├── gatewaypolicy/  request-level policy enforcement
├── gpu/            GPU inventory + bin-packing algorithm
├── lifecycle/      endpoint state machine + idle eviction
├── middleware/     Gin middleware (auth, metrics, request ID)
├── models/         all wire types (request/response structs)
├── nodeagent/      hardware metrics collection
├── placement/      resource-aware placement engine
├── policy/         Redis-based rate limit + quota engine
├── promptpolicy/   system prompt injection, PII, moderation
├── proxy/          full inference pipeline + multi-service handlers
├── runtime/        backend implementations + registry + watcher
├── scheduler/      Redis Streams priority queue
├── services/       AI Service Registry
└── usage/          async usage tracking + billing rollups
```
