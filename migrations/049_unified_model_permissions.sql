-- NexusLLM Migration 049 — Unified Model Permission Architecture
--
-- Goal: Collapse the dual permission system (team_model_permissions for local
--       models + team_virtual_model_permissions for remote catalog models) into
--       a single, backend-agnostic authorization table.
--
-- Architecture contract (migration 044 already established this):
--
--   ONE models table.
--   ONE permission table: team_model_permissions.
--   Authorization is MODEL-CENTRIC. Backend type never affects access.
--
-- Every Public Model that can be called — whether backed by:
--   vLLM, TGI, llama.cpp, cpu_native (local)
--   openai_provider, anthropic_provider, google_provider,
--   azure_openai_provider, openrouter_provider, groq_provider,
--   together_provider, mistral_provider, cohere_provider, deepseek_provider
--   (remote / cloud)
-- — MUST have a row in the models table and MUST be granted via
--   team_model_permissions. No second table is needed or permitted.
--
-- Mode-B virtual models (catalog direct-expose) are a display/discovery
-- feature. A model that appears in the virtual catalog is NOT callable until
-- an administrator creates a Public Model row (RegisterCatalogAlias or
-- equivalent) and grants it to a team via team_model_permissions.
--
-- Changes:
--   1. Add a comment to team_model_permissions clarifying it covers all
--      backend types (no functional change — comment only).
--   2. Drop team_virtual_model_permissions if it exists. It was a temporary
--      hotfix table introduced during the authorization security fix and is
--      superseded by this unified architecture.
--   3. Add a partial index on models(provider_is_external) to accelerate
--      permission seeding queries that join against remote models.
--   4. Ensure models.enabled has an index (used in every permission query).
--
-- All statements are idempotent (safe to re-run).

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. COMMENT: clarify that team_model_permissions covers ALL model types.
--    This is the canonical, universal grant table.
-- ─────────────────────────────────────────────────────────────────────────────

COMMENT ON TABLE team_model_permissions IS
    'Universal model ACL. One row = one team has permission to call one model. '
    'Applies to ALL backend types: local runtimes and cloud providers alike.';

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. DROP team_virtual_model_permissions.
--    This table was introduced as a temporary security hotfix for Mode-B
--    virtual catalog models. It is superseded by the unified architecture:
--    all callable models must have a models row and use team_model_permissions.
--
--    Safe to drop because:
--    a) The table was never created by a prior migration (no migration file
--       exists that creates it — it was referenced in Go code only, with
--       resilient queries that silently return nothing when it's absent).
--    b) Any rows that may exist in a deployment that manually created the
--       table are superseded: those models should be registered as Public
--       Models via RegisterCatalogAlias and granted via team_model_permissions.
-- ─────────────────────────────────────────────────────────────────────────────

DROP TABLE IF EXISTS team_virtual_model_permissions;

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. INDEX: accelerate permission-seeding queries for remote models.
--    The seedModelPermissions query joins team_model_permissions → models →
--    teams. A partial index on models(provider_is_external) makes the
--    "how many of these are remote?" question O(1).
-- ─────────────────────────────────────────────────────────────────────────────

CREATE INDEX IF NOT EXISTS idx_models_provider_external_enabled
    ON models(provider_is_external, enabled)
    WHERE provider_is_external = TRUE AND enabled = TRUE;

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. INDEX: models.enabled is used in every hot-path permission query.
--    Ensure a plain index exists in case the partial index above isn't used
--    by the planner for the generic case.
-- ─────────────────────────────────────────────────────────────────────────────

CREATE INDEX IF NOT EXISTS idx_models_enabled ON models(enabled) WHERE enabled = TRUE;
CREATE INDEX IF NOT EXISTS idx_models_name_enabled ON models(name) WHERE enabled = TRUE;

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. INDEX: team_model_permissions lookups are always by team_id.
--    The existing PRIMARY KEY (team_id, model_id) covers this, but a
--    dedicated index on team_id alone is more efficient for the full-scan
--    permission-seeding query.
-- ─────────────────────────────────────────────────────────────────────────────

CREATE INDEX IF NOT EXISTS idx_team_model_permissions_team
    ON team_model_permissions(team_id);

COMMIT;
