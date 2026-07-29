# External / Cloud Provider Models

## Architecture Rule

There is **one model registry**. A model is backed by either a local runtime or a remote provider — nothing else. The gateway never knows which.

```
Model
└── backend_type
    ├── vllm               (local)
    ├── tgi                (local)
    ├── llamacpp           (local)
    ├── cpu_native         (local)
    ├── openai_compat      (local self-hosted)
    ├── openai_provider    (remote)
    ├── anthropic_provider (remote)
    ├── google_provider    (remote)
    ├── azure_openai_provider (remote)
    ├── openrouter_provider   (remote)
    ├── groq_provider      (remote)
    ├── together_provider  (remote)
    ├── mistral_provider   (remote)
    ├── cohere_provider    (remote)
    └── deepseek_provider  (remote)
```

---

## Request Flow

```
Client
  POST /v1/chat/completions  {"model": "gpt-4.1"}
         │
         ▼
  ┌─────────────┐
  │     Auth    │  API key → OrgID / TeamID / ProjectID
  └──────┬──────┘
         │
         ▼
  ┌─────────────────┐
  │ Gateway Policy  │  temperature cap, tool restrictions, model allow/deny
  └──────┬──────────┘
         │
         ▼
  ┌─────────────┐
  │    Alias    │  "gpt4" → "gpt-4.1"
  └──────┬──────┘
         │
         ▼
  ┌──────────────────┐
  │ Capability Check │  model supports "chat"?
  └──────┬───────────┘
         │
         ▼
  ┌──────────────────────┐
  │  Infrastructure      │  RPM / TPM / concurrency / daily budget / quota
  │  Policy (Redis)      │  project-layer → org-governance-layer
  └──────┬───────────────┘
         │
         ▼
  ┌─────────────────┐
  │  Prompt Policy  │  system prompt injection / PII / content filter
  └──────┬──────────┘
         │
         ▼
  ┌──────────────────────────────┐
  │  Registry.ResolveWithFailover│  model → Pool → Endpoint
  └──────┬───────────────────────┘
         │
         ├─── local endpoint ──────────────────────────────────────────────►
         │    ep.URL = "http://gpu3:8001"                                    │
         │    ep.UpstreamBaseURL = ""                                        │
         │    backend = vllm / llamacpp / …                                 │
         │    EnsureRunning() called on registry miss                        │
         │                                                              Backend.Chat()
         │
         └─── provider endpoint ───────────────────────────────────────────►
              ep.URL = "http://0.0.0.0:0"  (placeholder)                    │
              ep.UpstreamBaseURL = "https://api.openai.com"                  │
              ep.UpstreamAPIKey  = "<stored key>"                            │
              backend = openAIProviderBackend / anthropicProviderBackend / … │
              EnsureRunning() NEVER called                               Backend.Chat()
                                                                              │
                                                                              ▼
                                                                   Usage Tracker
                                                                   (cost computed from
                                                                    model_cost_config)
```

**Nothing bypasses the pipeline.** Local and remote models go through identical steps 1–6. The only difference is what happens at step 7 (Resolve → Backend).

---

## Backend Interface

Every provider implements the same interface as every local runtime:

```go
type Backend interface {
    Type()               BackendType
    Health()             EndpointHealth
    Models()             []BackendModel
    Chat()               *BackendResponse
    Embeddings()         *EmbeddingResponse
    PrepareStartupArgs() []string            // no-op for providers
    ContainerPort()      int                 // 0 for providers
    ContainerPortEnvVars() map[string]string // nil for providers
}
```

The `providerNoopLifecycle` mixin provides the three lifecycle no-ops so each provider struct only implements `Type`, `Health`, `Models`, `Chat`, and `Embeddings`.

### Wire-format differences

| Provider | Wire format | Translation needed |
|---|---|---|
| OpenAI | OpenAI native | None — identical to local openai_compat |
| Anthropic | Anthropic Messages API | Request + response translated; SSE stream translated chunk-by-chunk |
| Google Gemini | OpenAI-compat at `/v1beta/openai/` | Path rewrite only |
| Azure OpenAI | Azure REST + deployment URL | URL pattern + `api-key` header instead of `Authorization: Bearer` |
| OpenRouter | OpenAI-compat at `/api/v1/` | Path rewrite only |
| Groq | OpenAI-compat at `/openai/v1/` | Path rewrite only |
| Together / Mistral / Cohere / DeepSeek | OpenAI-compat at `/v1/` | None |

---

## What providers skip

| Concern | Local model | Provider model |
|---|---|---|
| EnsureRunning / cold-start | ✓ | ✗ skipped |
| Scheduler / placement | ✓ | ✗ skipped |
| Container lifecycle (start/stop/upgrade) | ✓ | ✗ skipped |
| agent_runtime rows | ✓ | ✗ none created |
| GPU inventory | ✓ | ✗ not applicable |
| Watcher lifecycle mutations | ✓ | ✗ health_status only |

| Concern | Local model | Provider model |
|---|---|---|
| Auth | ✓ | ✓ identical |
| Gateway policy | ✓ | ✓ identical |
| Alias resolution | ✓ | ✓ identical |
| Capability validation | ✓ | ✓ identical |
| Infrastructure policy (RPM/TPM/quota) | ✓ | ✓ identical |
| Prompt policy | ✓ | ✓ identical |
| Usage tracking | ✓ | ✓ identical (+ cached/reasoning tokens) |
| Cost tracking | ✓ | ✓ identical (model_cost_config) |
| Health checking | ✓ | ✓ via UpstreamBaseURL |
| Registry / pool / failover | ✓ | ✓ identical |

---

## Database Schema

No new tables for models. All provider information is additive to existing tables.

### `models` (additions from migration 044)

```sql
provider_name         VARCHAR(64)   -- e.g. 'openai_provider'. NULL = local model.
provider_is_external  BOOLEAN       -- fast filter flag. FALSE for local models.
provider_api_version  VARCHAR(32)   -- Azure api-version, etc.
provider_extra_config JSONB         -- arbitrary provider-specific settings
```

### `model_endpoints` (additions from migration 044)

```sql
provider_timeout_seconds  INT    DEFAULT 120
provider_max_retries      INT    DEFAULT 2
provider_extra_headers    JSONB  DEFAULT '{}'
-- (upstream_api_key, upstream_base_url, upstream_proxy, upstream_model_name
--  were added by earlier migrations 040-042 and are reused unchanged)
```

### `model_cost_config` (new in migration 045)

Per-model billing rates. Used by `usage.Tracker.computeCostForModel()`.

```sql
model_id               UUID  FK → models
input_cost_per_1m      NUMERIC  -- USD per 1M input tokens
output_cost_per_1m     NUMERIC
cached_input_cost_per_1m NUMERIC
reasoning_cost_per_1m  NUMERIC
per_request_cost_usd   NUMERIC
effective_from / effective_until  -- versioned pricing
```

### `provider_rate_limits` (new in migration 045)

Provider-level RPM/TPM/cost budget limits. Scope hierarchy: `model+project > model+team > model > provider`.

```sql
provider_name   VARCHAR(64)   -- e.g. 'openai_provider'
model_id        UUID NULL     -- NULL = applies to all models for this provider
project_id      UUID NULL
team_id         UUID NULL
rpm_limit / tpm_limit / rpd_limit / tpd_limit
daily_cost_limit_usd / monthly_cost_limit_usd
error_threshold_pct / window_seconds / cooldown_seconds  -- circuit breaker
max_retries / retry_delay_ms / timeout_seconds
```

### `provider_defaults` (new in migration 044)

Canonical base URLs and capabilities per provider. Seed data included. Used by the admin UI to pre-fill forms and by the watcher to know where to health-check.

```sql
provider_name       PRIMARY KEY
display_name
default_base_url
health_path
supports_streaming / supports_functions / supports_vision / supports_embedding
```

### `usage_events` (additions from migration 045)

```sql
cached_tokens       INT      -- provider prompt-cache hits
reasoning_tokens    INT      -- o-series / extended thinking tokens
provider_name       VARCHAR  -- e.g. 'openai_provider'
provider_request_id TEXT     -- provider's own trace ID for invoice reconciliation
cost_currency       VARCHAR  -- always 'USD' currently
```

---

## Sequence Diagram: provider request (non-streaming)

```
Client          Gateway Proxy       Policy Engine     Registry           OpenAIProviderBackend
  │                  │                   │               │                      │
  │─POST /v1/chat──►│                   │               │                      │
  │                  │──Evaluate()──────►│               │                      │
  │                  │◄─allowed──────────│               │                      │
  │                  │──ResolveWithFailover("gpt-4.1")──►│                      │
  │                  │◄─(ep, backend)────────────────────│                      │
  │                  │  ep.UpstreamBaseURL="https://api.openai.com"              │
  │                  │  ep.UpstreamAPIKey="sk-…"                                │
  │                  │──backend.Chat(ChatRequest{                               │
  │                  │    EndpointURL: "https://api.openai.com",                │
  │                  │    UpstreamAPIKey: "sk-…"})──────────────────────────────►
  │                  │                                                          │
  │                  │                                          POST /v1/chat/completions
  │                  │                                          Authorization: Bearer sk-…
  │                  │                                                          │
  │                  │◄─BackendResponse{Body: …}────────────────────────────────│
  │                  │
  │                  │─── usage.Tracker.Record(Event{
  │                  │      ProviderName: "openai_provider",
  │                  │      CostUSD: computeCostForModel(modelID, prompt, completion, cached, reasoning)
  │                  │    })
  │◄─200 JSON────────│
```

---

## Sequence Diagram: Anthropic request with translation

```
Client          Gateway Proxy       AnthropicProviderBackend     api.anthropic.com
  │                  │                      │                          │
  │─POST /v1/chat──►│                      │                          │
  │  {messages:[     │                      │                          │
  │    {role:system},│                      │                          │
  │    {role:user}]} │                      │                          │
  │                  │──backend.Chat()─────►│                          │
  │                  │                      │─── translate request ───►│
  │                  │                      │  system → top-level       │
  │                  │                      │  max_tokens required      │
  │                  │                      │  x-api-key header         │
  │                  │                      │  POST /v1/messages        │
  │                  │                      │                          │
  │                  │                      │◄── Anthropic response ───│
  │                  │                      │  {content:[{text:"…"}],  │
  │                  │                      │   usage:{input:N,output:M}}
  │                  │                      │─── translate response    │
  │                  │                      │  → OpenAI format         │
  │                  │◄─BackendResponse─────│  choices[0].message.content
  │◄─200 OpenAI JSON─│
```

For streaming, `anthropicSSEStream` translates each Anthropic SSE event to an OpenAI `chat.completion.chunk` on the fly. The gateway's existing SSE proxy loop is unchanged.

---

## Watcher Behaviour

```
Watcher.checkOne(ep)
  │
  ├── healthURL = ep.UpstreamBaseURL ?? ep.URL
  │                                       ▲
  │     provider: "https://api.openai.com" │
  │     local:    "http://gpu3:8001"        │
  │
  ├── backend.Health(healthURL)
  │     provider: GET /v1/models (401 treated as healthy — key absent from probe)
  │     local:    GET /health
  │
  ├── circuit-breaker (3 consecutive failures → StatusDown)
  │
  ├── registry.UpdateEndpointHealth()    ← both paths
  ├── persistHealthResult()              ← both paths (health_status column only)
  │
  ├── IsProviderBackend(ep.BackendType)?
  │     YES → emit debug log, update Prometheus gauges, RETURN
  │            (no agent_runtimes mutations, no is_enabled flips)
  │
  └── NO  → full local lifecycle management
            (promote loading→ready, demote down→unhealthy, etc.)
```

---

## Security

### API Key storage

Keys are stored in `model_endpoints.upstream_api_key` (plaintext in DB by default). Migration 040 notes this explicitly. For production:

- Use a secrets manager (Vault, AWS Secrets Manager, GCP Secret Manager) and store a reference like `vault://secret/openai-key` in the column.
- Resolve the reference at registry load time in `Registry.loadEndpoints()`.
- Enable PostgreSQL column-level encryption or transparent data encryption at the storage layer.

The gateway **never returns** `upstream_api_key` in any API response. `GetModelHealth` and `ListModels` return only `upstream_api_key_set: bool`.

### Key rotation

`PUT /admin/v1/models/:id/upstream` updates `upstream_api_key` and immediately triggers `registry.Reload()`. Zero downtime — in-flight requests use the old key from the endpoint struct; new requests after reload use the new key.

### Proxy authentication

`upstream_proxy` is stored per-endpoint. The Factory caches one `*http.Client` per unique proxy URL. SOCKS5 and HTTP proxies are supported.

---

## Metrics

All provider metrics are in the `nexus_provider_*` namespace with `provider` and `model` labels.

| Metric | Type | Description |
|---|---|---|
| `nexus_provider_requests_total` | Counter | Requests by provider/model/status |
| `nexus_provider_latency_seconds` | Histogram | End-to-end round-trip |
| `nexus_provider_cost_total` | Counter | Cumulative USD cost |
| `nexus_provider_failures_total` | Counter | Failures by type (timeout/rate_limited/auth_error/…) |
| `nexus_provider_tokens_total` | Counter | Tokens by type (input/output/cached/reasoning) |
| `nexus_provider_retry_total` | Counter | Automatic retries |
| `nexus_provider_timeout_total` | Counter | Timeout events |
| `nexus_provider_health_status` | Gauge | 1=healthy, 0=down per provider/model |
| `nexus_provider_ttft_seconds` | Histogram | Time to first streaming token |

---

## Admin API

### Register a cloud model

```
POST /admin/v1/models/external
{
  "name":                  "gpt-4.1",
  "display_name":          "OpenAI GPT-4.1",
  "provider_backend_type": "openai_provider",
  "service_type":          "CHAT",
  "upstream_api_key":      "sk-…",
  "upstream_base_url":     "https://api.openai.com",
  "upstream_model_name":   "gpt-4.1",
  "capabilities":          ["chat","completion","vision"],
  "provider_timeout_seconds": 120,
  "provider_max_retries":     2
}

201 Created
{
  "model_id":              "…",
  "endpoint_id":           "…",
  "provider_backend_type": "openai_provider",
  "upstream_base_url":     "https://api.openai.com",
  "upstream_api_key_set":  true,
  "status":                "active",
  "note":                  "provider model registered and immediately routable — full policy pipeline applies"
}
```

### upstream_base_url per provider

Each provider backend appends its own path to `upstream_base_url`. Use the root domain — **do not include the API path**.

| Provider | `upstream_base_url` | Final chat URL constructed internally |
|---|---|---|
| `openai_provider` | `https://api.openai.com` | `…/v1/chat/completions` |
| `anthropic_provider` | `https://api.anthropic.com` | `…/v1/messages` |
| `google_provider` | `https://generativelanguage.googleapis.com` | `…/v1beta/openai/chat/completions` |
| `azure_openai_provider` | `https://YOUR_RESOURCE.openai.azure.com` | `…/openai/deployments/<model>/chat/completions?api-version=…` |
| `openrouter_provider` | `https://openrouter.ai` | `…/api/v1/chat/completions` |
| `groq_provider` | `https://api.groq.com` | `…/openai/v1/chat/completions` |
| `together_provider` | `https://api.together.xyz` | `…/v1/chat/completions` |
| `mistral_provider` | `https://api.mistral.ai` | `…/v1/chat/completions` |
| `cohere_provider` | `https://api.cohere.com` | `…/v1/chat/completions` |
| `deepseek_provider` | `https://api.deepseek.com` | `…/v1/chat/completions` |

> **Common mistake:** setting `upstream_base_url` to `https://openrouter.ai/api/v1` for OpenRouter.
> The backend strips `/v1` then appends `/api/v1/chat/completions`, producing a double-path
> (`/api/api/v1/chat/completions`) that returns a 404 HTML page from OpenRouter.
> Always use the root domain: `https://openrouter.ai`.

### List providers

```
GET /admin/v1/providers

200 OK
{
  "data": [
    { "provider_name": "openai_provider", "display_name": "OpenAI",
      "default_base_url": "https://api.openai.com",
      "supports_streaming": true, "supports_vision": true, … },
    …
  ]
}
```

### Rotate an API key

```
PUT /admin/v1/models/:id/upstream
{ "upstream_api_key": "sk-new-key" }
```

### Update per-provider outbound proxy

```
PUT /admin/v1/models/:id/transport
{ "proxy_url": "socks5://192.168.0.207:3315" }
```

Supported schemes: `http://`, `https://`, `socks5://`. Credentials may be embedded:
`http://user:pass@proxy.corp:3128`. Send `""` to remove the proxy and connect directly.

The registry rebuilds the per-endpoint `*http.Client` immediately after the update — no restart required.
Transport is fully isolated per provider: updating OpenRouter's proxy never affects OpenAI, Anthropic, or any other provider.

> **Note:** the `:id` parameter must be the model's UUID, not its name. Retrieve it with:
> ```
> GET /admin/v1/models
> ```
> and filter by `name`.

### Get current transport config

```
GET /admin/v1/models/:id/transport

200 OK
{
  "model_id": "…",
  "count": 1,
  "endpoints": [{
    "endpoint_id": "…",
    "proxy_url": "socks5://192.168.0.207:3315",
    "tls_insecure_skip_verify": false,
    "tls_root_ca_pem_set": false,
    "connect_timeout_seconds": 0,
    "read_timeout_seconds": 0,
    "idle_conn_timeout_seconds": 0,
    "response_header_timeout_seconds": 0,
    "max_idle_conns_per_host": 0,
    "max_conns_per_host": 0,
    "disable_http2": false
  }],
  "note": "zero values mean BuildProviderClient() defaults apply (connect=10s idle=90s response_header=30s pool=32)"
}
```

### Per-provider proxy: full transport field reference

All fields are optional. Zero values apply production defaults.

| Field | Default | Description |
|---|---|---|
| `proxy_url` | `""` (direct) | Outbound proxy. Schemes: `http`, `https`, `socks5`. Credentials allowed. |
| `tls_insecure_skip_verify` | `false` | Disable TLS cert check. Only for corporate MITM proxies. |
| `tls_root_ca_pem` | `""` | PEM root CA bundle appended to system roots. |
| `connect_timeout_seconds` | `10` | TCP dial + TLS handshake timeout. |
| `read_timeout_seconds` | `0` | Non-streaming body read timeout. `0` = unlimited (streaming-safe). |
| `idle_conn_timeout_seconds` | `90` | Keep-alive idle connection pool timeout. |
| `response_header_timeout_seconds` | `30` | Max wait for response headers after request sent. `-1` = disabled. |
| `max_idle_conns_per_host` | `32` | Idle keep-alive connections in pool per host. |
| `max_conns_per_host` | `0` | Total connections per host. `0` = unlimited. |
| `disable_http2` | `false` | Prevent HTTP/2 negotiation via ALPN. |

---

## Migration Plan

### New installations

Run migrations in order. All are idempotent:

```
044_external_provider_models.sql
045_provider_cost_and_rate_limits.sql
```

### Existing installations with legacy cloud models

If you previously registered cloud models using `POST /admin/v1/models` with `backend_type=openai_compat` and an `upstream_base_url`, migration 044 automatically:

1. Sets `provider_is_external=true` on those models.
2. Sets `provider_name='openai_provider'` (generic fallback).

To get full provider-specific behaviour (e.g. Anthropic translation, Gemini path rewrite), re-register those models using `POST /admin/v1/models/external` with the correct `provider_backend_type`.

The old `POST /admin/v1/models` endpoint continues to work unchanged for self-hosted models and backward compatibility.

---

## Invariants

These properties must hold at all times:

1. **Single registry.** `runtime.Registry` is the sole source of truth for all models, local and remote.
2. **Identical routing.** `proxy.Handler.ChatCompletions` has zero provider-specific branches. It resolves an endpoint and calls `backend.Chat()`.
3. **No policy bypass.** Every request traverses Auth → GatewayPolicy → Alias → CapabilityCheck → InfraPolicy → PromptPolicy → Resolve → Backend.
4. **No lifecycle for providers.** `IsProviderBackend(ep.BackendType)` gates all container lifecycle calls. Adding a new provider type requires only adding it to `IsProviderBackend()` and registering a Backend constructor in `Factory.NewFactory()`.
5. **Stable client API.** Clients always call `/v1/chat/completions`. The `model` field in the response always echoes the NexusLLM model name, never the upstream provider's internal ID.
