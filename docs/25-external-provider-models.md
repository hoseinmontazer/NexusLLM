# External / Cloud Provider Models

This document covers two ways to use cloud providers like OpenRouter through NexusLLM:

- **Managed mode** — register individual models as Public Models (classic enterprise workflow)
- **Catalog mode** — expose an entire provider catalogue as virtual models (OpenRouter-style, no registration required)

Both paths go through the full policy pipeline. Nothing is bypassed.

---

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

## `GET /v1/providers/:name/models` — Raw provider passthrough

This endpoint proxies the provider's own `/models` response **without any transformation**. The client gets exactly what OpenRouter (or any other configured provider) returns — including every provider-specific field:

```bash
# OpenRouter — all 600+ models with full metadata
curl http://localhost:8880/v1/providers/openrouter/models \
  -H "Authorization: Bearer <your-nexus-api-key>"

# OpenAI
curl http://localhost:8880/v1/providers/openai/models \
  -H "Authorization: Bearer <your-nexus-api-key>"

# Anthropic
curl http://localhost:8880/v1/providers/anthropic/models \
  -H "Authorization: Bearer <your-nexus-api-key>"
```

The `:name` segment matches the `name` column of the `providers` table — the internal name you set when creating the provider (e.g. `openrouter`, `openai`, `anthropic`).

**The response is the provider's raw JSON.** For OpenRouter it looks like:

```json
{
  "data": [
    {
      "id": "openai/gpt-5",
      "canonical_slug": "openai/gpt-5",
      "name": "GPT-5",
      "created": 1785606009,
      "description": "...",
      "context_length": 1048576,
      "architecture": {
        "modality": "text->text",
        "input_modalities": ["text"],
        "output_modalities": ["text"],
        "tokenizer": "Router",
        "instruct_type": null
      },
      "pricing": {
        "prompt": "0.00000009",
        "completion": "0.00000018",
        "input_cache_read": "0.000000018"
      },
      "top_provider": {
        "context_length": 1048576,
        "max_completion_tokens": 65536,
        "is_moderated": false
      },
      "supported_parameters": [
        "frequency_penalty", "include_reasoning", "logit_bias",
        "logprobs", "max_tokens", "presence_penalty", "reasoning",
        "reasoning_effort", "response_format", "seed", "stop",
        "structured_outputs", "temperature", "tool_choice", "tools",
        "top_k", "top_logprobs", "top_p"
      ],
      "per_request_limits": null,
      "reasoning": {
        "mandatory": false,
        "default_enabled": true,
        "supported_efforts": ["max", "high", "low"],
        "default_effort": "high"
      }
    }
  ]
}
```

**Auth still applies.** A valid NexusLLM API key is required. The request is not counted against rate limits (it's a catalog read, not an inference request).

**Query parameters are forwarded.** Any query string you pass is appended to the upstream URL unchanged:

```bash
# OpenRouter supports ?supported_parameters=tools to filter tool-capable models
curl "http://localhost:8880/v1/providers/openrouter/models?supported_parameters=tools" \
  -H "Authorization: Bearer <nexus-key>"
```

**How the upstream URL is built per provider:**

| Provider name | Backend type | Upstream URL called |
|---|---|---|
| `openrouter` | `openrouter_provider` | `https://openrouter.ai/api/v1/models` |
| `openai` | `openai_provider` | `https://api.openai.com/v1/models` |
| `anthropic` | `anthropic_provider` | `https://api.anthropic.com/v1/models` |
| `groq` | `groq_provider` | `https://api.groq.com/openai/v1/models` |
| `gemini` | `google_provider` | `https://generativelanguage.googleapis.com/v1beta/openai/models` |
| any other | `*_provider` | `<base_url>/v1/models` |

The provider's stored `api_key` is injected as `Authorization: Bearer <key>` (or the configured `api_key_header`) on the upstream request. The outbound proxy (if configured on the provider) is used automatically.

Response headers `X-Nexus-Provider` and `X-Nexus-Provider-URL` indicate which provider was called and the exact URL that was used.

---

## Option A — Catalog Mode (OpenRouter-style, recommended for large providers)

Catalog mode lets you expose an entire provider catalogue (e.g. all 600+ OpenRouter models) without registering each one individually. Models appear as virtual names in `GET /v1/models`. Authorization is at the project level, not the model level.

### Step 1 — Create the provider

```bash
curl -X POST http://localhost:8081/admin/v1/providers \
  -H 'Content-Type: application/json' \
  -d '{
    "name":                  "openrouter",
    "display_name":          "OpenRouter",
    "backend_type":          "openrouter_provider",
    "base_url":              "https://openrouter.ai",
    "api_key":               "sk-or-v1-…",
    "exposure_mode":         "catalog",
    "catalog_sync_enabled":  true,
    "catalog_sync_interval": 3600,
    "catalog_expose_prefix": "openrouter"
  }'
```

`exposure_mode` options:

| Value | Behaviour |
|-------|-----------|
| `managed` | Default. Only explicitly registered Public Models are visible. |
| `catalog` | Provider catalogue exposed directly as virtual models. No registration required. |
| `hybrid` | Both: registered Public Models AND virtual catalog models are visible simultaneously. |

### Step 2 — Sync the catalog

```bash
curl -X POST http://localhost:8081/admin/v1/providers/<provider-id>/sync
```

This fetches OpenRouter's `/api/v1/models` endpoint, stores all model metadata (name, description, context_length, pricing, capabilities) in `provider_remote_models`, and marks them as ready to route.

Check sync status:

```bash
curl http://localhost:8081/admin/v1/providers/<provider-id>
# → catalog_model_count: 634, catalog_sync_status: "ok"
```

### Step 3 — (Optional) Add exposure rules

By default, all synced models are exposed. Add rules to restrict which models are visible to clients.

Allow only specific model families:

```bash
curl -X POST http://localhost:8081/admin/v1/providers/<provider-id>/rules \
  -H 'Content-Type: application/json' \
  -d '{"rule_type": "allow_pattern", "pattern": "openai/*", "priority": 10}'

curl -X POST http://localhost:8081/admin/v1/providers/<provider-id>/rules \
  -H 'Content-Type: application/json' \
  -d '{"rule_type": "allow_pattern", "pattern": "anthropic/*", "priority": 10}'
```

Deny free/preview models by tag:

```bash
curl -X POST http://localhost:8081/admin/v1/providers/<provider-id>/rules \
  -H 'Content-Type: application/json' \
  -d '{"rule_type": "deny_pattern", "pattern": "*:free", "priority": 5}'
```

Preview what would be exposed:

```bash
curl -X POST http://localhost:8081/admin/v1/providers/<provider-id>/rules/preview
# → { "exposed_count": 142, "blocked_count": 492 }
```

### Step 4 — Grant a project access to this provider

Virtual models require a project context. The project's rate limits and quota apply to all calls regardless of which virtual model is used.

```bash
# Allow the project to call ALL models from this provider
curl -X POST http://localhost:8081/admin/v1/projects/<project-id>/provider-access \
  -H 'Content-Type: application/json' \
  -d '{"provider_id": "<provider-id>"}'
```

Allow only specific model families for this project:

```bash
curl -X POST http://localhost:8081/admin/v1/projects/<project-id>/provider-access \
  -H 'Content-Type: application/json' \
  -d '{
    "provider_id":      "<provider-id>",
    "allowed_prefixes": ["openrouter/openai/*", "openrouter/anthropic/*"],
    "denied_prefixes":  ["openrouter/openai/gpt-4-*"]
  }'
```

Prefix patterns use `path.Match` glob. `*` matches within one path segment.

### Step 5 — Scope an API key to the project

```bash
curl -X PUT http://localhost:8081/admin/v1/api-keys/<key-id>/project \
  -H 'Content-Type: application/json' \
  -d '{"project_id": "<project-id>"}'
```

### Step 6 — Call any virtual model

```bash
curl http://localhost:8880/v1/models \
  -H "Authorization: Bearer <your-nexus-api-key>"
```

Response includes OpenRouter-style rich metadata:

```json
{
  "object": "list",
  "data": [
    {
      "id":             "openrouter/openai/gpt-5",
      "object":         "model",
      "created":        1704067200,
      "owned_by":       "nexusllm",
      "name":           "GPT-5",
      "description":    "OpenAI's most powerful model…",
      "context_length": 1048576,
      "architecture": {
        "modality":          "text->text",
        "input_modalities":  ["text"],
        "output_modalities": ["text"],
        "tokenizer":         "Router"
      },
      "pricing": {
        "prompt":     "9e-08",
        "completion": "3.6e-07"
      },
      "top_provider": {
        "context_length":       1048576,
        "max_completion_tokens": 32768,
        "is_moderated":         false
      },
      "supported_parameters": ["temperature", "top_p", "max_tokens", "stream", "tools", "tool_choice"]
    },
    {
      "id": "openrouter/anthropic/claude-opus-4",
      …
    }
  ]
}
```

Then call any model by its virtual name — identical to calling any other NexusLLM model:

```bash
curl http://localhost:8880/v1/chat/completions \
  -H "Authorization: Bearer <your-nexus-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "openrouter/openai/gpt-5",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

```bash
# Streaming
curl http://localhost:8880/v1/chat/completions \
  -H "Authorization: Bearer <your-nexus-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model":    "openrouter/anthropic/claude-opus-4",
    "messages": [{"role": "user", "content": "Explain transformers"}],
    "stream":   true
  }'
```

The model name format is always `<expose_prefix>/<provider_model_id>`. For OpenRouter the provider model ID itself contains slashes (e.g. `openai/gpt-5`), so the full virtual name is `openrouter/openai/gpt-5`.

### What the full request flow looks like

```
Client → POST /v1/chat/completions { model: "openrouter/openai/gpt-5" }
  │
  ▼ Auth (API key → ProjectID)
  ▼ Alias resolution (no-op for virtual names)
  ▼ Gateway policy (temperature cap, tool restrictions)
  ▼ Policy Engine — Evaluate()
      Step 0 model ACL:
        Path A: nexus:org:<id>:models SISMEMBER → miss (not a registered Public Model)
        Path B: nexus:project:<id>:vproviders HGETALL
                → OpenRouter provider entry found
                → "openrouter/" prefix matches → ALLOWED
      Step 1: project RPM / TPM / budget / concurrency limits
      Step 2: org governance (disabled flag, monthly budget)
  ▼ Prompt policy
  ▼ registry.ResolveWithFailover() → miss (not a Public Model)
  ▼ virtualResolver.Resolve("openrouter/openai/gpt-5")
      → VirtualEndpoint {
          BackendType:       openrouter_provider,
          UpstreamBaseURL:   "https://openrouter.ai",
          UpstreamAPIKey:    "sk-or-…",
          UpstreamModelName: "openai/gpt-5"
        }
  ▼ BuildProviderClient(transport) — per-provider isolated HTTP client
  ▼ virtualDispatch:
      IncrementProjectInflight
      backend.Chat() → POST https://openrouter.ai/api/v1/chat/completions
                         model: "openai/gpt-5"   ← upstream name substituted
  ▼ Stream / return response to client
  ▼ RecordProjectTokenUsage, RecordOrgTokenUsage
  ▼ UsageTracker.Record(provider, model, tokens, latency, cost)
  ▼ DecrementProjectInflight
```

### Rate limits in catalog mode

All virtual models from all granted providers share the **same project budget**:

```
Project A — 100 RPM, 500,000 TPM

openrouter/openai/gpt-5          ─┐
openrouter/anthropic/claude-opus  ├─ all consume the SAME project RPM/TPM/budget
openrouter/google/gemini-2.5-pro ─┘
```

Configure limits via:

```bash
curl -X PUT http://localhost:8081/admin/v1/projects/<project-id>/policy \
  -H 'Content-Type: application/json' \
  -d '{
    "rpm":                  100,
    "tpm":                  500000,
    "daily_token_budget":   10000000,
    "monthly_token_budget": 200000000
  }'
```

---

## Option B — Managed Mode (register individual models)

Use this when you want explicit control over which models are available. Each model is registered as a Public Model and granted to teams via `team_model_permissions`.

### Register an OpenRouter model

```bash
curl -X POST http://localhost:8081/admin/v1/models/external \
  -H 'Content-Type: application/json' \
  -d '{
    "name":                  "claude-opus-4",
    "display_name":          "Claude Opus 4 (via OpenRouter)",
    "provider_backend_type": "openrouter_provider",
    "service_type":          "CHAT",
    "upstream_api_key":      "sk-or-v1-…",
    "upstream_base_url":     "https://openrouter.ai",
    "upstream_model_name":   "anthropic/claude-opus-4",
    "capabilities":          ["chat", "completion"],
    "provider_timeout_seconds": 120,
    "provider_max_retries":     2
  }'
```

`upstream_model_name` is what NexusLLM sends in the `model` field to the upstream provider. If omitted, the NexusLLM model name is used.

### Register from the provider catalog (bulk)

If the provider catalog has already been synced (Option A steps 1–2 can be done with `exposure_mode: "managed"` just for syncing), promote catalog entries to Public Models:

```bash
curl -X POST http://localhost:8081/admin/v1/providers/<provider-id>/register-models \
  -H 'Content-Type: application/json' \
  -d '{
    "models": [
      {
        "public_name":       "claude-opus-4",
        "provider_model_id": "anthropic/claude-opus-4",
        "service_type":      "CHAT"
      },
      {
        "public_name":       "gpt-5",
        "provider_model_id": "openai/gpt-5",
        "service_type":      "CHAT"
      }
    ]
  }'
```

Each entry becomes a first-class Public Model with its own `models` row and endpoint. Grant team access via the standard `POST /admin/v1/teams/:id/models` flow.

### Grant team access and call the model

```bash
# Grant team access
curl -X POST http://localhost:8081/admin/v1/teams/<team-id>/models \
  -H 'Content-Type: application/json' \
  -d '{"model_name": "claude-opus-4"}'

# Call the model
curl http://localhost:8880/v1/chat/completions \
  -H "Authorization: Bearer <your-nexus-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model":    "claude-opus-4",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

---

## Option C — Hybrid Mode

Use hybrid when you want both simultaneously — some models registered as Public Models (controlled, named your way) and the full catalog also available dynamically.

```bash
curl -X PUT http://localhost:8081/admin/v1/providers/<provider-id> \
  -H 'Content-Type: application/json' \
  -d '{"exposure_mode": "hybrid"}'
```

After this:
- `GET /v1/models` returns both registered Public Models and virtual catalog models
- Clients can call `my-custom-claude` (Public Model) or `openrouter/anthropic/claude-opus-4` (virtual) — both work
- Public Models authorize via `team_model_permissions`; virtual models authorize via `project_provider_access`

---

## Request Flow (both modes)

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
  ┌──────────────────────┐
  │  Infrastructure      │  RPM / TPM / concurrency / daily budget / quota
  │  Policy (Redis)      │  project-layer → org-governance-layer
  └──────┬───────────────┘
         │
         ▼
  ┌──────────────────────────────┐
  │  Registry.ResolveWithFailover│  try registered Public Models first
  └──────┬───────────────────────┘
         │
         ├─── local endpoint ─────────────────► Backend.Chat()
         │
         ├─── provider endpoint (Managed) ────► Backend.Chat()
         │    ep.UpstreamBaseURL = "https://openrouter.ai"
         │    ep.UpstreamAPIKey  = "sk-or-…"
         │
         └─── registry miss → VirtualResolver (Catalog/Hybrid)
              virtualResolver.Resolve("openrouter/openai/gpt-5")
              → VirtualEndpoint → Backend.Chat()
                                        │
                                        ▼
                               Usage Tracker + Cost
```

**Nothing bypasses the pipeline.** Local and remote models (managed and virtual) go through identical auth, policy, quota, and usage steps. The only difference is how the endpoint is resolved.

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

### Wire-format differences

| Provider | Wire format | Translation needed |
|---|---|---|
| OpenAI | OpenAI native | None |
| Anthropic | Anthropic Messages API | Request + response translated; SSE translated chunk-by-chunk |
| Google Gemini | OpenAI-compat at `/v1beta/openai/` | Path rewrite only |
| Azure OpenAI | Azure REST + deployment URL | URL pattern + `api-key` header |
| OpenRouter | OpenAI-compat at `/api/v1/` | Path rewrite only |
| Groq | OpenAI-compat at `/openai/v1/` | Path rewrite only |
| Together / Mistral / Cohere / DeepSeek | OpenAI-compat at `/v1/` | None |

---

## upstream_base_url per provider

Each provider backend appends its own path to `upstream_base_url`. Use the root domain — **do not include the API path**.

| Provider | `upstream_base_url` | Final URL constructed internally |
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
> The backend strips and re-appends the path internally, resulting in a double path that returns
> a 404 HTML page. Always use the root domain: `https://openrouter.ai`.

---

## What providers skip

| Concern | Local model | Provider model |
|---|---|---|
| EnsureRunning / cold-start | ✓ | ✗ skipped |
| Scheduler / placement | ✓ | ✗ skipped |
| Container lifecycle (start/stop/upgrade) | ✓ | ✗ skipped |
| agent_runtime rows | ✓ | ✗ none created |
| GPU inventory | ✓ | ✗ not applicable |

| Concern | Local model | Provider model |
|---|---|---|
| Auth | ✓ | ✓ identical |
| Gateway policy | ✓ | ✓ identical |
| Capability validation | ✓ | ✓ identical |
| Infrastructure policy (RPM/TPM/quota) | ✓ | ✓ identical |
| Prompt policy | ✓ | ✓ identical |
| Usage tracking | ✓ | ✓ identical |
| Cost tracking | ✓ | ✓ identical |
| Health checking | ✓ | ✓ via UpstreamBaseURL |

---

## Provider catalog sync details

After sync, `provider_remote_models` stores rich metadata for each model:

| Column | Description |
|--------|-------------|
| `display_name` | Human-readable model name from provider |
| `description` | Provider description |
| `context_length` | Max context window tokens |
| `max_output_tokens` | Max completion tokens |
| `provider_input_cost` | Per-token input cost (converted to per-1M) |
| `provider_output_cost` | Per-token output cost |
| `supports_streaming` | SSE streaming (always true for chat models) |
| `supports_tools` | Tool / function calling |
| `supports_vision` | Image input |
| `supports_audio` | Audio input |
| `supports_embeddings` | Embedding endpoint |
| `supports_reasoning` | Extended thinking / CoT |
| `supports_json_mode` | Structured JSON output |

Capability flags are derived from the provider's structured metadata on first sync (OpenRouter returns architecture + supported_parameters). They are **never overwritten on re-sync** — use `PUT /admin/v1/providers/:id/models/:model_id` to update them manually if needed.

---

## Outbound proxy

To route provider traffic through an outbound proxy (e.g. for network isolation or corporate egress):

```bash
# Set at provider level (applies to all models from this provider)
curl -X PUT http://localhost:8081/admin/v1/providers/<provider-id> \
  -H 'Content-Type: application/json' \
  -d '{"proxy_url": "socks5://192.168.0.207:3315"}'

# Set at model level (Managed mode only)
curl -X PUT http://localhost:8081/admin/v1/models/<model-id>/transport \
  -H 'Content-Type: application/json' \
  -d '{"proxy_url": "socks5://192.168.0.207:3315"}'
```

Supported proxy schemes: `http://`, `https://`, `socks5://`. Credentials can be embedded: `socks5://user:pass@host:port`. Send `""` to remove and connect directly.

Transport is fully isolated per provider — changing OpenRouter's proxy never affects OpenAI or any other provider. The HTTP client is rebuilt immediately after the update.

---

## API key rotation

```bash
# Managed mode (model-level key)
curl -X PUT http://localhost:8081/admin/v1/models/<model-id>/upstream \
  -H 'Content-Type: application/json' \
  -d '{"upstream_api_key": "sk-or-v1-new-key"}'

# Catalog/Hybrid mode (provider-level key, applies to all virtual models)
curl -X PUT http://localhost:8081/admin/v1/providers/<provider-id> \
  -H 'Content-Type: application/json' \
  -d '{"api_key": "sk-or-v1-new-key"}'
```

Zero downtime — in-flight requests use the old key; new requests after the reload use the new key.

---

## Metrics

All provider metrics are in the `nexus_provider_*` namespace:

| Metric | Type | Description |
|---|---|---|
| `nexus_provider_requests_total` | Counter | Requests by provider/model/status |
| `nexus_provider_latency_seconds` | Histogram | End-to-end round-trip |
| `nexus_provider_cost_total` | Counter | Cumulative USD cost |
| `nexus_provider_failures_total` | Counter | Failures by type |
| `nexus_provider_tokens_total` | Counter | Tokens by type (input/output/cached/reasoning) |
| `nexus_provider_health_status` | Gauge | 1=healthy, 0=down per provider/model |
| `nexus_provider_ttft_seconds` | Histogram | Time to first streaming token |

---

## Quick reference: choosing a mode

| Situation | Mode | How models are called |
|-----------|------|-----------------------|
| You want control over every available model | **Managed** | `my-approved-model` |
| You want OpenRouter-style access to everything | **Catalog** | `openrouter/openai/gpt-5` |
| Some models need approval, rest are open | **Hybrid** | Both: `approved-model` and `openrouter/openai/gpt-5` |
| You need per-team model ACLs | **Managed** | `team_model_permissions` |
| You need per-project provider ACLs with rate limits | **Catalog** | `project_provider_access` |

For more details on exposure modes, see [docs/28-provider-exposure-modes.md](28-provider-exposure-modes.md).

---

## Invariants

1. **Single registry.** `runtime.Registry` is the sole source of truth for all models.
2. **Identical routing.** `proxy.Handler.ChatCompletions` has zero provider-specific branches.
3. **No policy bypass.** Every request traverses Auth → GatewayPolicy → Alias → CapabilityCheck → InfraPolicy → PromptPolicy → Resolve → Backend.
4. **No lifecycle for providers.** `IsProviderBackend()` gates all container lifecycle calls.
5. **Stable client API.** Clients always call `/v1/chat/completions`. The `model` field in the response always echoes the NexusLLM model name, never the upstream provider's internal ID.
