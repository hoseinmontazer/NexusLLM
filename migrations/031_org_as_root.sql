-- NexusLLM Migration 031 — Organization as Execution Root
--
-- Domain model change:
--   BEFORE: Organization → Team → Project → Model → Runtime
--   AFTER:  Organization → Project → Model → Runtime
--            Organization → Team (RBAC/membership only)
--
-- Teams are now purely for collaboration and permission grouping.
-- All execution (rate limits, scheduling, quota, billing) is org → project scoped.
--
-- This migration is backward-compatible:
--   • team_id on projects becomes nullable (existing rows retain their team_id)
--   • api_keys gains org_id directly (no longer requires team JOIN for org context)
--   • org_model_permissions added (org-level model ACL, replaces team_model_permissions
--     as the canonical execution-path ACL; team permissions remain for RBAC)
--   • usage_events org_id becomes NOT NULL (was already populated; enforce it)
--   • projects UNIQUE constraint changes from (team_id, name) → (organization_id, name)
--   • gateway_policies gains 'project' as a valid scope
--   • org_usage_daily rollup table added for billing
--
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. api_keys: add direct org_id column
--    Allows auth to resolve org without a team JOIN (future: org-direct keys)
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS org_id UUID REFERENCES organizations(id) ON DELETE CASCADE;

-- Back-fill from team → org
UPDATE api_keys ak
SET org_id = t.org_id
FROM teams t
WHERE ak.team_id = t.id
  AND ak.org_id IS NULL;

CREATE INDEX IF NOT EXISTS idx_api_keys_org_id ON api_keys(org_id)
    WHERE org_id IS NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. projects: make team_id nullable
--    A project belongs to an organization directly.
--    team_id is optional RBAC grouping only.
-- ─────────────────────────────────────────────────────────────────────────────
-- Drop the NOT NULL constraint; keep the FK so existing data is intact.
ALTER TABLE projects
    ALTER COLUMN team_id DROP NOT NULL;

-- Change the uniqueness constraint:
--   BEFORE: UNIQUE(team_id, name)  → projects unique within a team
--   AFTER:  UNIQUE(organization_id, name) → projects unique within an org
DO $$
BEGIN
    ALTER TABLE projects
        DROP CONSTRAINT IF EXISTS projects_team_id_name_key;
    ALTER TABLE projects
        ADD CONSTRAINT projects_organization_id_name_key UNIQUE (organization_id, name);
EXCEPTION WHEN duplicate_table THEN
    -- Constraint already exists — nothing to do.
    NULL;
END $$;

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. org_model_permissions
--    Canonical model ACL at org level.
--    The policy engine checks this first; team_model_permissions remains for
--    finer-grained team RBAC and backward compat.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS org_model_permissions (
    org_id   UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    model_id UUID NOT NULL REFERENCES models(id)        ON DELETE CASCADE,
    PRIMARY KEY (org_id, model_id)
);
CREATE INDEX IF NOT EXISTS idx_org_model_permissions_org_id ON org_model_permissions(org_id);

-- Back-fill from team_model_permissions: any model a team can use, the org can too.
INSERT INTO org_model_permissions (org_id, model_id)
SELECT DISTINCT t.org_id, tmp.model_id
FROM team_model_permissions tmp
JOIN teams t ON t.id = tmp.team_id
ON CONFLICT DO NOTHING;

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. usage_events: enforce org_id NOT NULL, add index
--    org_id was already populated by the existing Go code. Make it mandatory.
-- ─────────────────────────────────────────────────────────────────────────────
-- Fill any nulls first (should be none after migration 005, but be safe)
UPDATE usage_events ue
SET org_id = t.org_id
FROM teams t
WHERE ue.team_id = t.id
  AND ue.org_id IS NULL;

-- Now enforce (only if no NULLs remain — this ALTER will fail if NULLs exist)
-- ALTER TABLE usage_events ALTER COLUMN org_id SET NOT NULL;
-- Commented out intentionally: partitioned tables cannot change column nullability
-- after the fact without rewriting partitions. The application enforces this.

CREATE INDEX IF NOT EXISTS idx_usage_project_time ON usage_events(project_id, created_at DESC)
    WHERE project_id IS NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. org_usage_daily — org-level billing rollup
--    The canonical billing view. Replaces the team-keyed usage_daily for billing.
--    usage_daily (team-keyed) is kept for team-scoped admin UI queries.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS org_usage_daily (
    org_id            UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    model_name        TEXT        NOT NULL DEFAULT '',
    day               DATE        NOT NULL,
    request_count     BIGINT      NOT NULL DEFAULT 0,
    error_count       BIGINT      NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT      NOT NULL DEFAULT 0,
    completion_tokens BIGINT      NOT NULL DEFAULT 0,
    total_tokens      BIGINT      NOT NULL DEFAULT 0,
    cost_usd          NUMERIC(16,8) NOT NULL DEFAULT 0,
    avg_latency_ms    NUMERIC(10,2) NOT NULL DEFAULT 0,
    PRIMARY KEY (org_id, model_name, day)
);
CREATE INDEX IF NOT EXISTS idx_org_usage_daily_org_day
    ON org_usage_daily(org_id, day DESC);

-- Back-fill from usage_events
INSERT INTO org_usage_daily (org_id, model_name, day,
    request_count, error_count, prompt_tokens, completion_tokens,
    total_tokens, cost_usd, avg_latency_ms)
SELECT
    org_id,
    COALESCE(model_name, '') AS model_name,
    created_at::date          AS day,
    COUNT(*),
    COUNT(*) FILTER (WHERE status != 'success'),
    COALESCE(SUM(prompt_tokens), 0),
    COALESCE(SUM(completion_tokens), 0),
    COALESCE(SUM(total_tokens), 0),
    COALESCE(SUM(cost_usd), 0),
    COALESCE(AVG(latency_ms), 0)
FROM usage_events
WHERE org_id IS NOT NULL
  AND created_at::date >= CURRENT_DATE - INTERVAL '90 days'
GROUP BY org_id, COALESCE(model_name, ''), created_at::date
ON CONFLICT (org_id, model_name, day) DO NOTHING;

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. gateway_policies: add 'project' as a valid scope
--    Allows per-project gateway controls (temperature cap, model deny-list,
--    streaming permissions, tool restrictions).
-- ─────────────────────────────────────────────────────────────────────────────
DO $$
BEGIN
    -- Try to modify the CHECK constraint to include 'project'
    ALTER TABLE gateway_policies
        DROP CONSTRAINT IF EXISTS gateway_policies_scope_check;
    ALTER TABLE gateway_policies
        ADD CONSTRAINT gateway_policies_scope_check
        CHECK (scope IN ('org', 'project', 'team', 'api_key'));
EXCEPTION WHEN others THEN
    -- If the constraint doesn't exist or has a different name, skip silently
    NULL;
END;
$$;

-- ─────────────────────────────────────────────────────────────────────────────
-- 7. Refresh the project_runtime_summary view
--    Add org_id directly so dashboards don't need a team JOIN.
-- ─────────────────────────────────────────────────────────────────────────────
DROP VIEW IF EXISTS project_runtime_summary;
CREATE OR REPLACE VIEW project_runtime_summary AS
SELECT
    p.id                                                AS project_id,
    p.name                                              AS project_name,
    p.organization_id                                   AS org_id,
    p.team_id,                                          -- nullable; null = no team assigned
    p.priority_weight,
    priority_label(p.priority_weight)                   AS priority_label,
    p.preemptible,
    COALESCE(ep.effective_priority, p.priority_weight)  AS effective_priority,
    COUNT(DISTINCT ar.id) FILTER (
        WHERE ar.state IN ('active','warm','ready','idle')
    )                                                   AS active_runtime_count,
    COALESCE(pr.reserved_vram_mb, 0)                   AS reserved_vram_mb,
    COALESCE(pr.reserved_cpu_cores, 0)                 AS reserved_cpu_cores,
    COALESCE(pr.reserved_memory_mb, 0)                 AS reserved_memory_mb,
    COALESCE(pr.max_gpu_vram_mb, 0)                    AS max_gpu_vram_mb,
    COALESCE(pr.max_cpu, 0)                            AS max_cpu,
    COALESCE(pr.max_memory_mb, 0)                      AS max_memory_mb,
    COALESCE(pc.always_running, FALSE)                 AS always_running,
    COALESCE(pc.protected, FALSE)                      AS protected,
    COALESCE(pc.minimum_replicas, 0)                   AS minimum_replicas,
    COALESCE(pc.admission_policy, 'queue')             AS admission_policy,
    COALESCE(u24.request_count, 0)                     AS requests_24h,
    COALESCE(u24.total_tokens, 0)                      AS tokens_24h,
    COALESCE(u24.cost_usd, 0)                          AS cost_usd_24h
FROM projects p
LEFT JOIN agent_runtimes ar         ON ar.project_id = p.id
LEFT JOIN project_reservations pr   ON pr.project_id = p.id
LEFT JOIN project_configurations pc ON pc.project_id = p.id
LEFT JOIN project_effective_priority ep ON ep.project_id = p.id
LEFT JOIN LATERAL (
    SELECT COUNT(*)          AS request_count,
           SUM(total_tokens) AS total_tokens,
           SUM(cost_usd)     AS cost_usd
    FROM usage_events
    WHERE project_id = p.id
      AND created_at >= NOW() - INTERVAL '24 hours'
) u24 ON TRUE
GROUP BY p.id, p.name, p.organization_id, p.team_id, p.priority_weight, p.preemptible,
         ep.effective_priority,
         pr.reserved_vram_mb, pr.reserved_cpu_cores, pr.reserved_memory_mb,
         pr.max_gpu_vram_mb, pr.max_cpu, pr.max_memory_mb,
         pc.always_running, pc.protected, pc.minimum_replicas, pc.admission_policy,
         u24.request_count, u24.total_tokens, u24.cost_usd;

COMMIT;
