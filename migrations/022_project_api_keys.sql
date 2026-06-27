-- NexusLLM Migration 022 — Project-aware API Keys
--
-- Problem: API keys are currently team-scoped only. Every request belongs to a
-- Team but has no Project context. This means the scheduler cannot prioritize
-- requests by project, and usage accounting is model-based only.
--
-- Solution:
--   1. Add optional project_id to api_keys so a key can be scoped to a project.
--   2. Denormalize project priority onto api_keys for fast hot-path reads
--      (avoids a JOIN on every request).
--   3. Add project_priority_weight to usage_events Go-accessible column.
--   4. Update the project_runtime_summary view to include request counts.
--
-- Backward-compatible: all new columns are nullable / have defaults.
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. ADD project_id TO api_keys
-- Optional: a key can belong to a specific project, or remain team-only (NULL).
-- When set, the gateway extracts project priority from the key at auth time
-- without an extra DB round-trip.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS project_id        UUID    REFERENCES projects(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS project_name      TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS project_priority_weight INTEGER NOT NULL DEFAULT 500;

CREATE INDEX IF NOT EXISTS idx_api_keys_project_id ON api_keys(project_id)
    WHERE project_id IS NOT NULL;

-- Trigger: keep project_name and project_priority_weight in sync when
-- the project's priority changes. This avoids a JOIN on every auth lookup.
CREATE OR REPLACE FUNCTION sync_api_key_project_priority()
RETURNS TRIGGER AS $$
BEGIN
    -- When a project's priority_weight changes, update all its API keys.
    UPDATE api_keys
    SET project_priority_weight = NEW.priority_weight,
        project_name            = NEW.name
    WHERE project_id = NEW.id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_sync_api_key_project_priority ON projects;
CREATE TRIGGER trg_sync_api_key_project_priority
    AFTER UPDATE OF priority_weight, name ON projects
    FOR EACH ROW EXECUTE FUNCTION sync_api_key_project_priority();

-- Back-fill for any existing project-scoped keys (if any exist)
UPDATE api_keys ak
SET project_name            = p.name,
    project_priority_weight = p.priority_weight
FROM projects p
WHERE ak.project_id = p.id
  AND (ak.project_name = '' OR ak.project_priority_weight = 500);

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. ADD project_priority_weight TO usage_events
-- Already added by migration 018, but ensure the column exists.
-- The Go usage.Event struct now reads this column.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE usage_events
    ADD COLUMN IF NOT EXISTS project_priority_weight INTEGER;

-- Back-fill from projects where we have a project_id
UPDATE usage_events ue
SET project_priority_weight = p.priority_weight
FROM projects p
WHERE ue.project_id = p.id
  AND ue.project_priority_weight IS NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. ADD X-Nexus-Project header lookup helper
-- project_by_slug: lets the gateway resolve a project by slug from the
-- X-Nexus-Project header without a full table scan.
-- ─────────────────────────────────────────────────────────────────────────────
-- We use the existing projects.name as the slug (it is unique within a team).
-- No new column needed; name is already indexed via UNIQUE(team_id, name).

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. EXTEND project_runtime_summary VIEW with usage metrics
-- ─────────────────────────────────────────────────────────────────────────────
DROP VIEW IF EXISTS project_runtime_summary;
CREATE OR REPLACE VIEW project_runtime_summary AS
SELECT
    p.id                                                AS project_id,
    p.name                                              AS project_name,
    p.team_id,
    p.priority_weight,
    priority_label(p.priority_weight)                   AS priority_label,
    p.preemptible,
    COALESCE(ep.effective_priority, p.priority_weight)  AS effective_priority,
    -- Active runtimes
    COUNT(DISTINCT ar.id) FILTER (
        WHERE ar.state IN ('active','warm','ready','idle')
    )                                                   AS active_runtime_count,
    -- Reservations
    COALESCE(pr.reserved_vram_mb, 0)                   AS reserved_vram_mb,
    COALESCE(pr.reserved_cpu_cores, 0)                 AS reserved_cpu_cores,
    COALESCE(pr.reserved_memory_mb, 0)                 AS reserved_memory_mb,
    -- Quotas
    COALESCE(pr.max_gpu_vram_mb, 0)                    AS max_gpu_vram_mb,
    COALESCE(pr.max_cpu, 0)                            AS max_cpu,
    COALESCE(pr.max_memory_mb, 0)                      AS max_memory_mb,
    -- Protection
    COALESCE(pc.always_running, FALSE)                 AS always_running,
    COALESCE(pc.protected, FALSE)                      AS protected,
    COALESCE(pc.minimum_replicas, 0)                   AS minimum_replicas,
    COALESCE(pc.admission_policy, 'queue')             AS admission_policy,
    -- Usage (last 24h) — aggregated for dashboard
    COALESCE(u24.request_count, 0)                     AS requests_24h,
    COALESCE(u24.total_tokens, 0)                      AS tokens_24h,
    COALESCE(u24.cost_usd, 0)                          AS cost_usd_24h
FROM projects p
LEFT JOIN agent_runtimes ar         ON ar.project_id = p.id
LEFT JOIN project_reservations pr   ON pr.project_id = p.id
LEFT JOIN project_configurations pc ON pc.project_id = p.id
LEFT JOIN project_effective_priority ep ON ep.project_id = p.id
LEFT JOIN LATERAL (
    SELECT COUNT(*)            AS request_count,
           SUM(total_tokens)   AS total_tokens,
           SUM(cost_usd)       AS cost_usd
    FROM usage_events
    WHERE project_id = p.id
      AND created_at >= NOW() - INTERVAL '24 hours'
) u24 ON TRUE
GROUP BY p.id, p.name, p.team_id, p.priority_weight, p.preemptible,
         ep.effective_priority,
         pr.reserved_vram_mb, pr.reserved_cpu_cores, pr.reserved_memory_mb,
         pr.max_gpu_vram_mb, pr.max_cpu, pr.max_memory_mb,
         pc.always_running, pc.protected, pc.minimum_replicas, pc.admission_policy,
         u24.request_count, u24.total_tokens, u24.cost_usd;

COMMIT;
