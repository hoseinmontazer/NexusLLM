# Provider Catalog Architecture

## Overview

This document specifies the redesign of the External / Cloud Model subsystem into a
four-layer **Provider Catalog** architecture. The goal is to support providers that
expose hundreds or thousands of models (OpenRouter, Azure, Bedrock) without creating
one NexusLLM Public Model row per remote model.

The gateway client API is unchanged. Clients always call `/v1/chat/completions`
with a NexusLLM model name. They never see provider-internal IDs.

---

## The Problem With The Current Design

The current implementation maps each remote model to one row in `models` + one row
in `model_endpoints`. For a provider like OpenRouter (500+ models) this means:

- 500+ rows in `models`
- 500+ rows in `model_endpoints`
- 500+ entries in the registry pool map
- 500+ per-endpoint `*http.Client` objects in `Registry.epClients`
- 500+ entries in the admin UI model list
- Manual re-registration every time OpenRouter adds a new model
- No way to express "expose all Anthropic models from OpenRouter"


---

## Four-Layer Architecture

```
┌──────────────────────────────────────────────────────┐
│  Layer 1 — Provider                                  │
│  credentials · proxy · transport · health · sync     │
└──────────────────────────┬───────────────────────────┘
                           │ syncs via GET /models
┌──────────────────────────▼───────────────────────────┐
│  Layer 2 — Remote Catalog                            │
│  provider_remote_models (read-only mirror)           │
│  openai/gpt-5 · anthropic/claude-sonnet-4 · …       │
└──────────────────────────┬───────────────────────────┘
                           │ evaluated after every sync
┌──────────────────────────▼───────────────────────────┐
│  Layer 3 — Exposure Rules                            │
│  allow patterns · deny patterns · capability filters │
└──────────────────────────┬───────────────────────────┘
                           │ produces
┌──────────────────────────▼───────────────────────────┐
│  Layer 4 — Public Models                             │
│  what clients see in /v1/models and can call         │
│  Mode A: explicit alias  Mode B: virtual catalog     │
└──────────────────────────────────────────────────────┘
```

**Invariant:** layers 1–3 are internal. Layer 4 is the only surface clients touch.
The gateway pipeline (auth → policy → alias → capability → quota → prompt → registry)
is identical for local and remote models.


---

## Layer 1 — Provider

### Concept

A Provider represents one cloud AI service. It is the single place that holds
everything needed to reach that service: credentials, proxy, transport settings,
health configuration, and synchronization schedule.

A Provider is **not** a model. It is a connection configuration.

### Examples

| Provider name | backend_type | base_url |
|---|---|---|
| openrouter-main | openrouter_provider | https://openrouter.ai |
| openai-production | openai_provider | https://api.openai.com |
| anthropic-eu | anthropic_provider | https://api.anthropic.com |
| azure-eastus | azure_openai_provider | https://myresource.openai.azure.com |
| internal-vllm | openai_compat | http://gpu-cluster:8000 |

### Database schema — `providers` table (migration 047)

```sql
CREATE TABLE providers (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Identity
    name              TEXT NOT NULL UNIQUE,          -- human-readable slug
    display_name      TEXT NOT NULL,
    backend_type      TEXT NOT NULL,                 -- BackendType constant
    base_url          TEXT NOT NULL,

    -- Credentials
    api_key           TEXT NOT NULL DEFAULT '',      -- encrypted at rest
    api_key_header    TEXT NOT NULL DEFAULT 'Authorization', -- override for Azure

    -- Catalog sync
    catalog_sync_enabled   BOOLEAN NOT NULL DEFAULT FALSE,
    catalog_sync_interval  INT     NOT NULL DEFAULT 3600,   -- seconds
    catalog_last_synced_at TIMESTAMPTZ,
    catalog_model_count    INT     NOT NULL DEFAULT 0,
    catalog_sync_status    TEXT    NOT NULL DEFAULT 'never', -- never|ok|error|syncing
    catalog_sync_error     TEXT,

    -- Transport (same fields as migration 046 per-endpoint columns)
    proxy_url                        TEXT,
    tls_insecure_skip_verify         BOOLEAN NOT NULL DEFAULT FALSE,
    tls_root_ca_pem                  TEXT,
    connect_timeout_seconds          INT     NOT NULL DEFAULT 0,
    read_timeout_seconds             INT     NOT NULL DEFAULT 0,
    idle_conn_timeout_seconds        INT     NOT NULL DEFAULT 0,
    response_header_timeout_seconds  INT     NOT NULL DEFAULT 0,
    max_idle_conns_per_host          INT     NOT NULL DEFAULT 0,
    max_conns_per_host               INT     NOT NULL DEFAULT 0,
    disable_http2                    BOOLEAN NOT NULL DEFAULT FALSE,

    -- Retry / timeout
    request_timeout_seconds INT NOT NULL DEFAULT 120,
    max_retries             INT NOT NULL DEFAULT 2,

    -- Status
    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    health       TEXT    NOT NULL DEFAULT 'unknown', -- healthy|degraded|down|unknown
    last_health_check TIMESTAMPTZ,

    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```


### Go type

```go
// internal/catalog/provider.go

type Provider struct {
    ID          string
    Name        string
    DisplayName string
    BackendType runtime.BackendType
    BaseURL     string
    APIKey      string

    CatalogSyncEnabled  bool
    CatalogSyncInterval time.Duration

    Transport runtime.ProviderTransportConfig
    Timeout   time.Duration
    MaxRetries int

    Enabled bool
    Health  string
}
```

### HTTP client construction

Every provider builds its own isolated `*http.Client` via `BuildProviderClient(p.Transport)`.
The same function already used per-endpoint in migration 046. No shared mutable transports.
No env-var proxy fallback.

---

## Layer 2 — Remote Catalog

### Concept

Each provider with `catalog_sync_enabled = TRUE` maintains a local mirror of the models
it exposes. This table is **read-only from the perspective of administrators**. It is
written only by the catalog synchronizer background job.

### Database schema — `provider_remote_models` table (migration 047)

```sql
CREATE TABLE provider_remote_models (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id       UUID NOT NULL REFERENCES providers(id) ON DELETE CASCADE,

    -- Provider's own identifier (e.g. "openai/gpt-5", "claude-sonnet-4-5")
    provider_model_id TEXT NOT NULL,

    -- Metadata from provider's /models response
    display_name      TEXT NOT NULL DEFAULT '',
    description       TEXT NOT NULL DEFAULT '',
    context_length    INT,
    input_cost_per_1m NUMERIC,    -- USD
    output_cost_per_1m NUMERIC,   -- USD

    -- Capability flags (derived from provider metadata)
    supports_streaming   BOOLEAN NOT NULL DEFAULT TRUE,
    supports_tools       BOOLEAN NOT NULL DEFAULT FALSE,
    supports_vision      BOOLEAN NOT NULL DEFAULT FALSE,
    supports_audio       BOOLEAN NOT NULL DEFAULT FALSE,
    supports_embeddings  BOOLEAN NOT NULL DEFAULT FALSE,
    supports_reasoning   BOOLEAN NOT NULL DEFAULT FALSE,
    supports_images      BOOLEAN NOT NULL DEFAULT FALSE, -- image generation

    -- Tags extracted from model ID (free, preview, beta, instruct, …)
    tags TEXT[] NOT NULL DEFAULT '{}',

    -- Raw metadata from provider (preserved for future use)
    provider_metadata JSONB NOT NULL DEFAULT '{}',

    -- Lifecycle
    enabled          BOOLEAN      NOT NULL DEFAULT TRUE,
    first_seen_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_seen_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    removed_at       TIMESTAMPTZ, -- set when provider stops returning this model

    UNIQUE (provider_id, provider_model_id)
);

CREATE INDEX idx_prm_provider     ON provider_remote_models(provider_id);
CREATE INDEX idx_prm_enabled      ON provider_remote_models(provider_id, enabled);
CREATE INDEX idx_prm_capabilities ON provider_remote_models
    USING gin(tags) WHERE enabled = TRUE;
```


### Synchronizer

```
internal/catalog/syncer.go
```

The synchronizer runs as a background goroutine per provider (or as a single goroutine
that iterates over providers on a scheduler tick).

**Algorithm:**

```
for each provider where catalog_sync_enabled AND enabled:
    1. Mark provider catalog_sync_status = 'syncing'
    2. Build *http.Client via BuildProviderClient(p.Transport)
    3. Call provider's /models endpoint (OpenAI-compat GET /v1/models or equivalent)
    4. Parse response into []RemoteModel
    5. BEGIN transaction
       a. For each model in response:
          - UPSERT into provider_remote_models
            SET last_seen_at = NOW(), enabled = TRUE, display_name, context_length, …
       b. For models NOT in response but previously enabled:
          - SET enabled = FALSE, removed_at = NOW()
          (never DELETE — preserve admin configuration)
       c. UPDATE providers SET catalog_last_synced_at, catalog_model_count, catalog_sync_status = 'ok'
    6. COMMIT
    7. Trigger exposure rule evaluation for this provider
```

**Error handling:**

- Network errors: mark `catalog_sync_status = 'error'`, `catalog_sync_error = err.Error()`
- Never clear existing catalog entries on error (stale is better than empty)
- Exponential backoff: 1 min → 5 min → 15 min → 60 min cap

**Parsing provider metadata:**

OpenRouter returns `architecture`, `pricing`, `top_provider` etc. in the `/models` response.
The syncer extracts capability flags from these fields and populates tags by parsing the
model ID (`:free`, `:preview`, `:beta`, `:nitro` suffixes, `instruct`, `turbo`, etc.).

---

## Layer 3 — Exposure Rules

### Concept

Rules determine which catalog entries become visible to clients.
Rules are evaluated **after every sync** and on explicit admin request.
The result is a set of "exposed" catalog entries — either as virtual models or
as the backing for explicit Public Model aliases.

### Database schema — `provider_exposure_rules` table (migration 047)

```sql
CREATE TABLE provider_exposure_rules (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id UUID NOT NULL REFERENCES providers(id) ON DELETE CASCADE,

    -- Rule type
    rule_type   TEXT NOT NULL, -- 'allow_model' | 'allow_pattern' | 'deny_pattern'
                               -- | 'capability_filter'

    -- Pattern (for allow_pattern / deny_pattern)
    -- Supports glob: "openai/*", "*:free", "anthropic/claude-*"
    pattern     TEXT,

    -- Exact model ID (for allow_model)
    model_id    TEXT,

    -- Capability filter (for capability_filter rule_type)
    require_streaming   BOOLEAN,
    require_tools       BOOLEAN,
    require_vision      BOOLEAN,
    require_audio       BOOLEAN,
    require_embeddings  BOOLEAN,
    require_reasoning   BOOLEAN,

    -- Deny tags (comma-separated, any match = denied)
    deny_tags   TEXT[] NOT NULL DEFAULT '{}',  -- e.g. {'free','preview','beta'}

    -- Priority: lower = evaluated first. Deny rules at lower priority block allow rules.
    priority    INT NOT NULL DEFAULT 100,

    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_per_provider ON provider_exposure_rules(provider_id, enabled);
```


### Rule Evaluation Engine

```
internal/catalog/rules.go
```

```go
// RuleEngine evaluates exposure rules against a catalog entry.
// Returns true if the entry should be exposed.
func (e *RuleEngine) IsExposed(entry RemoteModel, rules []ExposureRule) bool {
    // 1. Sort rules by priority ASC
    // 2. Walk rules in order:
    //    - deny_pattern match → return false immediately
    //    - deny_tags match    → return false immediately
    // 3. Walk rules again:
    //    - allow_model exact match → return true
    //    - allow_pattern glob match → return true (if capability filter passes)
    // 4. Default → return false (deny by default)
}
```

**Glob matching:**

Use `path.Match` semantics. Examples:

| Pattern | Matches | Does not match |
|---|---|---|
| `openai/*` | `openai/gpt-5`, `openai/gpt-4o` | `anthropic/claude` |
| `*` | all models | — |
| `*:free` | `openai/gpt-4o:free` | `openai/gpt-4o` |
| `anthropic/claude-*` | `anthropic/claude-sonnet-4-5` | `openai/gpt-4o` |

**Capability filter example:**

Rule: `allow_pattern = "*"`, `require_tools = TRUE`, `deny_tags = ['free','preview']`

Result: all models that support function calling, excluding free-tier and preview variants.

### Exposure result materialization

After rule evaluation the result is written to a view (not a table) for fast lookups:

```sql
CREATE OR REPLACE VIEW exposed_catalog AS
SELECT
    prm.id              AS remote_model_id,
    prm.provider_id,
    prm.provider_model_id,
    prm.display_name,
    prm.context_length,
    prm.supports_streaming,
    prm.supports_tools,
    prm.supports_vision,
    prm.supports_embeddings,
    prm.supports_reasoning,
    prm.input_cost_per_1m,
    prm.output_cost_per_1m,
    p.name              AS provider_name,
    p.backend_type,
    p.base_url,
    p.api_key,
    p.proxy_url,
    -- derived columns
    p.name || '/' || prm.provider_model_id AS virtual_model_name
FROM provider_remote_models prm
JOIN providers p ON p.id = prm.provider_id
WHERE prm.enabled = TRUE
  AND p.enabled   = TRUE
  AND catalog_exposure_rule_passes(prm.id); -- plpgsql function wrapping rule engine
```

This view is queried by the virtual model resolver (Layer 4 Mode B).


---

## Layer 4 — Public Models

### Two exposure modes

#### Mode A — Alias Mode (explicit)

Administrator manually creates a NexusLLM Public Model that is backed by a catalog entry.

```
Public Model "company-gpt"
    └── provider_id  = openrouter-main
    └── remote_model_id = openai/gpt-5
```

This creates a single row in the existing `models` table with:
- `backend_type` = the provider's `backend_type` (e.g. `openrouter_provider`)
- `provider_catalog_id` = FK to `provider_remote_models.id` (new column, migration 047)
- `provider_id` = FK to `providers.id`

The model behaves identically to the current external model approach. The `upstream_api_key`,
`upstream_base_url`, `upstream_model_name` columns on `model_endpoints` are populated from
the provider and catalog entry at registration time. The transport config comes from the
provider row, not the endpoint row.

One Public Model. One `model_endpoints` row. One entry in `Registry.epClients`.

#### Mode B — Direct Catalog Exposure (virtual)

Administrator enables `catalog_direct_expose = TRUE` on a provider (or on specific rules).

The registry loads exposed catalog entries dynamically without creating `models` rows.
The virtual models are resolved at request time by the **VirtualModelResolver**.

```
Client: POST /v1/chat/completions  {"model": "openrouter/openai/gpt-5"}
                │
                ▼
        Registry.Resolve("openrouter/openai/gpt-5")
                │
          model NOT found in pools
                │
                ▼
        VirtualModelResolver.Resolve("openrouter/openai/gpt-5")
                │
          query: SELECT * FROM exposed_catalog
                 WHERE virtual_model_name = 'openrouter/openai/gpt-5'
                │
          found: {provider_id, provider_model_id, backend_type, base_url, api_key, proxy_url}
                │
                ▼
        construct synthetic Endpoint{
            ID:              "virt:" + remoteModelID,
            BackendType:     backendType,
            UpstreamBaseURL: baseURL,
            UpstreamAPIKey:  apiKey,
            UpstreamModelName: provider_model_id,
            Transport:       BuildTransportFromProvider(provider),
        }
                │
                ▼
        Backend.Chat(ChatRequest{Client: providerClient, …})
```

**No `models` row. No `model_endpoints` row. No registry pool entry.**

The provider's `*http.Client` is cached in a separate `ProviderClientCache` keyed by
`provider_id`, not `endpoint_id`. This means all virtual models sharing a provider
reuse the same single HTTP client — correct behaviour because they share the same
proxy, TLS, and pool configuration.

```go
// internal/catalog/resolver.go

type VirtualModelResolver struct {
    db      *sqlx.DB
    clients sync.Map // provider_id → *http.Client
    log     *zap.Logger
}

func (r *VirtualModelResolver) Resolve(modelName string) (*VirtualEndpoint, error) {
    // 1. Parse prefix: "openrouter/" or "provider-name/"
    // 2. Query exposed_catalog view
    // 3. Return synthetic endpoint
}
```


---

## Registry Integration

### Current flow

```
Registry.Reload()
  → loads model_endpoints rows
  → builds Pool per model_name
  → builds *http.Client per provider endpoint
```

### New flow

```
Registry.Reload()
  → (unchanged) loads model_endpoints rows → Pool per model_name
  → (unchanged) builds *http.Client per endpoint via BuildProviderClient()
  → (NEW) loads providers rows → ProviderClientCache[provider_id] = *http.Client
  → (NEW) signals VirtualModelResolver to invalidate its cache

proxy.Handler.ChatCompletions()
  → Registry.ResolveWithFailover(modelName)
      → found in pools → return (ep, backend)      [unchanged path]
      → NOT found
          → VirtualModelResolver.Resolve(modelName)
              → found in exposed_catalog → return VirtualEndpoint
              → NOT found → 404 model not found    [unchanged error]
```

### VirtualEndpoint satisfies the same interface as Endpoint

```go
// internal/catalog/virtual_endpoint.go

// VirtualEndpoint is a synthetic Endpoint constructed from a catalog entry.
// It is never stored in a Pool. It is constructed per-request and discarded
// after the response is sent. The provider *http.Client is reused across
// all virtual endpoints for the same provider.
type VirtualEndpoint struct {
    // Mirrors the fields proxy.Handler reads from *runtime.Endpoint
    ID               string
    BackendType      runtime.BackendType
    UpstreamBaseURL  string
    UpstreamAPIKey   string
    UpstreamModelName string
    Transport        runtime.ProviderTransportConfig
    Status           runtime.HealthStatus // always StatusHealthy for virtual
}
```

### ProviderClientCache

```go
// internal/catalog/client_cache.go

// ProviderClientCache holds one *http.Client per provider, keyed by provider_id.
// The client is built once from the provider's Transport config and reused for
// all requests to all virtual models served by that provider.
// This is the catalog equivalent of Registry.epClients.
type ProviderClientCache struct {
    mu      sync.RWMutex
    clients map[string]*http.Client // provider_id → client
}

func (c *ProviderClientCache) GetOrBuild(p *Provider) (*http.Client, error) {
    c.mu.RLock()
    if cl, ok := c.clients[p.ID]; ok {
        c.mu.RUnlock()
        return cl, nil
    }
    c.mu.RUnlock()
    // build new client from p.Transport
    cl, err := runtime.BuildProviderClient(p.Transport)
    if err != nil {
        return nil, err
    }
    c.mu.Lock()
    c.clients[p.ID] = cl
    c.mu.Unlock()
    return cl, nil
}
```

**Transport isolation is preserved:** one client per provider, not one per virtual model.
Changing OpenRouter's proxy rebuilds only the OpenRouter client. Other providers are
unaffected. No environment variable fallback.

---

## Gateway Changes

### proxy/handler.go — ChatCompletions

```
Current:
    ep, backend, err := h.registry.ResolveWithFailover(req.Model, maxFailoverAttempts)
    if err != nil {
        // activator path or 503
    }

New:
    ep, backend, err := h.registry.ResolveWithFailover(req.Model, maxFailoverAttempts)
    if err != nil {
        // 1. Try VirtualModelResolver
        vep, verr := h.virtualResolver.Resolve(req.Model)
        if verr != nil {
            // activator path or 503 (unchanged)
        }
        // use vep as if it were ep
        ep = vep.AsEndpoint()
        backend = h.registry.BackendForType(vep.BackendType)
    }
```

No other changes to the pipeline. Auth, policy, quota, prompt policy, usage tracking,
streaming, and metrics are all unchanged. The virtual endpoint carries enough information
for the backend to make the upstream call.

### GET /v1/models

```
Current: returns models from Registry.ListModels() (pool keys)

New:
    models := Registry.ListModels()                   // explicit Public Models
    virtual := VirtualModelResolver.ListExposed()     // Mode B virtual models
    return deduplicated union
```

`ListExposed()` queries `exposed_catalog` and returns `virtual_model_name` values.
Virtual models are returned only if `catalog_direct_expose = TRUE` on their provider.

Clients with access to a virtual model see it in `/v1/models`. Clients without
access (team policy model allowlist) do not.

### EnsureRunning (activator)

Virtual models never trigger `EnsureRunning`. The activator is only called for local
models. The `VirtualModelResolver.Resolve()` path explicitly skips the activator.

```go
if vep != nil {
    // provider model — never cold-start
    goto dispatchVirtual
}
if h.activator != nil {
    // local model — may cold-start
}
```

---

## REST API

### Providers

```
POST   /admin/v1/providers                    Create provider
GET    /admin/v1/providers                    List providers
GET    /admin/v1/providers/:id                Get provider
PUT    /admin/v1/providers/:id                Update provider (credentials, transport, sync config)
DELETE /admin/v1/providers/:id                Delete provider (disables all its models)
POST   /admin/v1/providers/:id/sync           Trigger manual catalog sync
GET    /admin/v1/providers/:id/health         Test connectivity to provider
PUT    /admin/v1/providers/:id/transport      Update transport config (proxy, TLS, timeouts)
```

### Remote Catalog

```
GET  /admin/v1/providers/:id/catalog          List catalog entries (paginated, filterable)
GET  /admin/v1/providers/:id/catalog/:mid     Get single catalog entry
POST /admin/v1/providers/:id/catalog/search   Search with filters
```

Query params for catalog list:
- `q` — full-text search on provider_model_id and display_name
- `capability` — `chat|embedding|audio|image|vision|reasoning`
- `tag` — filter by tag (`free`, `preview`, `beta`, …)
- `exposed` — `true|false|all` (default `all`)
- `page`, `per_page`

### Exposure Rules

```
GET    /admin/v1/providers/:id/rules          List rules
POST   /admin/v1/providers/:id/rules          Create rule
PUT    /admin/v1/providers/:id/rules/:rid     Update rule
DELETE /admin/v1/providers/:id/rules/:rid     Delete rule
POST   /admin/v1/providers/:id/rules/preview  Dry-run: returns matching + blocked model counts
```

### Public Models (Mode A — Alias)

```
POST /admin/v1/models/catalog-alias
{
    "name":              "company-gpt",
    "display_name":      "Company GPT",
    "provider_id":       "uuid",
    "provider_model_id": "openai/gpt-5",
    "capabilities":      ["chat","completion"],
    "service_type":      "CHAT"
}
```

This replaces `POST /admin/v1/models/external` for catalog-backed models.
The existing `POST /admin/v1/models/external` remains for models not in any catalog
(e.g. a specific Azure deployment with a custom URL).

### Direct Catalog Exposure (Mode B toggle)

```
PUT /admin/v1/providers/:id
{
    "catalog_direct_expose": true,
    "catalog_expose_prefix": "openrouter"
}
```

`catalog_expose_prefix` controls the namespace in Mode B.
Example: `openrouter/openai/gpt-5`. If omitted, defaults to the provider `name` field.


---

## Database Migrations

### Migration 047 — providers and catalog tables

File: `migrations/047_provider_catalog.sql`

```sql
BEGIN;

-- ── Layer 1: providers ────────────────────────────────────────────────────────

CREATE TABLE providers (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name              TEXT NOT NULL UNIQUE,
    display_name      TEXT NOT NULL,
    backend_type      TEXT NOT NULL,
    base_url          TEXT NOT NULL,
    api_key           TEXT NOT NULL DEFAULT '',
    api_key_header    TEXT NOT NULL DEFAULT 'Authorization',

    catalog_sync_enabled   BOOLEAN     NOT NULL DEFAULT FALSE,
    catalog_sync_interval  INT         NOT NULL DEFAULT 3600,
    catalog_direct_expose  BOOLEAN     NOT NULL DEFAULT FALSE,
    catalog_expose_prefix  TEXT        NOT NULL DEFAULT '',
    catalog_last_synced_at TIMESTAMPTZ,
    catalog_model_count    INT         NOT NULL DEFAULT 0,
    catalog_sync_status    TEXT        NOT NULL DEFAULT 'never',
    catalog_sync_error     TEXT,

    proxy_url                        TEXT,
    tls_insecure_skip_verify         BOOLEAN NOT NULL DEFAULT FALSE,
    tls_root_ca_pem                  TEXT,
    connect_timeout_seconds          INT     NOT NULL DEFAULT 0,
    read_timeout_seconds             INT     NOT NULL DEFAULT 0,
    idle_conn_timeout_seconds        INT     NOT NULL DEFAULT 0,
    response_header_timeout_seconds  INT     NOT NULL DEFAULT 0,
    max_idle_conns_per_host          INT     NOT NULL DEFAULT 0,
    max_conns_per_host               INT     NOT NULL DEFAULT 0,
    disable_http2                    BOOLEAN NOT NULL DEFAULT FALSE,

    request_timeout_seconds INT     NOT NULL DEFAULT 120,
    max_retries             INT     NOT NULL DEFAULT 2,

    enabled           BOOLEAN     NOT NULL DEFAULT TRUE,
    health            TEXT        NOT NULL DEFAULT 'unknown',
    last_health_check TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── Layer 2: remote catalog ───────────────────────────────────────────────────

CREATE TABLE provider_remote_models (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id       UUID NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    provider_model_id TEXT NOT NULL,
    display_name      TEXT NOT NULL DEFAULT '',
    description       TEXT NOT NULL DEFAULT '',
    context_length    INT,
    input_cost_per_1m  NUMERIC,
    output_cost_per_1m NUMERIC,

    supports_streaming   BOOLEAN NOT NULL DEFAULT TRUE,
    supports_tools       BOOLEAN NOT NULL DEFAULT FALSE,
    supports_vision      BOOLEAN NOT NULL DEFAULT FALSE,
    supports_audio       BOOLEAN NOT NULL DEFAULT FALSE,
    supports_embeddings  BOOLEAN NOT NULL DEFAULT FALSE,
    supports_reasoning   BOOLEAN NOT NULL DEFAULT FALSE,
    supports_images      BOOLEAN NOT NULL DEFAULT FALSE,

    tags              TEXT[]      NOT NULL DEFAULT '{}',
    provider_metadata JSONB       NOT NULL DEFAULT '{}',

    enabled           BOOLEAN     NOT NULL DEFAULT TRUE,
    first_seen_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    removed_at        TIMESTAMPTZ,

    UNIQUE (provider_id, provider_model_id)
);

CREATE INDEX idx_prm_provider     ON provider_remote_models(provider_id);
CREATE INDEX idx_prm_enabled      ON provider_remote_models(provider_id, enabled);
CREATE INDEX idx_prm_tags         ON provider_remote_models USING gin(tags);
CREATE INDEX idx_prm_capabilities ON provider_remote_models(provider_id, supports_tools,
    supports_vision, supports_audio, supports_embeddings, supports_reasoning);

-- ── Layer 3: exposure rules ───────────────────────────────────────────────────

CREATE TABLE provider_exposure_rules (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id UUID NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    rule_type   TEXT NOT NULL CHECK (rule_type IN
                    ('allow_model','allow_pattern','deny_pattern','capability_filter')),
    pattern     TEXT,
    model_id    TEXT,
    require_streaming   BOOLEAN,
    require_tools       BOOLEAN,
    require_vision      BOOLEAN,
    require_audio       BOOLEAN,
    require_embeddings  BOOLEAN,
    require_reasoning   BOOLEAN,
    deny_tags   TEXT[]  NOT NULL DEFAULT '{}',
    priority    INT     NOT NULL DEFAULT 100,
    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_per_provider ON provider_exposure_rules(provider_id, enabled, priority);

-- ── Layer 4: link Public Models to catalog entries ────────────────────────────

ALTER TABLE models
    ADD COLUMN IF NOT EXISTS provider_id          UUID REFERENCES providers(id),
    ADD COLUMN IF NOT EXISTS provider_catalog_id  UUID REFERENCES provider_remote_models(id);

COMMENT ON COLUMN models.provider_id IS
    'FK to providers table. Set for catalog-backed Public Models (Mode A). NULL for local models.';
COMMENT ON COLUMN models.provider_catalog_id IS
    'FK to provider_remote_models. Set for catalog-backed aliases. NULL otherwise.';

-- Seed: migrate existing external models to have provider_id where possible.
-- This is advisory — existing models continue working via model_endpoints as before.

COMMIT;
```


---

## Go Package Structure

```
internal/
  catalog/
    provider.go          Provider struct, DB load/save
    syncer.go            CatalogSyncer: background sync goroutine per provider
    rules.go             RuleEngine: glob matching, capability filtering
    resolver.go          VirtualModelResolver: resolves Mode B model names
    client_cache.go      ProviderClientCache: one *http.Client per provider
    virtual_endpoint.go  VirtualEndpoint: synthetic endpoint for pipeline
    scheduler.go         SyncScheduler: manages per-provider sync timers

internal/admin/handlers/
    catalog.go           HTTP handlers: providers, catalog, rules, catalog-alias

cmd/gateway/main.go
    — wire VirtualModelResolver into proxy.Handler
    — wire SyncScheduler into background goroutines

cmd/admin/main.go
    — register catalog handler routes
    — start SyncScheduler
```

### Key interfaces

```go
// internal/catalog/resolver.go

type Resolver interface {
    // Resolve returns a VirtualEndpoint for a Mode B model name.
    // Returns (nil, nil) when the name is not in the exposed catalog.
    // Returns (nil, err) on DB error.
    Resolve(ctx context.Context, modelName string) (*VirtualEndpoint, error)

    // ListExposed returns all virtual model names currently exposed.
    // Used by GET /v1/models.
    ListExposed(ctx context.Context) ([]string, error)

    // Invalidate clears the internal cache. Called after every sync.
    Invalidate()
}
```

```go
// internal/catalog/syncer.go

type Syncer interface {
    // SyncProvider performs an immediate catalog sync for one provider.
    SyncProvider(ctx context.Context, providerID string) error

    // Start begins background sync goroutines for all enabled providers.
    Start(ctx context.Context)
}
```

---

## Backward Compatibility

### Existing `POST /admin/v1/models/external` endpoint

Unchanged. Models registered via this endpoint continue to work exactly as before.
They are not backed by a catalog entry. They have their own `model_endpoints` row,
their own pool entry, and their own `Registry.epClients` entry.

### Existing `models` + `model_endpoints` rows

Unchanged. The registry loads them identically. No migration required for existing
data. The new `provider_id` and `provider_catalog_id` columns default to NULL.

### Existing transport config (migration 046)

Per-endpoint transport in `model_endpoints` remains authoritative for Mode A
Public Models registered before migration 047. For new Mode A aliases created
via `POST /admin/v1/models/catalog-alias`, the transport comes from the `providers`
row and is written to `model_endpoints` at creation time (copied, not referenced).

### No breaking changes to the gateway client API

`/v1/chat/completions`, `/v1/embeddings`, `/v1/audio/transcriptions`, `/v1/models`
all behave identically. The `model` field in responses still echoes the NexusLLM name.

---

## Synchronization Lifecycle

```
SyncScheduler.Start()
  │
  for each provider where catalog_sync_enabled:
      schedule ticker at catalog_sync_interval
      on tick: go SyncProvider(ctx, provider.ID)

SyncProvider(ctx, providerID)
  │
  1. Load provider from DB
  2. Build *http.Client via ProviderClientCache.GetOrBuild()
  3. Mark catalog_sync_status = 'syncing'
  4. Call provider backend's Models() method
     (already implemented in runtime/providers.go as openAICompatModels())
  5. Parse returned []BackendModel
  6. For each returned model:
     a. Parse metadata: extract context_length, pricing, capability flags, tags
     b. UPSERT provider_remote_models
  7. For models not returned this sync:
     SET enabled = FALSE, removed_at = NOW()
  8. Re-evaluate exposure rules for this provider
     → update VirtualModelResolver cache
  9. UPDATE providers: catalog_last_synced_at, catalog_model_count, status='ok'
 10. Emit Prometheus metric: nexus_catalog_sync_total{provider, status}

On error at step 4-5:
  → UPDATE providers: catalog_sync_status='error', catalog_sync_error=err
  → Emit nexus_catalog_sync_errors_total{provider}
  → Do NOT modify existing catalog entries
  → Schedule retry with backoff
```

### Tag extraction from model IDs

```go
func extractTags(modelID string) []string {
    tags := []string{}
    parts := strings.Split(modelID, ":")
    if len(parts) > 1 {
        // "openai/gpt-4o:free" → tag "free"
        tags = append(tags, parts[1:]...)
    }
    keywords := []string{"preview","beta","instruct","turbo","mini",
                         "nano","vision","audio","thinking","reasoning"}
    for _, kw := range keywords {
        if strings.Contains(modelID, kw) {
            tags = append(tags, kw)
        }
    }
    return deduplicate(tags)
}
```


---

## Admin UI — Page Structure

### New top-level nav entries

```
Providers          /providers
  └─ [provider]    /providers/:id
       └─ Catalog  /providers/:id/catalog
       └─ Rules    /providers/:id/rules

Models             /models  (unchanged, but now shows Mode A + Mode B models)
```

---

### Page: Cloud Providers  `/providers`

Summary table showing all providers.

```
┌──────────────────┬────────┬────────────┬──────────┬──────────┬─────────┬─────────┐
│ Provider         │ Type   │ Health     │ Synced   │ Catalog  │ Exposed │ Actions │
├──────────────────┼────────┼────────────┼──────────┼──────────┼─────────┼─────────┤
│ 🟢 openrouter    │ OpenR. │ ● healthy  │ 2m ago   │ 482      │ 128     │ …       │
│ 🟢 openai-prod   │ OpenAI │ ● healthy  │ 1h ago   │ 24       │ 24      │ …       │
│ 🟡 anthropic-eu  │ Anthro.│ ● degraded │ 15m ago  │ 8        │ 8       │ …       │
│ 🔴 azure-eastus  │ Azure  │ ✗ down     │ never    │ 0        │ 0       │ …       │
└──────────────────┴────────┴────────────┴──────────┴──────────┴─────────┴─────────┘
```

Row actions: **Sync** · **Test** · **Edit** · **Disable**

Create provider button: opens a form with fields matching the `providers` table.

---

### Page: Provider Detail  `/providers/:id`

Two tabs: **Overview** and **Transport**.

Overview tab shows:
- Name, type, base URL, health, last sync, catalog count, exposed count
- Sync schedule toggle + interval picker
- Direct catalog expose toggle + prefix field
- API key (masked, with rotate button)

Transport tab shows same fields as the current `/transport` endpoint:
proxy_url, TLS, timeouts, pool settings. Identical form to the model-level transport
panel but at provider scope.

---

### Page: Remote Catalog  `/providers/:id/catalog`

Large searchable, filterable table. Server-side pagination.

```
Search: [____________]   Capability: [All ▼]   Tag: [All ▼]   Exposed: [All ▼]

┌───────────────────────────────┬──────────────────┬─────────┬─────────┬──────────┐
│ Model ID                      │ Capabilities     │ Context │ $/1M in │ Exposed  │
├───────────────────────────────┼──────────────────┼─────────┼─────────┼──────────┤
│ openai/gpt-5                  │ 💬 🔧 👁        │ 1M      │ $10.00  │ ✓ alias  │
│ openai/gpt-5:free             │ 💬             🆓│ 128K    │ $0.00   │ ✗ denied │
│ anthropic/claude-sonnet-4-5   │ 💬 🔧 👁        │ 200K    │ $3.00   │ ✓ virtual│
│ google/gemini-2.5-flash       │ 💬 🔧 👁 🎤    │ 1M      │ $0.15   │ ✓ virtual│
│ openai/o3                     │ 💬 🧠          │ 200K    │ $2.00   │ ✓ alias  │
└───────────────────────────────┴──────────────────┴─────────┴─────────┴──────────┘

Capability icons: 💬=chat  🔧=tools  👁=vision  🎤=audio  🧮=embedding  🧠=reasoning
                  🆓=free tag  🔬=preview tag  🚧=beta tag

Bulk actions (on selected rows):
  [Create Alias]  [Add Allow Rule]  [Add Deny Rule]
```

Clicking a row expands metadata: description, pricing breakdown, full tag list,
first_seen_at, last_seen_at, provider_metadata JSON viewer.

---

### Page: Exposure Rules  `/providers/:id/rules`

```
┌──────────────────────────────────────────────────────────────┐
│ Rule evaluation order (drag to reorder)                      │
├──────────────────────────────────────────────────────────────┤
│ ① DENY   *:free          [tag: free]          priority 10   │
│ ② DENY   *:preview       [tag: preview]        priority 10   │
│ ③ ALLOW  openai/*        [all capabilities]    priority 50   │
│ ④ ALLOW  anthropic/*     [chat+tools only]     priority 50   │
│ ⑤ ALLOW  google/*        [require_vision=true] priority 50   │
└──────────────────────────────────────────────────────────────┘

Preview result:
  ✓ 128 models exposed
  ✗ 54 models blocked (34 by rule ①, 20 by rule ②)
  ? 300 models not matched by any allow rule (default deny)

[+ Add Rule]
```

Rule editor (slide-out panel):
- Type: Allow Model / Allow Pattern / Deny Pattern / Capability Filter
- Pattern field with glob syntax hint and live match count
- Capability checkboxes
- Deny tags multi-select
- Priority number input

---

### Public Models page `/models` — changes

The existing page gains a new badge on each model row:

- Local models: `vllm` / `llamacpp` / `tgi` etc. badge (unchanged)
- Mode A (catalog alias): provider badge + `alias` chip + link to catalog entry
- Mode B (virtual): provider badge + `virtual` chip
- Legacy external (pre-catalog): provider badge (unchanged)

The "Register Cloud/External" button now opens a choice:

```
┌─────────────────────────────────────────────┐
│ Add a cloud model                           │
│                                             │
│ ○ From Catalog                              │
│   Browse your synced provider catalogs      │
│   and create a named alias.                 │
│                                             │
│ ○ Manual (advanced)                         │
│   Enter provider, base URL, and API key     │
│   directly (existing behavior).             │
└─────────────────────────────────────────────┘
```

"From Catalog" opens a provider picker → catalog browser → alias name form.


---

## Policies, Quota, and Audit

### No bypasses

Every request through a virtual model traverses the identical pipeline as a local model:

```
Auth → GatewayPolicy → AliasResolver → CapabilityValidator
     → InfraPolicy (RPM/TPM/quota) → PromptPolicy
     → VirtualModelResolver         ← NEW: replaces Registry miss
     → Backend.Chat()
     → UsageTracker.Record()
```

The `VirtualEndpoint` carries `BackendType`, `UpstreamAPIKey`, and `UpstreamModelName`.
The proxy handler builds `runtime.ChatRequest` from it identically to a normal endpoint.

### Team model allowlist

Team policies store a list of permitted model names. For Mode B virtual models the
name is the `virtual_model_name` from `exposed_catalog` (e.g. `openrouter/openai/gpt-5`).
Administrators add virtual model names to team allowlists the same way they add local
model names. No special handling.

### Quota and rate limiting

`UsageTracker.Record()` receives an `Event` with `ModelName` set to the NexusLLM name
(the virtual model name for Mode B, the alias name for Mode A). Token counters, daily
budgets, and monthly budgets all accumulate against this name. No change to the policy
engine or quota logic.

### Prompt policy

`PromptPolicy.Evaluate()` runs against the NexusLLM model name. Virtual models are
subject to prompt policies the same as any other model. No change.

### Capability validation

`CapabilityValidator.CheckAndAbort()` checks the model's declared capabilities before
routing. For Mode A aliases, capabilities are stored in `models.capabilities` as usual.
For Mode B virtual models, capabilities are derived from `provider_remote_models` flags
and returned by `VirtualModelResolver.Capabilities(modelName)`.

The `CapabilityValidator` is extended to query the resolver as a fallback:

```go
func (v *CapabilityValidator) CheckAndAbort(c *gin.Context, modelName, path string) bool {
    caps, ok := v.registry.GetModelCapabilities(ctx, modelName)
    if !ok && v.catalogResolver != nil {
        caps, ok = v.catalogResolver.Capabilities(ctx, modelName)
    }
    if !ok {
        // model unknown — let downstream handle it
        return true
    }
    // … existing capability check logic unchanged
}
```

### Audit logging

`UsageTracker.Record()` already writes to `usage_events`. The `provider_name` column
(added in migration 045) is populated from the provider's `backend_type`. No change needed.

---

## Metrics

All new metrics use the `nexus_catalog_` namespace.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `nexus_catalog_sync_total` | Counter | `provider`, `status` | Sync attempts by outcome (`ok`/`error`) |
| `nexus_catalog_sync_duration_seconds` | Histogram | `provider` | Time per sync run |
| `nexus_catalog_sync_errors_total` | Counter | `provider`, `error_type` | Sync failures by type |
| `nexus_catalog_models_total` | Gauge | `provider` | Total models in catalog |
| `nexus_catalog_exposed_total` | Gauge | `provider` | Models currently exposed |
| `nexus_catalog_virtual_requests_total` | Counter | `provider`, `model` | Requests via virtual resolver |
| `nexus_catalog_virtual_latency_seconds` | Histogram | `provider` | Resolver lookup latency |
| `nexus_provider_health_status` | Gauge | `provider` | `1`=healthy `0`=down |

All existing metrics (`nexus_provider_requests_total`, `nexus_provider_latency_seconds`,
`nexus_provider_cost_total`, etc.) continue to work. Virtual model requests are recorded
with `provider` label set to the provider name and `model` label set to the virtual
model name.

---

## Implementation Order

The following order minimises risk. Each phase is independently deployable and
backward-compatible.

### Phase 1 — Provider table and admin API (no catalog yet)

Deliverables:
- Migration 047 (providers table only, skip catalog tables)
- `internal/catalog/provider.go` — Provider struct, DB load/save
- `GET/POST/PUT/DELETE /admin/v1/providers`
- `PUT /admin/v1/providers/:id/transport`
- `GET /admin/v1/providers/:id/health`
- Admin UI: Cloud Providers list + create/edit form
- Tests: provider CRUD, transport isolation

This phase introduces no runtime behaviour change. Existing external models are unaffected.

### Phase 2 — Remote Catalog sync

Deliverables:
- Migration 047 complete (add catalog + rules tables)
- `internal/catalog/syncer.go`
- `internal/catalog/scheduler.go`
- `GET /admin/v1/providers/:id/catalog`
- `POST /admin/v1/providers/:id/sync`
- Admin UI: Remote Catalog page (read-only view)
- Tests: sync upsert, removal detection, error handling, tag extraction

No gateway behaviour change. Catalog is populated but not yet served to clients.

### Phase 3 — Exposure Rules engine

Deliverables:
- `internal/catalog/rules.go`
- `exposed_catalog` view
- `GET/POST/PUT/DELETE /admin/v1/providers/:id/rules`
- `POST /admin/v1/providers/:id/rules/preview`
- Admin UI: Exposure Rules page with live preview
- Tests: glob matching, deny-before-allow, capability filters, tag deny

Still no gateway behaviour change. Rules are evaluated but not yet consumed.

### Phase 4 — Mode A: Catalog Aliases

Deliverables:
- `POST /admin/v1/models/catalog-alias`
- Migration 047 addendum: `provider_id` + `provider_catalog_id` on `models`
- Admin UI: "From Catalog" path in Add Cloud Model dialog
- Tests: alias creation, transport inherited from provider, registry loads correctly

Public Models backed by catalog entries. Existing `POST /admin/v1/models/external`
continues to work. No virtual resolution needed yet.

### Phase 5 — Mode B: Virtual Model Resolver

Deliverables:
- `internal/catalog/resolver.go`
- `internal/catalog/client_cache.go`
- `internal/catalog/virtual_endpoint.go`
- `proxy.Handler` extended with `VirtualModelResolver`
- `GET /v1/models` updated to include virtual model names
- `CapabilityValidator` extended to query catalog resolver
- Tests: virtual resolution, pipeline traversal, policy enforcement,
         streaming through virtual endpoint, no env-var proxy leakage

This is the only phase that changes gateway request handling.

### Phase 6 — Frontend completion

Deliverables:
- Provider Dashboard with health, sync status, model counts
- Remote Catalog full UI (search, filters, bulk actions, capability badges)
- Exposure Rules drag-to-reorder, live preview panel
- Public Models page: Mode A / Mode B / legacy badges
- Sync status indicators with last-sync timestamps

---

## Invariants

These must hold after all phases are complete.

1. **Single client API.** Clients call `/v1/chat/completions` with a NexusLLM name.
   They never see provider-internal model IDs unless `catalog_direct_expose` is enabled
   and they are calling a Mode B virtual name.

2. **No pipeline bypass.** Every request — local, Mode A, Mode B, legacy external —
   traverses the full pipeline: Auth → Policy → Alias → Capability → Quota →
   PromptPolicy → (Registry or VirtualResolver) → Backend → UsageTracker.

3. **Transport isolation.** One `*http.Client` per provider in `ProviderClientCache`.
   Never one per virtual model. Never reads `HTTP_PROXY` / `HTTPS_PROXY` env vars.
   `BuildProviderClient()` is the only constructor.

4. **No catalog rows in the model pool.** `Registry.pools` contains only rows from
   `models` + `model_endpoints`. Virtual models are resolved by `VirtualModelResolver`
   independently. The two namespaces never overlap.

5. **Catalog entries are never deleted.** Sync marks removed models `enabled=FALSE`.
   Admin configuration (rules, aliases) referencing a removed model is preserved.
   The model is hidden from clients until it reappears in a future sync.

6. **Default deny.** A catalog entry that matches no allow rule is not exposed.
   An empty rule set means zero models are exposed, not all models.

7. **Backward compatibility.** `POST /admin/v1/models/external`, existing `models`
   rows, existing `model_endpoints` rows, existing transport config (migration 046),
   and all existing admin API endpoints continue to work without modification.

8. **Streaming unchanged.** SSE forwarding, chunk normalisation, usage synthesis,
   and TTFT metrics work identically for virtual models. The backend receives a
   `ChatRequest` with `Client` from `ProviderClientCache` — same interface as today.

9. **Health checking.** The watcher continues to health-check Mode A endpoints via
   `model_endpoints`. Provider-level health is checked separately by a dedicated
   goroutine that calls `GET /v1/models` (or equivalent) on each provider and updates
   `providers.health`. Virtual models inherit their provider's health status.

10. **Metrics completeness.** Every request generates `nexus_provider_requests_total`,
    `nexus_provider_latency_seconds`, and a `usage_events` row regardless of whether
    it was served via pool, Mode A alias, or Mode B virtual endpoint.
