# 28 — Provider Exposure Modes

NexusLLM supports three modes for surfacing cloud provider models to gateway
clients. The mode is set per-provider and has no effect on any other component
of the system — authentication, rate limiting, quota, audit, usage accounting,
and streaming all behave identically regardless of which mode is active.

---

## The Three Modes

### Managed (default)

The administrator explicitly registers each cloud model as a **Public Model**.
Only registered models appear in `GET /v1/models` or are callable by clients.
Authorization is through `team_model_permissions`, the same table used for
local models.

```
Provider → Catalog Sync → Admin registers model → Public Model
                                                        ↓
                                              team_model_permissions
                                                        ↓
                                                     Gateway
                                                        ↓
                                                Cloud Provider API
```

This is correct for environments where every model deployment is a deliberate
decision. It remains the default and is unchanged by migration 050.

---

### Catalog

The provider catalogue is surfaced directly as **virtual models**. No Public
Model rows are required. A provider with 600 models immediately exposes all
600 (subject to exposure rules) without any registration step.

Virtual model names take the form `<prefix>/<provider_model_id>`, where
`prefix` is the provider's `catalog_expose_prefix` field (defaults to the
provider name).

```
openrouter/openai/gpt-5
openrouter/anthropic/claude-opus-4
openrouter/google/gemini-2.5-pro
```

Authorization moves from the model level to the **provider level**: projects
are granted access to an entire provider catalogue via
`project_provider_access`, optionally narrowed with glob prefix patterns.

```
Provider → Catalog Sync → Virtual Model Resolver
                                    ↓
                         project_provider_access
                                    ↓
                              Policy Engine
                         (RPM / TPM / quota — unchanged)
                                    ↓
                               Gateway
                                    ↓
                          Cloud Provider API
```

**Nothing else changes.** Rate limits, quota, usage recording, prompt policy,
streaming, and metrics all run identically.

---

### Hybrid

Both mechanisms run simultaneously. A provider in Hybrid mode:

- Exposes its catalogue as virtual models (Catalog path)
- Still allows any of its models to also be registered as Public Models
  (Managed path)

This is useful when an organization wants most models available dynamically
but needs a handful under explicit team-permission control, or when migrating
from Managed to Catalog incrementally.

---

## Choosing a Mode

| Situation | Recommended mode |
|-----------|-----------------|
| Enterprise — every model is an approved deployment | **Managed** |
| OpenRouter-style — expose everything, let projects self-serve | **Catalog** |
| Mixed — some models controlled, rest open | **Hybrid** |
| Migration — moving from Managed to Catalog without downtime | **Hybrid** → **Catalog** |

---

## Setting the Exposure Mode

### On create

```http
POST /admin/v1/providers
{
  "name":          "openrouter",
  "display_name":  "OpenRouter",
  "backend_type":  "openrouter_provider",
  "base_url":      "https://openrouter.ai",
  "api_key":       "sk-or-...",
  "exposure_mode": "catalog",
  "catalog_sync_enabled":  true,
  "catalog_sync_interval": 3600,
  "catalog_expose_prefix": "openrouter"
}
```

### On update

```http
PUT /admin/v1/providers/:id
{ "exposure_mode": "hybrid" }
```

The DB trigger (`trg_sync_catalog_direct_expose`) automatically keeps
`catalog_direct_expose` in sync, so older code paths that read that column
continue to work without change.

Valid values: `managed` · `catalog` · `hybrid`

---

## Project Provider Access (Catalog / Hybrid)

In Catalog and Hybrid mode, authorization is granted at the **project** level,
not the individual model level. A project that has access to OpenRouter can
call any model from that provider subject to its prefix rules.

### Grant access to all models from a provider

```http
POST /admin/v1/projects/:project_id/provider-access
{
  "provider_id": "uuid-of-openrouter-provider"
}
```

The project can now call any virtual model from OpenRouter:

```
openrouter/openai/gpt-5
openrouter/anthropic/claude-opus-4
...
```

### Grant access with prefix restrictions

```http
POST /admin/v1/projects/:project_id/provider-access
{
  "provider_id":      "uuid",
  "allowed_prefixes": ["openrouter/openai/*", "openrouter/anthropic/*"],
  "denied_prefixes":  ["openrouter/openai/gpt-4-*"]
}
```

Evaluation order (deny wins):

1. If the model name matches any `denied_prefixes` pattern → **blocked**
2. If `allowed_prefixes` is empty → **allowed** (all models from this provider)
3. If the model name matches any `allowed_prefixes` pattern → **allowed**
4. Default → **blocked**

Pattern syntax uses `path.Match` glob (`*` matches within one path segment;
`openrouter/openai/*` matches `openrouter/openai/gpt-5` but not
`openrouter/openai/fine-tuned/v2`).

### Manage existing grants

```http
GET    /admin/v1/projects/:project_id/provider-access
PUT    /admin/v1/projects/:project_id/provider-access/:provider_id
DELETE /admin/v1/projects/:project_id/provider-access/:provider_id
```

### Redis propagation

Access grants are pushed into Redis on gateway startup and every 60 seconds
by the `seedProjectProviderAccess` reload function. Changes are live within
≤ 60 seconds. To force immediate propagation, restart the gateway process.

The Redis key layout:

```
nexus:project:<project_id>:vproviders   (Hash)
  field = <provider_id>
  value = JSON { "prefix": "openrouter", "allowed": [...], "denied": [...] }
```

---

## How `GET /v1/models` Works per Mode

The response merges two lists, deduplicated:

**Path 1 — Public Models** (always, for all callers with team permissions)
Every model the caller's team has been granted via `team_model_permissions`,
filtered to those currently routable in the registry. This is the existing,
unchanged behaviour.

**Path 2 — Virtual catalog models** (Catalog / Hybrid, project-scoped callers only)
If the request carries a project context (API key scoped to a project, or
`X-Nexus-Project` header), virtual models from providers the project has
access to are appended. Models already in Path 1 are not duplicated.

A caller without a project context only sees Path 1, even if the provider
is in Catalog mode. This ensures legacy team-only API keys are unaffected.

---

## How `GET /v1/models/:model_id` Works

Resolution order:

1. Check `team_model_permissions` (Public Model path) — return if found
2. Check virtual resolver + project provider access (Catalog path) — return
   if found and project has a matching grant
3. Return 404

---

## Rate Limits in Catalog Mode

Rate limits are **project-based** and apply identically across all models from
all providers the project has access to. The budget is shared.

```
Project A — 100 RPM, 500,000 TPM

openrouter/openai/gpt-5       ─┐
openrouter/anthropic/claude-4  ├─ all consume the SAME project budget
openrouter/google/gemini-pro  ─┘
```

The policy engine `Evaluate()` enforces:

| Limit | Redis key |
|-------|-----------|
| RPM | `nexus:project:<id>:rpm` |
| TPM | `nexus:project:<id>:tpm` |
| Daily token budget | `nexus:project:<id>:daily:<date>` |
| Monthly token budget | `nexus:project:<id>:monthly:<YYYY-MM>` |
| Concurrency | `nexus:project:<id>:inflight` |

None of these keys change between Managed and Catalog mode.

---

## Catalog Metadata

`provider_remote_models` stores the full capability surface of each model,
expanded in migration 050:

| Column | Type | Description |
|--------|------|-------------|
| `service_type` | text | `chat` · `embedding` · `speech` · `image` · `rerank` · `ocr` |
| `context_length` | int | Max context window tokens |
| `max_output_tokens` | int | Max completion tokens |
| `supports_streaming` | bool | SSE streaming |
| `supports_tools` | bool | Tool / function calling |
| `supports_vision` | bool | Image input |
| `supports_audio` | bool | Audio input |
| `supports_embeddings` | bool | Embedding endpoint |
| `supports_reasoning` | bool | Extended thinking / chain-of-thought |
| `supports_json_mode` | bool | Structured JSON output |
| `supports_functions` | bool | Legacy function calling |
| `supports_image_gen` | bool | Image generation output |
| `supports_rerank` | bool | Cross-encoder rerank |
| `supports_ocr` | bool | OCR |
| `supports_speech` | bool | TTS / speech synthesis |
| `provider_input_cost` | numeric | Provider-reported input cost per 1M tokens |
| `provider_output_cost` | numeric | Provider-reported output cost per 1M tokens |
| `provider_description` | text | Human-readable description from provider |

Capability flags default to `false` on first sync and are **never overwritten
on re-sync**. Update them via:

```http
PUT /admin/v1/providers/:id/models/:model_id
{ "supports_tools": true, "supports_vision": true }
```

---

## Backward Compatibility

- `exposure_mode` defaults to `managed` for all existing providers.
- `catalog_direct_expose` is kept and kept in sync by a DB trigger. Existing
  installations that used `catalog_direct_expose=TRUE` are automatically
  promoted to `exposure_mode='catalog'` when migration 050 runs.
- All existing Public Models, team permissions, and policy configurations are
  unchanged.
- Team-only API keys (no project context) are entirely unaffected — they only
  ever see Public Models via Path 1.

---

## Full Request Flow (Catalog Mode)

```
Client → POST /v1/chat/completions  { model: "openrouter/openai/gpt-5" }
           │
           ▼
       Auth middleware
       (API key → TeamClaims with ProjectID)
           │
           ▼
       Alias resolution (no-op for virtual names)
           │
           ▼
       Capability validation (uses virtual resolver caps)
           │
           ▼
       Gateway policy (temperature cap, tool restrictions)
           │
           ▼
       Policy Engine — Evaluate()
         Step 0: model ACL
           Path A: nexus:org:<id>:models SISMEMBER → miss
           Path B: nexus:project:<id>:vproviders HGETALL
                   → entry for openrouter provider found
                   → "openrouter/" prefix matches → ALLOWED
         Step 1: project RPM / TPM / budget / concurrency
         Step 2: org governance (enabled, monthly budget, GPU pool)
           │
           ▼
       Prompt policy (system injection, PII filter)
           │
           ▼
       registry.ResolveWithFailover() → miss (not a Public Model)
           │
           ▼
       virtualResolver.Resolve("openrouter/openai/gpt-5")
         → VirtualEndpoint { BackendType: openrouter_provider,
                             UpstreamBaseURL: "https://openrouter.ai",
                             UpstreamAPIKey:  "sk-or-...",
                             UpstreamModelName: "openai/gpt-5" }
           │
           ▼
       BuildProviderClient(transport) → per-provider isolated *http.Client
       registry.CacheVirtualClient(ep.ID, client)
           │
           ▼
       virtualDispatch:
         IncrementProjectInflight
         backend.Chat() → HTTP POST to OpenRouter
           │
           ▼
       Stream / sync response to client
           │
           ▼
       RecordProjectTokenUsage / RecordOrgTokenUsage
       UsageTracker.Record(provider, model, tokens, latency, cost)
       DecrementProjectInflight
```

The only difference from a Managed-mode request is steps 5a–5b (virtual
resolver instead of registry hit). Everything before and after is identical.
