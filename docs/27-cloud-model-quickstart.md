# Cloud Model Access — Operational Guide

How to go from a raw provider API key to a team making inference calls,
with full rate-limit and token-budget enforcement.

---

## The complete flow in one diagram

```
Provider API key
        │
        ▼
1. Create Provider          (Providers → + New Provider)
        │
        ▼
2. Sync Catalog              (Providers → Sync)
   provider_remote_models populated with all available model IDs
        │
        ▼
3. Register as Public Model  (Providers → Catalog → select → Register as Public Models)
   OR
   POST /admin/v1/models/catalog-alias
   OR
   POST /admin/v1/models/external  (manual, no catalog)
        │
        ▼
        models row created
        model_endpoints row created (upstream_api_key, upstream_base_url, upstream_model_name)
        registry reloaded automatically
        │
        ▼
4. Grant Team Access         (Teams → Model Access → Cloud tab → + grant)
   team_model_permissions row inserted
   Redis ACL updated (nexus:org:<id>:models)
        │
        ▼
5. Set Rate Limits & Budgets (Projects → Policy tab)
   project_policies: rpm, tpm, max_concurrent, daily/monthly token budget
        │
        ▼
6. Client calls /v1/chat/completions  {"model": "<public-name>"}
```

---

## Step 1 — Create a Provider

A Provider holds the credentials and connection config for one cloud service.
All models from the same provider share these settings.

**Web UI:** Providers → + New Provider

**API:**
```bash
curl -X POST http://admin:8081/admin/v1/providers \
  -H "Content-Type: application/json" \
  -d '{
    "name":         "openrouter-main",
    "display_name": "OpenRouter",
    "backend_type": "openrouter_provider",
    "base_url":     "https://openrouter.ai",
    "api_key":      "sk-or-v1-..."
  }'
```

`base_url` is always the root domain — never include the API path:

| Provider | `backend_type` | `base_url` |
|---|---|---|
| OpenAI | `openai_provider` | `https://api.openai.com` |
| Anthropic | `anthropic_provider` | `https://api.anthropic.com` |
| Google Gemini | `google_provider` | `https://generativelanguage.googleapis.com` |
| Azure OpenAI | `azure_openai_provider` | `https://YOUR_RESOURCE.openai.azure.com` |
| OpenRouter | `openrouter_provider` | `https://openrouter.ai` |
| Groq | `groq_provider` | `https://api.groq.com` |
| Together | `together_provider` | `https://api.together.xyz` |
| Mistral | `mistral_provider` | `https://api.mistral.ai` |
| Cohere | `cohere_provider` | `https://api.cohere.com` |
| DeepSeek | `deepseek_provider` | `https://api.deepseek.com` |

The provider `name` is a slug used internally. It does not appear in client-facing
model names unless you use it as a catalog expose prefix.

---

## Step 2 — Sync the Catalog

Fetches the provider's `/models` endpoint and populates `provider_remote_models`.
This is the source of truth for what models the provider offers.

**Web UI:** Providers → [provider] → Sync Catalog button

**API:**
```bash
curl -X POST http://admin:8081/admin/v1/providers/PROVIDER_ID/sync
```

After sync, browse the catalog:

```bash
curl "http://admin:8081/admin/v1/providers/PROVIDER_ID/catalog?per_page=20"
```

Syncs can be scheduled automatically — set `catalog_sync_enabled: true` and
`catalog_sync_interval` (seconds) on the provider.

---

## Step 3 — Register as a Public Model

A Public Model is what clients use. It has a public name (e.g. `gpt-oss-20b`),
maps to a provider model ID internally, and appears in `/v1/models`.

**The public name can be anything.** `company-gpt`, `production-ai`, `gpt-oss-20b`.
Clients only ever see this name. The provider's internal model ID (`openai/gpt-oss-20b`)
is kept internal.

### Option A — From the catalog (recommended)

**Web UI:** Providers → [provider] → Catalog tab → select rows → Register as Public Models

This opens an inline form where you set the public name for each selected model.

**API:**
```bash
curl -X POST http://admin:8081/admin/v1/providers/PROVIDER_ID/register-models \
  -H "Content-Type: application/json" \
  -d '{
    "models": [
      {
        "public_name":       "gpt-oss-20b",
        "provider_model_id": "openai/gpt-oss-20b",
        "service_type":      "CHAT"
      },
      {
        "public_name":       "company-gpt",
        "provider_model_id": "anthropic/claude-3-5-sonnet-20241022",
        "service_type":      "CHAT"
      }
    ]
  }'
```

Response:
```json
{
  "created": 2,
  "total":   2,
  "results": [
    { "public_name": "gpt-oss-20b",  "model_id": "...", "endpoint_id": "..." },
    { "public_name": "company-gpt",  "model_id": "...", "endpoint_id": "..." }
  ]
}
```

### Option B — Named alias (same as A, single model)

```bash
curl -X POST http://admin:8081/admin/v1/models/catalog-alias \
  -H "Content-Type: application/json" \
  -d '{
    "name":              "gpt-oss-20b",
    "provider_id":       "PROVIDER_UUID",
    "provider_model_id": "openai/gpt-oss-20b",
    "service_type":      "CHAT"
  }'
```

### Option C — Manual (no catalog)

For models not in any catalog, or when you want full control:

```bash
curl -X POST http://admin:8081/admin/v1/models/external \
  -H "Content-Type: application/json" \
  -d '{
    "name":                  "gpt-oss-20b",
    "provider_backend_type": "openrouter_provider",
    "upstream_api_key":      "sk-or-v1-...",
    "upstream_base_url":     "https://openrouter.ai",
    "upstream_model_name":   "openai/gpt-oss-20b",
    "service_type":          "CHAT"
  }'
```

---

## Step 4 — Grant Team Access

After registration the model exists in the registry, but no team can call it yet.
Authorization is model-centric: every team needs an explicit grant, identically
for local and cloud models.

**Web UI:** Teams → [team] → Model Access → Cloud tab → click `+ gpt-oss-20b`

**API:**
```bash
curl -X POST http://admin:8081/admin/v1/teams/TEAM_ID/models \
  -H "Content-Type: application/json" \
  -d '{ "model_name": "gpt-oss-20b" }'
```

The Redis ACL set `nexus:org:<OrgID>:models` is updated immediately. No restart needed.
API keys belonging to this team will pass the policy Step-0 check for `gpt-oss-20b`
on the next request.

To revoke:
```bash
curl -X DELETE http://admin:8081/admin/v1/teams/TEAM_ID/models/gpt-oss-20b
```

---

## Step 5 — Set Rate Limits and Token Budgets

Rate limits and token budgets belong to **Projects**, not teams. This matches the
execution model: projects carry priority weight, quota, and scheduling context.

**Web UI:** Projects → [project] → Policy tab

| Field | What it controls |
|---|---|
| RPM | Max requests per minute from this project |
| TPM | Max estimated input tokens per minute |
| Max concurrent | Max simultaneous in-flight requests |
| Max context tokens | Request rejected if estimated input exceeds this |
| Daily token budget | Total tokens (input+output) per calendar day |
| Monthly token budget | Total tokens per calendar month |

**API — create/update project policy:**
```bash
curl -X PUT http://admin:8081/admin/v1/projects/PROJECT_ID/policy \
  -H "Content-Type: application/json" \
  -d '{
    "rpm":                  60,
    "tpm":                  100000,
    "max_concurrent":       10,
    "max_context_tokens":   32000,
    "daily_token_budget":   5000000,
    "monthly_token_budget": 100000000
  }'
```

**Check live quota status:**
```bash
curl http://admin:8081/admin/v1/projects/PROJECT_ID/quota
```

```json
{
  "project_id":           "...",
  "daily_tokens_used":    142000,
  "monthly_tokens_used":  2400000,
  "tpm_current":          3200,
  "inflight":             2
}
```

Policy changes take effect on the next request — they are written to Redis immediately
with no gateway restart.

---

## Step 6 — Make an inference call

```bash
curl -X POST http://gateway:8880/v1/chat/completions \
  -H "Authorization: Bearer nxs_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model":      "gpt-oss-20b",
    "messages":   [{"role": "user", "content": "Hi"}],
    "max_tokens": 50
  }'
```

The `model` field in the response always echoes the public name (`gpt-oss-20b`),
never the provider's internal ID (`openai/gpt-oss-20b`).

---

## Authorization pipeline — what runs for every cloud model request

The gateway executes these stages in order. Cloud models go through every stage,
identical to local models. Backend type is irrelevant to authorization.

```
1. Authentication      API key → OrgID / TeamID / ProjectID / Permissions
2. Alias resolution    optional: "gpt4" → "gpt-oss-20b"
3. Capability check    does "gpt-oss-20b" support "chat"? (from capabilities column)
4. Gateway policy      temperature cap, tool restrictions, model allow/deny list
5. Model ACL           is "gpt-oss-20b" in nexus:org:<OrgID>:models? (Redis)
6. Project policy      RPM, TPM, concurrency, daily/monthly budget (Redis)
7. Org governance      org disabled? org monthly budget exceeded?
8. Prompt policy       system prompt injection, PII filter, content rules
9. Endpoint resolve    registry.ResolveWithFailover("gpt-oss-20b") → endpoint
                       endpoint.UpstreamBaseURL = "https://openrouter.ai"
                       endpoint.UpstreamModelName = "openai/gpt-oss-20b"
10. Backend dispatch   openrouterProviderBackend.Chat(...)
                       POST https://openrouter.ai/api/v1/chat/completions
                       Authorization: Bearer sk-or-v1-...
11. Usage recording    tokens, latency, cost → usage_events + Redis counters
```

Stages 1–8 are identical for every model type. The gateway never checks `backend_type`
during authorization.

---

## What the gateway never does for cloud models

| Operation | Local model | Cloud model |
|---|---|---|
| EnsureRunning (cold-start) | ✓ on registry miss | ✗ never |
| Scheduler / placement | ✓ | ✗ never |
| Container start/stop | ✓ | ✗ never |
| agent_runtimes rows | ✓ | ✗ none |
| GPU inventory check | ✓ | ✗ not applicable |

Everything else (auth, policy, quota, prompt policy, usage, aliases, streaming,
capability validation, health watcher) runs identically.

---

## Common mistakes

### Wrong `upstream_base_url`

Do **not** include the API path. The backend appends its own path.

```
❌ https://openrouter.ai/api/v1        → produces /api/api/v1/chat/completions
✓  https://openrouter.ai              → produces /api/v1/chat/completions
```

### Using the provider model ID as the client model name

The client uses the **public name** you assigned, not the provider's internal ID.

```
❌ "model": "openai/gpt-oss-20b"   → 404 model_not_found (unless you named it that)
✓  "model": "gpt-oss-20b"          → works if you registered it with that public name
```

### Model registered but client gets `model_not_allowed`

The team wasn't granted access. Step 4 was skipped.

```bash
# Verify the grant exists
curl http://admin:8081/admin/v1/teams/TEAM_ID/models
```

### Model registered but `/v1/models` doesn't show it

The API key belongs to a team that hasn't been granted access. `/v1/models` only
returns models the caller's team is authorized to use.

### `model_starting` error for a cloud model

The gateway tried to cold-start the model, which means either:
- The model row in the DB has a local `backend_type` (e.g. `vllm` instead of `openrouter_provider`)
- The registry hasn't reloaded since registration — wait ≤10 seconds and retry

Check the actual backend type:
```bash
curl http://admin:8081/admin/v1/models?name=gpt-oss-20b
```

---

## Recommended approach: which registration method to use

| Situation | Method |
|---|---|
| Provider offers many models, you want to pick a few | Sync catalog → select in UI → Register as Public Models |
| You know exactly which model ID and want a clean name | `POST /models/catalog-alias` |
| Specific Azure deployment with custom URL | `POST /models/external` (manual) |
| Expose all matching models as `provider/model-id` names (no public names) | Provider → `catalog_direct_expose: true` + exposure rules |

The first three methods all create a `models` row and go through the identical
permission system. The fourth (Mode B / virtual) is for browsing; models still
need to be promoted to Public Models before teams can be granted access.

For production, the recommended approach is:

1. Create a Provider per cloud service
2. Enable catalog sync
3. Register Public Models from the catalog with meaningful business names
4. Grant team access per model
5. Set project-level rate limits and token budgets

This gives you a clean, auditable model registry where every callable model has
a known name, a known owner, and an explicit access grant.
