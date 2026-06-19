-- NexusLLM initial schema
-- Run order: 001_initial.sql → 002_seed_data.sql

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- Utility: auto-update updated_at columns
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION trigger_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
  NEW.updated_at = NOW();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- Organizations
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE organizations (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name        VARCHAR(255) NOT NULL,
    slug        VARCHAR(100) NOT NULL UNIQUE,
    active      BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_orgs_slug ON organizations(slug);

CREATE TRIGGER set_organizations_updated_at
BEFORE UPDATE ON organizations
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- Teams
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE teams (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name       VARCHAR(255) NOT NULL,
    slug       VARCHAR(100) NOT NULL,
    priority   INTEGER     NOT NULL DEFAULT 5 CHECK (priority BETWEEN 1 AND 100),
    active     BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(org_id, slug)
);
CREATE INDEX idx_teams_org_id ON teams(org_id);

CREATE TRIGGER set_teams_updated_at
BEFORE UPDATE ON teams
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- Users
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE users (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    email      VARCHAR(255) NOT NULL UNIQUE,
    role       VARCHAR(50) NOT NULL DEFAULT 'member' CHECK (role IN ('admin','member','viewer')),
    active     BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_users_org_id ON users(org_id);
CREATE INDEX idx_users_email  ON users(email);

CREATE TRIGGER set_users_updated_at
BEFORE UPDATE ON users
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- Team Members
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE team_members (
    team_id UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role    VARCHAR(50) NOT NULL DEFAULT 'member',
    PRIMARY KEY (team_id, user_id)
);

-- ─────────────────────────────────────────────────────────────────────────────
-- Service Accounts
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE service_accounts (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id    UUID        NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    name       VARCHAR(255) NOT NULL,
    active     BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_sa_team_id ON service_accounts(team_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- API Keys  (raw key stored only as SHA-256 hash)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE api_keys (
    id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id            UUID        REFERENCES teams(id) ON DELETE CASCADE,
    service_account_id UUID        REFERENCES service_accounts(id) ON DELETE CASCADE,
    name               VARCHAR(255),
    key_hash           VARCHAR(64) NOT NULL UNIQUE,  -- SHA-256 hex
    key_prefix         VARCHAR(16) NOT NULL,          -- first ~12 chars for display
    active             BOOLEAN     NOT NULL DEFAULT TRUE,
    expires_at         TIMESTAMPTZ,
    last_used_at       TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (team_id IS NOT NULL OR service_account_id IS NOT NULL)
);
CREATE INDEX idx_apikeys_key_hash ON api_keys(key_hash);
CREATE INDEX idx_apikeys_team_id  ON api_keys(team_id);

CREATE TRIGGER set_api_keys_updated_at
BEFORE UPDATE ON api_keys
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- Models (vLLM endpoint registry)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE models (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name          VARCHAR(255) NOT NULL UNIQUE,   -- e.g. "gemma-27b"
    display_name  VARCHAR(255) NOT NULL,
    vllm_endpoint VARCHAR(512) NOT NULL,           -- http://vllm-gemma:8000
    max_tokens    INTEGER     NOT NULL DEFAULT 4096,
    active        BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_models_name ON models(name);

CREATE TRIGGER set_models_updated_at
BEFORE UPDATE ON models
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- Policies  (one row per team, enforced on the hot path via Redis cache)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE policies (
    id                UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id           UUID    NOT NULL REFERENCES teams(id) ON DELETE CASCADE UNIQUE,
    rpm               INTEGER NOT NULL DEFAULT 100,       -- requests/min
    tpd               INTEGER NOT NULL DEFAULT 1000000,   -- tokens/day
    max_concurrent    INTEGER NOT NULL DEFAULT 10,
    max_context_tokens INTEGER NOT NULL DEFAULT 8192,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TRIGGER set_policies_updated_at
BEFORE UPDATE ON policies
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- Team ↔ Model permissions
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE team_model_permissions (
    team_id  UUID NOT NULL REFERENCES teams(id)  ON DELETE CASCADE,
    model_id UUID NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    PRIMARY KEY (team_id, model_id)
);

-- ─────────────────────────────────────────────────────────────────────────────
-- Audit Log  (append-only)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE audit_logs (
    id          BIGSERIAL   PRIMARY KEY,
    org_id      UUID        NOT NULL,
    team_id     UUID,
    user_id     UUID,
    action      VARCHAR(100) NOT NULL,  -- "api_key.created", "policy.updated"
    resource    VARCHAR(100),
    resource_id UUID,
    metadata    JSONB       NOT NULL DEFAULT '{}',
    ip_address  INET,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_audit_org_time  ON audit_logs(org_id,  created_at DESC);
CREATE INDEX idx_audit_team_time ON audit_logs(team_id, created_at DESC);

COMMIT;
