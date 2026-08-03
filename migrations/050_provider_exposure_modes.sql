-- NexusLLM Migration 050 — Provider Exposure Modes
--
-- Introduces three-mode exposure for cloud providers:
--
--   managed  (default, existing behaviour)
--     Providers are a source catalogue only. Administrators explicitly register
--     Public Models via RegisterCatalogAlias / BulkRegisterFromCatalog.
--     Only registered models appear in GET /v1/models.
--     Authorization: team_model_permissions (unchanged).
--
--   catalog
--     The provider catalogue is exposed directly as virtual models.
--     No Public Model rows are required.
--     GET /v1/models returns <prefix>/<provider_model_id> names.
--     Authorization: project_provider_access — projects are granted access to
--     providers (and optionally constrained by prefix rules).
--     Every request still passes through the full policy/rate-limit/quota pipeline.
--
--   hybrid
--     Both mechanisms operate simultaneously.
--     Registered Public Models AND catalog virtual models are visible and callable.
--     Authorization: team_model_permissions for Public Models +
--                    project_provider_access for virtual catalog models.
--
-- Architecture invariants preserved:
--   • The policy engine, rate limiter, quota, audit, and usage tracker
--     run identically for all three modes.
--   • catalog_direct_expose is kept for backward compatibility and is the
--     computed equivalent of (exposure_mode IN ('catalog','hybrid')).
--   • No existing Public Model rows or team_model_permissions rows are changed.
--   • Managed remains the default — existing providers are unaffected.
--
-- All statements are idempotent (safe to re-run).

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. providers.exposure_mode
--
--    Replaces the boolean catalog_direct_expose with a 3-value enum.
--    catalog_direct_expose is kept and kept in sync by a trigger so that
--    existing code that reads it continues to work unchanged.
-- ─────────────────────────────────────────────────────────────────────────────

ALTER TABLE providers
    ADD COLUMN IF NOT EXISTS exposure_mode TEXT NOT NULL DEFAULT 'managed'
        CHECK (exposure_mode IN ('managed', 'catalog', 'hybrid'));

COMMENT ON COLUMN providers.exposure_mode IS
    'managed = explicit Public Model registration only (default). '
    'catalog = provider catalogue exposed as virtual models, no registration required. '
    'hybrid  = both Public Models and virtual catalogue models are available.';

-- Back-fill: providers that already have catalog_direct_expose=TRUE were in the
-- old "Catalog" behaviour — promote them to exposure_mode='catalog'.
UPDATE providers
   SET exposure_mode = 'catalog'
 WHERE catalog_direct_expose = TRUE
   AND exposure_mode = 'managed';

-- Trigger to keep catalog_direct_expose in sync with exposure_mode so that
-- all existing code paths (resolver.go WHERE catalog_direct_expose=TRUE) keep
-- working without modification.  We update catalog_direct_expose here so the
-- source of truth is exposure_mode, not the boolean.
CREATE OR REPLACE FUNCTION trg_sync_catalog_direct_expose()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    NEW.catalog_direct_expose :=
        (NEW.exposure_mode IN ('catalog', 'hybrid'));
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS sync_catalog_direct_expose ON providers;
CREATE TRIGGER sync_catalog_direct_expose
    BEFORE INSERT OR UPDATE OF exposure_mode
    ON providers
    FOR EACH ROW EXECUTE FUNCTION trg_sync_catalog_direct_expose();

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. project_provider_access
--
--    Controls which projects may call catalog/hybrid-mode virtual models.
--    Replaces per-model team_model_permissions for the catalog path.
--    Rate limits and quotas continue to be enforced at project level
--    (via project_policies) — this table is AUTHORIZATION ONLY.
--
--    A project row in this table means: "this project may call ANY virtual
--    model from this provider, subject to optional prefix restrictions".
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS project_provider_access (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  UUID NOT NULL REFERENCES projects(id)  ON DELETE CASCADE,
    provider_id UUID NOT NULL REFERENCES providers(id) ON DELETE CASCADE,

    -- Optional fine-grained prefix allow/deny list.
    -- NULL means "allow all models from this provider" (the common case).
    -- When set, the allowed_prefixes list is evaluated as glob patterns against
    -- <prefix>/<provider_model_id>.  The denied_prefixes list is checked first
    -- (deny wins over allow).
    --
    -- Example:
    --   allowed_prefixes = ['openrouter/openai/*', 'openrouter/anthropic/*']
    --   denied_prefixes  = ['openrouter/openai/gpt-4-*']
    --
    -- Pattern matching uses the same path.Match glob as exposure rules.
    allowed_prefixes TEXT[]      NOT NULL DEFAULT '{}',
    denied_prefixes  TEXT[]      NOT NULL DEFAULT '{}',

    enabled     BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE (project_id, provider_id)
);

COMMENT ON TABLE project_provider_access IS
    'Grants a project access to all virtual (catalog/hybrid) models from a provider. '
    'Does not affect Public Model (managed mode) authorization — that remains in '
    'team_model_permissions. Rate limits are still enforced by project_policies.';

CREATE INDEX IF NOT EXISTS idx_ppa_project  ON project_provider_access(project_id, enabled);
CREATE INDEX IF NOT EXISTS idx_ppa_provider ON project_provider_access(provider_id, enabled);

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. Expand provider_remote_models with richer metadata
--
--    Stores the full capability surface returned by providers like OpenRouter
--    so the UI can filter/display without additional API calls.
--    All new columns are nullable / have safe defaults — existing rows are
--    unaffected and all capability flags can still be set manually.
-- ─────────────────────────────────────────────────────────────────────────────

ALTER TABLE provider_remote_models
    -- Service classification (chat / embedding / speech / image / rerank / ocr)
    ADD COLUMN IF NOT EXISTS service_type         TEXT        NOT NULL DEFAULT 'chat',
    -- Max output token cap reported by the provider
    ADD COLUMN IF NOT EXISTS max_output_tokens    INT,
    -- JSON Mode / structured outputs
    ADD COLUMN IF NOT EXISTS supports_json_mode   BOOLEAN     NOT NULL DEFAULT FALSE,
    -- Function calling (distinct from tool-use in some providers)
    ADD COLUMN IF NOT EXISTS supports_functions   BOOLEAN     NOT NULL DEFAULT FALSE,
    -- Image generation capability
    ADD COLUMN IF NOT EXISTS supports_image_gen   BOOLEAN     NOT NULL DEFAULT FALSE,
    -- Rerank (cross-encoder models)
    ADD COLUMN IF NOT EXISTS supports_rerank      BOOLEAN     NOT NULL DEFAULT FALSE,
    -- OCR
    ADD COLUMN IF NOT EXISTS supports_ocr         BOOLEAN     NOT NULL DEFAULT FALSE,
    -- TTS / Speech synthesis
    ADD COLUMN IF NOT EXISTS supports_speech      BOOLEAN     NOT NULL DEFAULT FALSE,
    -- Provider-reported pricing (may differ from our manual input_cost_per_1m)
    ADD COLUMN IF NOT EXISTS provider_input_cost  NUMERIC,
    ADD COLUMN IF NOT EXISTS provider_output_cost NUMERIC,
    -- Human-readable description from provider metadata
    ADD COLUMN IF NOT EXISTS provider_description TEXT        NOT NULL DEFAULT '';

COMMENT ON COLUMN provider_remote_models.service_type IS
    'chat | embedding | speech | image | rerank | ocr — drives capability default in the UI.';

CREATE INDEX IF NOT EXISTS idx_prm_service_type
    ON provider_remote_models(provider_id, service_type)
    WHERE enabled = TRUE;

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. Index for fast virtual model name resolution
--
--    The hot path in VirtualModelResolver.buildCache executes:
--      SELECT ... FROM provider_remote_models WHERE provider_id=$1 AND enabled=TRUE
--    Add a covering index on the columns read in the tight inner loop.
-- ─────────────────────────────────────────────────────────────────────────────

CREATE INDEX IF NOT EXISTS idx_prm_catalog_lookup
    ON provider_remote_models(provider_id, provider_model_id)
    WHERE enabled = TRUE;

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. providers: index on exposure_mode for VirtualModelResolver query
--    (WHERE enabled=TRUE AND exposure_mode IN ('catalog','hybrid'))
-- ─────────────────────────────────────────────────────────────────────────────

CREATE INDEX IF NOT EXISTS idx_providers_exposure_mode
    ON providers(exposure_mode, enabled)
    WHERE enabled = TRUE;

COMMIT;
