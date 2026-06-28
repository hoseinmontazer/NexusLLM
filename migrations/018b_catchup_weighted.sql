-- Migration 018b — Catch-up for partial 018 application
-- Some DB instances have migration 018 partially applied (index creation
-- for scheduler_decisions.project_id failed because the column didn't exist yet).
-- This migration is idempotent and safe to re-run.
BEGIN;

-- Ensure priority_weight exists on projects (018 may have partially run)
ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS priority_weight INTEGER NOT NULL DEFAULT 500
    CHECK (priority_weight BETWEEN 0 AND 1000);

ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS preemptible     BOOLEAN NOT NULL DEFAULT TRUE;

-- Ensure project_effective_priority table exists (needed by admin service)
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

-- Seed effective priority rows for all existing projects
INSERT INTO project_effective_priority (project_id, base_weight, effective_priority)
SELECT id, COALESCE(priority_weight, 500), COALESCE(priority_weight, 500)
FROM projects
WHERE id NOT IN (SELECT project_id FROM project_effective_priority)
ON CONFLICT DO NOTHING;

-- Ensure scheduler_decisions has project_id (may be missing if 018 partially ran)
ALTER TABLE scheduler_decisions
    ADD COLUMN IF NOT EXISTS project_id UUID REFERENCES projects(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_scheduler_decisions_project
    ON scheduler_decisions(project_id, decided_at DESC)
    WHERE project_id IS NOT NULL;

-- Ensure preemption_events has numeric weight columns
ALTER TABLE preemption_events
    ADD COLUMN IF NOT EXISTS preempted_weight  INTEGER,
    ADD COLUMN IF NOT EXISTS requesting_weight INTEGER;

-- Ensure deployment_queue has the weighted priority columns
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
    ADD COLUMN IF NOT EXISTS preemption_reason  TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS scheduling_trace   JSONB NOT NULL DEFAULT '[]',
    ADD COLUMN IF NOT EXISTS next_retry_at      TIMESTAMPTZ;

-- Re-create the pending queue index (drop first to avoid conflict)
DROP INDEX IF EXISTS idx_deploy_queue_pending;
CREATE INDEX IF NOT EXISTS idx_deploy_queue_pending
    ON deployment_queue(effective_priority DESC, waiting_since ASC)
    WHERE status = 'pending';

COMMIT;
