-- NexusLLM initial schema — idempotent (safe to re-run)
BEGIN;

CREATE OR REPLACE FUNCTION trigger_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN NEW.updated_at = NOW(); RETURN NEW; END;
$$ LANGUAGE plpgsql;

-- Organizations
CREATE TABLE IF NOT EXISTS organizations (
    id         UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    name       VARCHAR(255) NOT NULL,
    slug       VARCHAR(100) NOT NULL UNIQUE,
    active     BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_orgs_slug ON organizations(slug);
DROP TRIGGER IF EXISTS set_organizations_updated_at ON organizations;
CREATE TRIGGER set_organizations_updated_at
    BEFORE UPDATE ON organizations FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- Teams
CREATE TABLE IF NOT EXISTS teams (
    id         UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID         NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name       VARCHAR(255) NOT NULL,
    slug       VARCHAR(100) NOT NULL,
    priority   INTEGER      NOT NULL DEFAULT 5 CHECK (priority BETWEEN 1 AND 100),
    active     BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE(org_id, slug)
);
CREATE INDEX IF NOT EXISTS idx_teams_org_id ON teams(org_id);
DROP TRIGGER IF EXISTS set_teams_updated_at ON teams;
CREATE TRIGGER set_teams_updated_at
    BEFORE UPDATE ON teams FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- Users
CREATE TABLE IF NOT EXISTS users (
    id         UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID         NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    email      VARCHAR(255) NOT NULL UNIQUE,
    role       VARCHAR(50)  NOT NULL DEFAULT 'member' CHECK (role IN ('admin','member','viewer')),
    active     BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_users_org_id ON users(org_id);
CREATE INDEX IF NOT EXISTS idx_users_email  ON users(email);
DROP TRIGGER IF EXISTS set_users_updated_at ON users;
CREATE TRIGGER set_users_updated_at
    BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- Team Members
CREATE TABLE IF NOT EXISTS team_members (
    team_id UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role    VARCHAR(50) NOT NULL DEFAULT 'member',
    PRIMARY KEY (team_id, user_id)
);

-- Service Accounts
CREATE TABLE IF NOT EXISTS service_accounts (
    id         UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id    UUID         NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    name       VARCHAR(255) NOT NULL,
    active     BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_sa_team_id ON service_accounts(team_id);

-- API Keys
CREATE TABLE IF NOT EXISTS api_keys (
    id                 UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id            UUID         REFERENCES teams(id) ON DELETE CASCADE,
    service_account_id UUID         REFERENCES service_accounts(id) ON DELETE CASCADE,
    name               VARCHAR(255),
    key_hash           VARCHAR(64)  NOT NULL UNIQUE,
    key_prefix         VARCHAR(16)  NOT NULL,
    active             BOOLEAN      NOT NULL DEFAULT TRUE,
    expires_at         TIMESTAMPTZ,
    last_used_at       TIMESTAMPTZ,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (team_id IS NOT NULL OR service_account_id IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS idx_apikeys_key_hash ON api_keys(key_hash);
CREATE INDEX IF NOT EXISTS idx_apikeys_team_id  ON api_keys(team_id);
DROP TRIGGER IF EXISTS set_api_keys_updated_at ON api_keys;
CREATE TRIGGER set_api_keys_updated_at
    BEFORE UPDATE ON api_keys FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- Models (base — extended by migration 003)
CREATE TABLE IF NOT EXISTS models (
    id            UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    name          VARCHAR(255) NOT NULL UNIQUE,
    display_name  VARCHAR(255) NOT NULL,
    vllm_endpoint VARCHAR(512) NOT NULL DEFAULT '',
    max_tokens    INTEGER      NOT NULL DEFAULT 4096,
    active        BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_models_name ON models(name);
DROP TRIGGER IF EXISTS set_models_updated_at ON models;
CREATE TRIGGER set_models_updated_at
    BEFORE UPDATE ON models FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- Policies
CREATE TABLE IF NOT EXISTS policies (
    id                 UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id            UUID    NOT NULL REFERENCES teams(id) ON DELETE CASCADE UNIQUE,
    rpm                INTEGER NOT NULL DEFAULT 100,
    tpd                INTEGER NOT NULL DEFAULT 1000000,
    max_concurrent     INTEGER NOT NULL DEFAULT 10,
    max_context_tokens INTEGER NOT NULL DEFAULT 8192,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
DROP TRIGGER IF EXISTS set_policies_updated_at ON policies;
CREATE TRIGGER set_policies_updated_at
    BEFORE UPDATE ON policies FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- Team ↔ Model permissions
CREATE TABLE IF NOT EXISTS team_model_permissions (
    team_id  UUID NOT NULL REFERENCES teams(id)  ON DELETE CASCADE,
    model_id UUID NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    PRIMARY KEY (team_id, model_id)
);

-- Audit Logs
CREATE TABLE IF NOT EXISTS audit_logs (
    id          BIGSERIAL    PRIMARY KEY,
    org_id      UUID         NOT NULL,
    team_id     UUID,
    user_id     UUID,
    action      VARCHAR(100) NOT NULL,
    resource    VARCHAR(100),
    resource_id UUID,
    metadata    JSONB        NOT NULL DEFAULT '{}',
    ip_address  INET,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_audit_org_time  ON audit_logs(org_id,  created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_team_time ON audit_logs(team_id, created_at DESC);

COMMIT;
