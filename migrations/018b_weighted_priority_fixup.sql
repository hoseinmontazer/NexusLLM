-- NexusLLM Migration 018b — Weighted Priority Fix-up
-- Applies the missing columns/objects from 018 that failed due to the
-- scheduler_decisions.project_id index referencing a column that didn't exist yet
-- in the old table. Safe to re-run (all statements are idempotent).
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. projects — add missing columns
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS priority_weight INTEGER NOT NULL DEFAULT 500
        CHECK (priority_weight BETWEEN 0 AND 1000),
    ADD COLUMN IF NOT EXISTS preemptible     BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN IF NOT EXISTS max_cpu         INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS max_memory_mb   BIGINT  NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS max_gpu_vram_mb BIGINT  NOT NULL DEFAULT 0;

-- Back-fill from legacy enum
UPDATE projects SET priority_weight =
    CASE priority
        WHEN 'CRITICAL'    THEN 900
        WHEN 'HIGH'        THEN 700
        WHEN 'NORMAL'      THEN 500
        WHEN 'LOW'         THEN 300
        WHEN 'BEST_EFFORT' THEN 0
        ELSE 500
    END
WHERE priority_weight = 500;

UPDATE projects SET preemptible = FALSE
WHERE priority IN ('CRITICAL','HIGH') AND preemptible = TRUE;

ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_priority_check;

CREATE INDEX IF NOT EXISTS idx_projects_priority_weight ON projects(priority_weight DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. project_reservations — add max quota columns
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE project_reservations
    ADD COLUMN IF NOT EXISTS max_cpu         INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS max_memory_mb   BIGINT  NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS max_gpu_vram_mb BIGINT  NOT NULL DEFAULT 0;

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. project_effective_priority table
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS project_effective_priority (
    project_id          UUID    PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    base_weight         INTEGER NOT NULL DEFAULT 500,
    waiting_bonus       INTEGER NOT NULL DEFAULT 0,
    reservation_bonus   INTEGER NOT NULL DEFAULT 0,
    sla_bonus           INTEGER NOT NULL DEFAULT 0,
    resource_penalty    INTEGER NOT NULL DEFAULT 0,
    effective_priority  INTEGER NOT NULL DEFAULT 500,
    queue_position      INTEGER,
    last_computed_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_effective_priority_score
    ON project_effective_priority(effective_priority DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. deployment_queue — add weighted priority fields
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

UPDATE deployment_queue SET priority_weight = priority_score WHERE priority_weight = 500;
UPDATE deployment_queue SET effective_priority = priority_score WHERE effective_priority = 500;

DROP INDEX IF EXISTS idx_deploy_queue_pending;
CREATE INDEX IF NOT EXISTS idx_deploy_queue_pending
    ON deployment_queue(effective_priority DESC, waiting_since ASC)
    WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS idx_deploy_queue_retry
    ON deployment_queue(next_retry_at)
    WHERE status = 'pending' AND next_retry_at IS NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. preemption_events — add numeric weight columns
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE preemption_events
    ADD COLUMN IF NOT EXISTS preempted_weight   INTEGER,
    ADD COLUMN IF NOT EXISTS requesting_weight  INTEGER,
    ADD COLUMN IF NOT EXISTS decision_trace     JSONB NOT NULL DEFAULT '{}';

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
-- 6. usage_events — add project_priority_weight
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE usage_events
    ADD COLUMN IF NOT EXISTS project_priority_weight INTEGER;

UPDATE usage_events ue
SET project_priority_weight = (
    SELECT p.priority_weight FROM projects p WHERE p.id = ue.project_id
)
WHERE ue.project_priority_weight IS NULL AND ue.project_id IS NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 7. scheduler_decisions — add missing columns to existing table
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE scheduler_decisions
    ADD COLUMN IF NOT EXISTS model_name         TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS project_id         UUID REFERENCES projects(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS priority_weight    INTEGER NOT NULL DEFAULT 500,
    ADD COLUMN IF NOT EXISTS effective_priority INTEGER NOT NULL DEFAULT 500,
    ADD COLUMN IF NOT EXISTS waiting_bonus      INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS reservation_bonus  INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS resource_penalty   INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS node_score         NUMERIC(8,4) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS decision_trace     JSONB NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS decision_type      VARCHAR(30);

-- Set a default for decision_type on existing rows, then add constraint
UPDATE scheduler_decisions SET decision_type = 'placement' WHERE decision_type IS NULL;
ALTER TABLE scheduler_decisions ALTER COLUMN decision_type SET NOT NULL;
ALTER TABLE scheduler_decisions DROP CONSTRAINT IF EXISTS scheduler_decisions_decision_type_check;
ALTER TABLE scheduler_decisions ADD CONSTRAINT scheduler_decisions_decision_type_check
    CHECK (decision_type IN ('placement', 'preemption', 'queue', 'reject', 'reschedule'));

-- Now add the indexes that failed before
CREATE INDEX IF NOT EXISTS idx_scheduler_decisions_project
    ON scheduler_decisions(project_id, decided_at DESC)
    WHERE project_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_scheduler_decisions_outcome
    ON scheduler_decisions(outcome, decided_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 8. Helper functions
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION project_priority_score(p VARCHAR) RETURNS INTEGER AS $$
BEGIN
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

CREATE OR REPLACE FUNCTION compute_effective_priority(
    p_base_weight      INTEGER,
    p_waiting_secs     DOUBLE PRECISION,
    p_has_reservation  BOOLEAN,
    p_over_quota       BOOLEAN
) RETURNS INTEGER AS $$
DECLARE
    waiting_bonus     INTEGER;
    reservation_bonus INTEGER;
    resource_penalty  INTEGER;
    effective         INTEGER;
BEGIN
    waiting_bonus     := LEAST(FLOOR(p_waiting_secs / 60.0)::INTEGER, 200);
    reservation_bonus := CASE WHEN p_has_reservation THEN 50 ELSE 0 END;
    resource_penalty  := CASE WHEN p_over_quota THEN 100 ELSE 0 END;
    effective := LEAST(1000, GREATEST(0,
        p_base_weight + waiting_bonus + reservation_bonus - resource_penalty
    ));
    RETURN effective;
END;
$$ LANGUAGE plpgsql IMMUTABLE;

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
-- 9. project_runtime_summary view
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
