-- NexusLLM Migration 018 — Weighted Priority System
-- Replaces the 5-tier enum priority (CRITICAL/HIGH/NORMAL/LOW/BEST_EFFORT) with a
-- continuous integer weight in [0, 1000]. All existing enum values are migrated to
-- their canonical weight equivalents. Every component — DB, Go, API, UI — must use
-- priority_weight after this migration.
--
-- Mapping:
--   CRITICAL    → 900   (top of production band)
--   HIGH        → 700
--   NORMAL      → 500
--   LOW         → 300
--   BEST_EFFORT → 0
--
-- Idempotent: safe to re-run.
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. ADD priority_weight TO projects
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE projects ADD COLUMN IF NOT EXISTS priority_weight INTEGER NOT NULL DEFAULT 500
    CHECK (priority_weight BETWEEN 0 AND 1000);

-- Back-fill from legacy enum so existing data is preserved
UPDATE projects SET priority_weight =
    CASE priority
        WHEN 'CRITICAL'    THEN 900
        WHEN 'HIGH'        THEN 700
        WHEN 'NORMAL'      THEN 500
        WHEN 'LOW'         THEN 300
        WHEN 'BEST_EFFORT' THEN 0
        ELSE 500
    END
WHERE priority_weight = 500;  -- only rows that haven't been set yet

CREATE INDEX IF NOT EXISTS idx_projects_priority_weight ON projects(priority_weight DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. REMOVE old enum constraint and legacy priority column
-- We keep the column for 1 release as a soft-delete alias, then drop in 019.
-- For now, add a generated/maintained alias that the Go code can stop using.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_priority_check;

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. EXTEND projects WITH scheduler fields
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS preemptible     BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN IF NOT EXISTS max_cpu         INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS max_memory_mb   BIGINT  NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS max_gpu_vram_mb BIGINT  NOT NULL DEFAULT 0;

-- Non-preemptible projects that were formerly CRITICAL or HIGH keep that flag
UPDATE projects SET preemptible = FALSE
WHERE priority IN ('CRITICAL','HIGH') AND preemptible = TRUE;

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. EXTEND project_reservations WITH max quotas
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE project_reservations
    ADD COLUMN IF NOT EXISTS max_cpu         INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS max_memory_mb   BIGINT  NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS max_gpu_vram_mb BIGINT  NOT NULL DEFAULT 0;

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. EFFECTIVE PRIORITY CACHE (computed by scheduler, stored for audit)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS project_effective_priority (
    project_id          UUID    PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    base_weight         INTEGER NOT NULL DEFAULT 500,
    waiting_bonus       INTEGER NOT NULL DEFAULT 0,   -- starvation prevention aging
    reservation_bonus   INTEGER NOT NULL DEFAULT 0,   -- has guaranteed quota
    sla_bonus           INTEGER NOT NULL DEFAULT 0,   -- future SLA tier bonus
    resource_penalty    INTEGER NOT NULL DEFAULT 0,   -- over-quota penalty (negative stored positive)
    effective_priority  INTEGER NOT NULL DEFAULT 500, -- base_weight + bonuses - penalties
    queue_position      INTEGER,                      -- last known queue position
    last_computed_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_effective_priority_score
    ON project_effective_priority(effective_priority DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. EXTEND deployment_queue with weighted priority fields
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE deployment_queue
    ADD COLUMN IF NOT EXISTS priority_weight    INTEGER NOT NULL DEFAULT 500,
    ADD COLUMN IF NOT EXISTS effective_priority INTEGER NOT NULL DEFAULT 500,
    ADD COLUMN IF NOT EXISTS waiting_since      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN IF NOT EXISTS model_name         TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS model_id           UUID,
    ADD COLUMN IF NOT EXISTS required_vram_mb   BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS required_ram_mb    BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS required_cpu       INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS execution_mode     VARCHAR(20) NOT NULL DEFAULT 'auto',
    ADD COLUMN IF NOT EXISTS prefer_node_id     UUID REFERENCES nodes(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS preemption_reason  TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS scheduling_trace   JSONB NOT NULL DEFAULT '[]',
    ADD COLUMN IF NOT EXISTS next_retry_at      TIMESTAMPTZ;

-- Back-fill priority_weight from old priority_score for existing queue rows
UPDATE deployment_queue SET priority_weight = priority_score WHERE priority_weight = 500;
UPDATE deployment_queue SET effective_priority = priority_score WHERE effective_priority = 500;

-- Drop old FIFO index, replace with effective_priority DESC ordering
DROP INDEX IF EXISTS idx_deploy_queue_pending;
CREATE INDEX IF NOT EXISTS idx_deploy_queue_pending
    ON deployment_queue(effective_priority DESC, waiting_since ASC)
    WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS idx_deploy_queue_retry
    ON deployment_queue(next_retry_at)
    WHERE status = 'pending' AND next_retry_at IS NOT NULL;


-- ─────────────────────────────────────────────────────────────────────────────
-- 7. EXTEND preemption_events WITH numeric priority columns
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE preemption_events
    ADD COLUMN IF NOT EXISTS preempted_weight   INTEGER,
    ADD COLUMN IF NOT EXISTS requesting_weight  INTEGER,
    ADD COLUMN IF NOT EXISTS decision_trace     JSONB NOT NULL DEFAULT '{}';

-- Back-fill numeric weights from legacy string priority
UPDATE preemption_events SET preempted_weight =
    CASE preempted_priority
        WHEN 'CRITICAL'    THEN 900
        WHEN 'HIGH'        THEN 700
        WHEN 'NORMAL'      THEN 500
        WHEN 'LOW'         THEN 300
        WHEN 'BEST_EFFORT' THEN 0
        ELSE NULL
    END
WHERE preempted_weight IS NULL AND preempted_priority IS NOT NULL;

UPDATE preemption_events SET requesting_weight =
    CASE requesting_priority
        WHEN 'CRITICAL'    THEN 900
        WHEN 'HIGH'        THEN 700
        WHEN 'NORMAL'      THEN 500
        WHEN 'LOW'         THEN 300
        WHEN 'BEST_EFFORT' THEN 0
        ELSE NULL
    END
WHERE requesting_weight IS NULL AND requesting_priority IS NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 8. EXTEND usage_events WITH priority_weight
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE usage_events
    ADD COLUMN IF NOT EXISTS project_priority_weight INTEGER;

UPDATE usage_events ue
SET project_priority_weight = (
    SELECT p.priority_weight FROM projects p WHERE p.id = ue.project_id
)
WHERE ue.project_priority_weight IS NULL AND ue.project_id IS NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 9. SCHEDULER DECISIONS TABLE (extended)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS scheduler_decisions (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id            UUID NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    model_name          TEXT NOT NULL DEFAULT '',
    runtime_id          UUID REFERENCES agent_runtimes(id) ON DELETE SET NULL,
    node_id             UUID REFERENCES nodes(id) ON DELETE SET NULL,
    project_id          UUID REFERENCES projects(id) ON DELETE SET NULL,
    -- Decision metadata
    decision_type       VARCHAR(30) NOT NULL CHECK (decision_type IN
                          ('placement', 'preemption', 'queue', 'reject', 'reschedule')),
    -- Priority context at decision time
    priority_weight     INTEGER NOT NULL DEFAULT 500,
    effective_priority  INTEGER NOT NULL DEFAULT 500,
    waiting_bonus       INTEGER NOT NULL DEFAULT 0,
    reservation_bonus   INTEGER NOT NULL DEFAULT 0,
    resource_penalty    INTEGER NOT NULL DEFAULT 0,
    -- Placement score
    node_score          NUMERIC(8,4) NOT NULL DEFAULT 0,
    reason              TEXT NOT NULL DEFAULT '',
    decision_trace      JSONB NOT NULL DEFAULT '{}',
    alternatives        JSONB NOT NULL DEFAULT '[]',
    -- Outcome tracking
    outcome             VARCHAR(20) NOT NULL DEFAULT 'pending' CHECK (outcome IN
                          ('pending', 'success', 'failed', 'timeout', 'cancelled')),
    error_msg           TEXT NOT NULL DEFAULT '',
    decided_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at        TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_scheduler_decisions_model
    ON scheduler_decisions(model_id, decided_at DESC);
CREATE INDEX IF NOT EXISTS idx_scheduler_decisions_project
    ON scheduler_decisions(project_id, decided_at DESC)
    WHERE project_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_scheduler_decisions_outcome
    ON scheduler_decisions(outcome, decided_at DESC);


-- ─────────────────────────────────────────────────────────────────────────────
-- 10. REPLACE project_priority_score() WITH weighted version
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION project_priority_score(p VARCHAR) RETURNS INTEGER AS $$
BEGIN
    -- Legacy enum → weight mapping, kept for backward compat with any old call sites
    RETURN CASE p
        WHEN 'CRITICAL'    THEN 900
        WHEN 'HIGH'        THEN 700
        WHEN 'NORMAL'      THEN 500
        WHEN 'LOW'         THEN 300
        WHEN 'BEST_EFFORT' THEN 0
        ELSE 500
    END;
END;
$$ LANGUAGE plpgsql IMMUTABLE;

-- New function: compute effective priority with aging/bonuses/penalties
CREATE OR REPLACE FUNCTION compute_effective_priority(
    p_base_weight      INTEGER,
    p_waiting_secs     DOUBLE PRECISION,   -- seconds in queue
    p_has_reservation  BOOLEAN,
    p_over_quota       BOOLEAN
) RETURNS INTEGER AS $$
DECLARE
    waiting_bonus     INTEGER;
    reservation_bonus INTEGER;
    resource_penalty  INTEGER;
    effective         INTEGER;
BEGIN
    -- Starvation prevention: +1 per 60 seconds waiting, capped at +200
    waiting_bonus := LEAST(FLOOR(p_waiting_secs / 60.0)::INTEGER, 200);

    -- Reservation bonus: projects with guaranteed quota get +50
    reservation_bonus := CASE WHEN p_has_reservation THEN 50 ELSE 0 END;

    -- Over-quota penalty: projects consuming beyond their max quota get -100
    resource_penalty := CASE WHEN p_over_quota THEN 100 ELSE 0 END;

    effective := LEAST(1000, GREATEST(0,
        p_base_weight + waiting_bonus + reservation_bonus - resource_penalty
    ));

    RETURN effective;
END;
$$ LANGUAGE plpgsql IMMUTABLE;

-- ─────────────────────────────────────────────────────────────────────────────
-- 11. PRIORITY LABEL HELPER (for UI display only — not used for scheduling)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION priority_label(w INTEGER) RETURNS TEXT AS $$
BEGIN
    RETURN CASE
        WHEN w >= 950 THEN 'Emergency'
        WHEN w >= 900 THEN 'Production Critical'
        WHEN w >= 800 THEN 'Revenue Critical'
        WHEN w >= 700 THEN 'Core Internal'
        WHEN w >= 500 THEN 'Standard'
        WHEN w >= 300 THEN 'Batch'
        WHEN w >= 100 THEN 'Development'
        WHEN w >= 50  THEN 'Playground'
        ELSE               'Best Effort'
    END;
END;
$$ LANGUAGE plpgsql IMMUTABLE;

-- ─────────────────────────────────────────────────────────────────────────────
-- 12. REPLACE project_runtime_summary VIEW
-- Drop first to avoid "cannot change name of view column" error.
-- ─────────────────────────────────────────────────────────────────────────────
DROP VIEW IF EXISTS project_runtime_summary;
CREATE OR REPLACE VIEW project_runtime_summary AS
SELECT
    p.id                                                AS project_id,
    p.name                                              AS project_name,
    p.priority_weight,
    priority_label(p.priority_weight)                   AS priority_label,
    p.preemptible,
    -- Effective priority (live calculation)
    compute_effective_priority(
        p.priority_weight,
        EXTRACT(EPOCH FROM (NOW() - COALESCE(
            (SELECT MIN(dq.waiting_since) FROM deployment_queue dq
             WHERE dq.project_id = p.id AND dq.status = 'pending'),
            NOW()
        ))),
        COALESCE(pr.reserved_vram_mb, 0) > 0 OR COALESCE(pr.reserved_cpu_cores, 0) > 0,
        FALSE -- over-quota flag computed separately
    )                                                   AS effective_priority,
    -- Active runtimes
    COUNT(ar.id) FILTER (WHERE ar.state IN ('active','warm'))
                                                        AS active_runtime_count,
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
    COALESCE(pc.admission_policy, 'queue')             AS admission_policy
FROM projects p
LEFT JOIN agent_runtimes ar        ON ar.project_id = p.id
LEFT JOIN project_reservations pr  ON pr.project_id = p.id
LEFT JOIN project_configurations pc ON pc.project_id = p.id
GROUP BY p.id, p.name, p.priority_weight, p.preemptible,
         pr.reserved_vram_mb, pr.reserved_cpu_cores, pr.reserved_memory_mb,
         pr.max_gpu_vram_mb, pr.max_cpu, pr.max_memory_mb,
         pc.always_running, pc.protected, pc.minimum_replicas, pc.admission_policy;

COMMIT;
