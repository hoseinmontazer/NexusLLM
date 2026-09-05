-- NexusLLM Migration 062 — Multi-Provider Credential Routing
--
-- Problem: providers.api_key (migration 047) is a single credential shared by
-- every project/team authorized to call that provider (via
-- project_provider_access, migration 050). Two apps with two different
-- OpenRouter accounts routed through NexusLLM had no way to keep their
-- upstream tokens separate — both were forced onto the same providers.api_key
-- row.
--
-- Fix: introduce a pool of named credentials per provider
-- (provider_credentials), and let project_provider_access — which already
-- models "this project may call this provider" — optionally pin one specific
-- credential per grant. This deliberately extends the existing authorization
-- table instead of introducing a parallel "CredentialAssignment" concept:
-- project_provider_access already has UNIQUE(project_id, provider_id), so
-- "at most one credential per project per provider" falls out for free.
--
-- Resolution precedence (implemented in internal/catalog/credential_resolver.go):
--   1. project_provider_access.credential_id, if set and enabled
--      -> if set but the credential is disabled/missing: FAIL CLOSED
--         (provider_credential_unavailable). Never silently reassign.
--   2. provider_credentials row with is_default=TRUE and enabled=TRUE
--   3. providers.api_key (legacy single-credential column), only when the
--      provider has ZERO rows in provider_credentials at all — pure backward
--      compatibility for providers never migrated to the new model.
--   4. otherwise: provider_credential_unavailable.
--
-- Secrets are encrypted at rest (AES-256-GCM, internal/secretstore) — this is
-- the first reversible-secret storage in NexusLLM; providers.api_key and
-- model_endpoints.upstream_api_key remain plaintext (pre-existing, documented
-- in migration 040) and are NOT touched by this migration — re-encrypting
-- them would require the plaintext value in hand, which a SQL migration
-- cannot safely do. Tracked as a follow-up (see deployment report).
--
-- All statements are idempotent (safe to re-run).

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. provider_credentials — a pool of named, encrypted credentials per provider
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS provider_credentials (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id       UUID NOT NULL REFERENCES providers(id) ON DELETE CASCADE,

    -- Human-readable label, e.g. "production-app-a". Never the secret itself.
    name              TEXT NOT NULL,

    -- AES-256-GCM ciphertext, base64-encoded (nonce || ciphertext || tag),
    -- produced by internal/secretstore.Store.Encrypt. The encryption key lives
    -- in NEXUS_CREDENTIAL_ENCRYPTION_KEY (env), never in this database.
    secret_ciphertext TEXT NOT NULL,

    -- Per-credential header override. NULL = inherit providers.api_key_header.
    api_key_header    TEXT,

    -- The credential used when a project has an authorization grant
    -- (project_provider_access) but no credential_id pinned on it.
    -- At most one default per provider (partial unique index below).
    is_default        BOOLEAN NOT NULL DEFAULT FALSE,

    enabled           BOOLEAN NOT NULL DEFAULT TRUE,
    metadata          JSONB   NOT NULL DEFAULT '{}',

    last_used_at      TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE (provider_id, name)
);

COMMENT ON TABLE provider_credentials IS
    'Pool of named upstream credentials for one provider. A project is routed '
    'to exactly one of these via project_provider_access.credential_id, or to '
    'the is_default row when no credential is pinned. Secrets are AES-256-GCM '
    'encrypted (internal/secretstore) — never returned by any API, never logged.';

COMMENT ON COLUMN provider_credentials.secret_ciphertext IS
    'Encrypted secret. NEVER select this column in any admin-facing query — '
    'only credential_resolver.go may read and decrypt it.';

-- At most one default credential per provider.
CREATE UNIQUE INDEX IF NOT EXISTS idx_provider_credentials_one_default
    ON provider_credentials(provider_id)
    WHERE is_default = TRUE;

CREATE INDEX IF NOT EXISTS idx_provider_credentials_provider
    ON provider_credentials(provider_id, enabled);

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. project_provider_access.credential_id — pin a project's grant to one
--    specific credential from the pool above.
--
--    NULL (the default for every existing row) preserves current behaviour
--    exactly: falls through to the provider's default credential, or to
--    providers.api_key if the provider has no provider_credentials rows yet.
--    Existing installations are therefore unaffected until an operator
--    explicitly creates provider_credentials rows and assigns one.
-- ─────────────────────────────────────────────────────────────────────────────

ALTER TABLE project_provider_access
    ADD COLUMN IF NOT EXISTS credential_id UUID REFERENCES provider_credentials(id) ON DELETE SET NULL;

COMMENT ON COLUMN project_provider_access.credential_id IS
    'Pins this project''s grant to one specific provider_credentials row. '
    'NULL = use the provider''s default credential (or providers.api_key if '
    'the provider has no provider_credentials rows at all). If set, the '
    'credential MUST exist and be enabled or the request fails closed with '
    'provider_credential_unavailable — it never silently falls back to a '
    'different credential.';

CREATE INDEX IF NOT EXISTS idx_ppa_credential ON project_provider_access(credential_id)
    WHERE credential_id IS NOT NULL;

-- Guard: a pinned credential_id must belong to the same provider_id as the
-- access grant row itself — enforced at the application layer (see
-- internal/admin/handlers/catalog.go AssignProjectProviderCredential), because
-- Postgres CHECK constraints cannot reference other tables. Documented here
-- so the invariant is discoverable from the schema.

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. usage_events.credential_id — per-request attribution.
--
--    usage_events (migration 005) is the live, populated usage pipeline
--    (internal/usage/tracker.go). Adding credential_id here — rather than to
--    the richer but currently-unpopulated inference_usage table (migration
--    054) — means every proxied request through a virtual/catalog provider
--    model can now be attributed to the exact credential that handled it,
--    without waiting on inference_usage to be wired into the live path.
-- ─────────────────────────────────────────────────────────────────────────────

ALTER TABLE usage_events
    ADD COLUMN IF NOT EXISTS credential_id UUID;

COMMENT ON COLUMN usage_events.credential_id IS
    'provider_credentials.id that handled this request (virtual/catalog '
    'provider models only). NULL for managed/local models and for requests '
    'predating this column. Intentionally not a FK: usage_events is RANGE '
    'partitioned by created_at and a credential may be deleted long after '
    'its usage history should still be queryable.';

CREATE INDEX IF NOT EXISTS idx_usage_events_credential
    ON usage_events(credential_id)
    WHERE credential_id IS NOT NULL;

COMMIT;
