-- NexusLLM Migration 047 — Provider Catalog Architecture
--
-- Introduces the four-layer provider catalog system:
--   Layer 1: providers       — connection config per cloud provider
--   Layer 2: provider_remote_models — synced catalog (read-only mirror)
--   Layer 3: provider_exposure_rules — allow/deny rules
--   Layer 4: models FK columns — link Public Models to catalog entries
--
-- All statements are idempotent (safe to re-run).
-- Existing models / model_endpoints rows are unaffected.

BEGIN;

-- ── Layer 1: providers ────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS providers (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Identity
    name              TEXT NOT NULL UNIQUE,
    display_name      TEXT NOT NULL,
    backend_type      TEXT NOT NULL,
    base_url          TEXT NOT NULL,

    -- Credentials
    api_key           TEXT NOT NULL DEFAULT '',
    api_key_header    TEXT NOT NULL DEFAULT 'Authorization',

    -- Catalog sync
    catalog_sync_enabled   BOOLEAN     NOT NULL DEFAULT FALSE,
    catalog_sync_interval  INT         NOT NULL DEFAULT 3600,
    catalog_direct_expose  BOOLEAN     NOT NULL DEFAULT FALSE,
    catalog_expose_prefix  TEXT        NOT NULL DEFAULT '',
    catalog_last_synced_at TIMESTAMPTZ,
    catalog_model_count    INT         NOT NULL DEFAULT 0,
    catalog_sync_status    TEXT        NOT NULL DEFAULT 'never',
    catalog_sync_error     TEXT,

    -- Transport (mirrors migration 046 per-endpoint columns at provider scope)
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

    -- Request settings
    request_timeout_seconds INT NOT NULL DEFAULT 120,
    max_retries             INT NOT NULL DEFAULT 2,

    -- Status
    enabled           BOOLEAN     NOT NULL DEFAULT TRUE,
    health            TEXT        NOT NULL DEFAULT 'unknown',
    last_health_check TIMESTAMPTZ,

    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_providers_enabled ON providers(enabled);
CREATE INDEX IF NOT EXISTS idx_providers_backend ON providers(backend_type);

-- ── Layer 2: remote catalog ───────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS provider_remote_models (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id       UUID NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    provider_model_id TEXT NOT NULL,

    -- Metadata from provider /models response
    display_name      TEXT NOT NULL DEFAULT '',
    description       TEXT NOT NULL DEFAULT '',
    context_length    INT,
    input_cost_per_1m  NUMERIC,
    output_cost_per_1m NUMERIC,

    -- Capability flags
    supports_streaming   BOOLEAN NOT NULL DEFAULT TRUE,
    supports_tools       BOOLEAN NOT NULL DEFAULT FALSE,
    supports_vision      BOOLEAN NOT NULL DEFAULT FALSE,
    supports_audio       BOOLEAN NOT NULL DEFAULT FALSE,
    supports_embeddings  BOOLEAN NOT NULL DEFAULT FALSE,
    supports_reasoning   BOOLEAN NOT NULL DEFAULT FALSE,
    supports_images      BOOLEAN NOT NULL DEFAULT FALSE,

    -- Tags extracted from model ID (free, preview, beta, instruct, etc.)
    tags              TEXT[]      NOT NULL DEFAULT '{}',

    -- Raw metadata from provider
    provider_metadata JSONB       NOT NULL DEFAULT '{}',

    -- Lifecycle
    enabled           BOOLEAN     NOT NULL DEFAULT TRUE,
    first_seen_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    removed_at        TIMESTAMPTZ,

    UNIQUE (provider_id, provider_model_id)
);

CREATE INDEX IF NOT EXISTS idx_prm_provider     ON provider_remote_models(provider_id);
CREATE INDEX IF NOT EXISTS idx_prm_enabled      ON provider_remote_models(provider_id, enabled);
CREATE INDEX IF NOT EXISTS idx_prm_tags         ON provider_remote_models USING gin(tags);

-- ── Layer 3: exposure rules ───────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS provider_exposure_rules (
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

CREATE INDEX IF NOT EXISTS idx_per_provider ON provider_exposure_rules(provider_id, enabled, priority);

-- ── Layer 4: link Public Models to catalog entries ────────────────────────────

ALTER TABLE models
    ADD COLUMN IF NOT EXISTS provider_id         UUID REFERENCES providers(id),
    ADD COLUMN IF NOT EXISTS provider_catalog_id UUID REFERENCES provider_remote_models(id);

COMMENT ON COLUMN models.provider_id IS
    'FK to providers. Set for catalog-backed Public Models. NULL for local models.';
COMMENT ON COLUMN models.provider_catalog_id IS
    'FK to provider_remote_models. Set for catalog-backed aliases. NULL otherwise.';

COMMIT;
