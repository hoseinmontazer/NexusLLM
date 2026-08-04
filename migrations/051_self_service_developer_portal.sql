-- NexusLLM Migration 051 — Self-Service Developer Portal
--
-- Introduces self-service developer portal tables:
--   • project_access_requests: access request state machine & review details
--   • developer_notifications: alert system for developers (approved, rejected, quota, key lifecycle)
--   • projects extensions: environment, expected usage metrics, owner_user_id
--
-- Architecture invariants preserved:
--   • Reuses existing team_model_permissions, project_provider_access, project_policies, api_keys.
--   • Approved requests automatically provision permissions, rates, quotas, and API keys.
--   • All statements are idempotent (safe to re-run).

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. Projects extensions
-- ─────────────────────────────────────────────────────────────────────────────

ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS environment VARCHAR(32) DEFAULT 'development',
    ADD COLUMN IF NOT EXISTS expected_monthly_requests BIGINT DEFAULT 0,
    ADD COLUMN IF NOT EXISTS expected_monthly_tokens BIGINT DEFAULT 0,
    ADD COLUMN IF NOT EXISTS owner_user_id UUID;

ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_status_check;
ALTER TABLE projects ADD CONSTRAINT projects_status_check
    CHECK (status IN ('active', 'inactive', 'archived', 'pending'));

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. project_access_requests
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS project_access_requests (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id              UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    organization_id         UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    team_id                 UUID REFERENCES teams(id) ON DELETE CASCADE,
    requested_by            UUID,
    status                  VARCHAR(32) NOT NULL DEFAULT 'pending_review',
    requested_models        JSONB NOT NULL DEFAULT '[]'::jsonb,
    requested_providers     JSONB NOT NULL DEFAULT '[]'::jsonb,
    business_use_case       TEXT,
    expected_rpm            INT DEFAULT 0,
    expected_tpm            INT DEFAULT 0,
    required_context_size   INT DEFAULT 0,
    reviewed_by             UUID,
    review_notes            TEXT,
    provisioned_api_key_id  UUID REFERENCES api_keys(id) ON DELETE SET NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    reviewed_at             TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_par_project ON project_access_requests(project_id);
CREATE INDEX IF NOT EXISTS idx_par_org     ON project_access_requests(organization_id);
CREATE INDEX IF NOT EXISTS idx_par_status  ON project_access_requests(status);

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. developer_notifications
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS developer_notifications (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID,
    project_id  UUID REFERENCES projects(id) ON DELETE CASCADE,
    title       VARCHAR(255) NOT NULL,
    message     TEXT NOT NULL,
    type        VARCHAR(64) NOT NULL DEFAULT 'general',
    read        BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_dev_notif_user ON developer_notifications(user_id, read);
CREATE INDEX IF NOT EXISTS idx_dev_notif_proj ON developer_notifications(project_id);

COMMIT;
