-- NexusLLM Migration 011 — Project-based Priority, Resource Reservation, and Preemption
-- Introduces Projects as a first-class hierarchy level between Team and Models.
-- New hierarchy: Organization → Team → Project → Models → Runtimes
-- All statements are idempotent (IF NOT EXISTS / DO NOTHING / ADD COLUMN IF NOT EXISTS).
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. PROJECTS TABLE
-- A Project is the unit of SLA enforcement, resource reservation, and priority
-- scheduling. It belongs to one Team (and transitively to one Organization).
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS projects (
    id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID         NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    team_id         UUID         NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    name            VARCHAR(200) NOT NULL,
    description     VARCHAR(1000) NOT NULL DEFAULT '',
    -- Priority tier: CRITICAL > HIGH > NORMAL > LOW > BEST_EFFORT
    priority        VARCHAR(20)  NOT NULL DEFAULT 'NORMAL'
                    CHECK (priority IN ('CRITICAL','HIGH','NORMAL','LOW','BEST_EFFORT')),
    -- Status: active | inactive | archived
    status          VARCHAR(20)  NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active','inactive','archived')),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    -- Name must be unique within a team
    UNIQUE(team_id, name)
);

CREATE INDEX IF NOT EXISTS idx_projects_org_id   ON projects(organization_id);
CREATE INDEX IF NOT EXISTS idx_projects_team_id  ON projects(team_id);
CREATE INDEX IF NOT EXISTS idx_projects_priority ON projects(priority);
CREATE INDEX IF NOT EXISTS idx_projects_status   ON projects(status);

DROP TRIGGER IF EXISTS set_projects_updated_at ON projects;
CREATE TRIGGER set_projects_updated_at
    BEFORE UPDATE ON projects FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. PROJECT RESERVATIONS
-- A project may declare guaranteed minimum resources.
-- The scheduler subtracts these from available headroom for lower-priority projects.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS project_reservations (
    project_id          UUID    PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    reserved_vram_mb    BIGINT  NOT NULL DEFAULT 0 CHECK (reserved_vram_mb >= 0),
    reserved_cpu_cores  INTEGER NOT NULL DEFAULT 0 CHECK (reserved_cpu_cores >= 0),
    reserved_memory_mb  BIGINT  NOT NULL DEFAULT 0 CHECK (reserved_memory_mb >= 0),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. PROJECT CONFIGURATIONS
-- Per-project runtime protection settings.
-- Controls IdleManager and PreemptionEngine behaviour.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS project_configurations (
    project_id       UUID     PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    always_running   BOOLEAN  NOT NULL DEFAULT FALSE,
    protected        BOOLEAN  NOT NULL DEFAULT FALSE,
    minimum_replicas INTEGER  NOT NULL DEFAULT 0 CHECK (minimum_replicas BETWEEN 0 AND 100),
    -- Admission control policy for when deployment cannot be immediately satisfied
    -- queue: add to Deployment_Queue (default)
    -- preempt_then_queue: attempt preemption first, then queue
    -- reject: immediately reject with HTTP 409
    admission_policy VARCHAR(30) NOT NULL DEFAULT 'queue'
                     CHECK (admission_policy IN ('queue','preempt_then_queue','reject')),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. FK COLUMNS ON MODELS AND AGENT_RUNTIMES
-- Nullable project_id: backward-compatible — existing rows keep project_id = NULL.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE models
    ADD COLUMN IF NOT EXISTS project_id UUID REFERENCES projects(id) ON DELETE SET NULL;

ALTER TABLE agent_runtimes
    ADD COLUMN IF NOT EXISTS project_id UUID REFERENCES projects(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_models_project_id         ON models(project_id);
CREATE INDEX IF NOT EXISTS idx_agent_runtimes_project_id ON agent_runtimes(project_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. PREEMPTION EVENTS
-- Audit log for every preemption evaluation and execution.
-- Both pressure-detection rows (no preempted_runtime_id) and
-- active-preemption rows (with preempted_runtime_id) are stored here.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS preemption_events (
    id                    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id               UUID        REFERENCES nodes(id) ON DELETE SET NULL,
    -- The runtime that was stopped (NULL for pressure-evaluation-only rows)
    preempted_runtime_id  UUID        REFERENCES agent_runtimes(id) ON DELETE SET NULL,
    preempted_project_id  UUID,       -- store even if project is later deleted
    preempted_priority    VARCHAR(20),
    -- The runtime/project that requested resources
    requesting_runtime_id UUID        REFERENCES agent_runtimes(id) ON DELETE SET NULL,
    requesting_project_id UUID,
    requesting_priority   VARCHAR(20),
    -- What triggered the preemption/evaluation
    trigger               VARCHAR(30) NOT NULL
                          CHECK (trigger IN ('gpu_utilization','vram_exhaustion','memory_exhaustion','admission')),
    pressure_value        NUMERIC(8,4),  -- the utilisation/pressure reading that triggered evaluation
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_preemption_events_node        ON preemption_events(node_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_preemption_events_preempted   ON preemption_events(preempted_project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_preemption_events_requesting  ON preemption_events(requesting_project_id, created_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. DEPLOYMENT QUEUE
-- Holds pending deployment requests that cannot be immediately scheduled.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS deployment_queue (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id       UUID        REFERENCES projects(id) ON DELETE CASCADE,
    -- Serialised DeployRuntimePayload (or similar) plus placement hints
    runtime_config   JSONB       NOT NULL DEFAULT '{}',
    priority_score   INTEGER     NOT NULL DEFAULT 50,
    admission_policy VARCHAR(30) NOT NULL DEFAULT 'queue',
    status           VARCHAR(20) NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending','deployed','expired','failed')),
    attempts         INTEGER     NOT NULL DEFAULT 0,
    enqueued_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at       TIMESTAMPTZ,
    last_attempt_at  TIMESTAMPTZ,
    error_msg        TEXT        NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_deploy_queue_pending
    ON deployment_queue(priority_score DESC, enqueued_at ASC)
    WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_deploy_queue_project
    ON deployment_queue(project_id, status);

-- ─────────────────────────────────────────────────────────────────────────────
-- 7. PROJECT CONTEXT ON USAGE_EVENTS
-- Adds project tracking to usage records (backward-compatible NULLs for
-- legacy events that pre-date this migration).
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE usage_events
    ADD COLUMN IF NOT EXISTS project_id       UUID,
    ADD COLUMN IF NOT EXISTS project_name     VARCHAR(200),
    ADD COLUMN IF NOT EXISTS project_priority VARCHAR(20);

CREATE INDEX IF NOT EXISTS idx_usage_events_project
    ON usage_events(project_id, created_at DESC)
    WHERE project_id IS NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 8. PRIORITY SCORE HELPER FUNCTION
-- Returns the integer score for a priority tier.
-- Used by the scheduler and preemption engine.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION project_priority_score(p VARCHAR) RETURNS INTEGER AS $$
BEGIN
    RETURN CASE p
        WHEN 'CRITICAL'    THEN 100
        WHEN 'HIGH'        THEN 75
        WHEN 'NORMAL'      THEN 50
        WHEN 'LOW'         THEN 25
        WHEN 'BEST_EFFORT' THEN 10
        ELSE 50
    END;
END;
$$ LANGUAGE plpgsql IMMUTABLE;

-- ─────────────────────────────────────────────────────────────────────────────
-- 9. PROJECT ANALYTICS VIEW
-- Convenience view for the analytics API — joins preemption + usage counts.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE VIEW project_runtime_summary AS
SELECT
    p.id                                                 AS project_id,
    p.name                                               AS project_name,
    p.priority,
    project_priority_score(p.priority)                   AS priority_score,
    COUNT(ar.id) FILTER (
        WHERE ar.state IN ('active','warm')
    )                                                    AS active_runtime_count,
    COALESCE(pr.reserved_vram_mb, 0)                     AS reserved_vram_mb,
    COALESCE(pr.reserved_cpu_cores, 0)                   AS reserved_cpu_cores,
    COALESCE(pr.reserved_memory_mb, 0)                   AS reserved_memory_mb,
    COALESCE(pc.always_running, FALSE)                   AS always_running,
    COALESCE(pc.protected, FALSE)                        AS protected,
    COALESCE(pc.minimum_replicas, 0)                     AS minimum_replicas,
    COALESCE(pc.admission_policy, 'queue')               AS admission_policy
FROM projects p
LEFT JOIN agent_runtimes ar        ON ar.project_id = p.id
LEFT JOIN project_reservations pr  ON pr.project_id = p.id
LEFT JOIN project_configurations pc ON pc.project_id = p.id
GROUP BY p.id, p.name, p.priority, pr.reserved_vram_mb, pr.reserved_cpu_cores,
         pr.reserved_memory_mb, pc.always_running, pc.protected,
         pc.minimum_replicas, pc.admission_policy;

COMMIT;
