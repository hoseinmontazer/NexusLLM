-- NexusLLM Migration 017 — Automatic Scheduler
-- Adds tables and columns needed by the automatic placement and scheduling engine.
-- All statements are idempotent (IF NOT EXISTS / DO NOTHING / ADD COLUMN IF NOT EXISTS).
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. SCHEDULER STATE (singleton table for control plane state)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS scheduler_state (
    singleton       BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton = TRUE),
    last_sweep_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    queue_length    INTEGER NOT NULL DEFAULT 0,
    active_deploys  INTEGER NOT NULL DEFAULT 0,
    decisions_total BIGINT NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO scheduler_state (singleton) VALUES (TRUE) ON CONFLICT DO NOTHING;

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. NODE CAPABILITIES (extended for scheduler queries)
-- Cached computed values updated by node agent on every heartbeat.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS node_capabilities (
    node_id         UUID PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    has_gpu         BOOLEAN NOT NULL DEFAULT FALSE,
    gpu_count       INTEGER NOT NULL DEFAULT 0,
    gpu_available   BOOLEAN NOT NULL DEFAULT FALSE,
    gpu_vram_mb     BIGINT NOT NULL DEFAULT 0,
    has_nvlink      BOOLEAN NOT NULL DEFAULT FALSE,
    has_rdma        BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_node_capabilities_has_gpu 
    ON node_capabilities(has_gpu) WHERE has_gpu = TRUE;


-- ─────────────────────────────────────────────────────────────────────────────
-- 3. MODEL REQUIREMENTS (cached computed resource needs)
-- Computed by RequirementsComputer from model_runtime_configs.
-- Speeds up scheduler queries by avoiding JOIN + computation on every decision.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS model_requirements (
    model_id            UUID PRIMARY KEY REFERENCES models(id) ON DELETE CASCADE,
    required_cpu        INTEGER NOT NULL DEFAULT 0,
    required_ram_mb     BIGINT NOT NULL DEFAULT 0,
    required_vram_mb    BIGINT NOT NULL DEFAULT 0,
    required_gpu_count  INTEGER NOT NULL DEFAULT 0,
    execution_mode      VARCHAR(20) NOT NULL DEFAULT 'auto'
                        CHECK (execution_mode IN ('cpu', 'gpu', 'auto')),
    workload_policy     VARCHAR(20) NOT NULL DEFAULT 'lazy_load'
                        CHECK (workload_policy IN ('lazy_load', 'always_on')),
    computed_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_model_requirements_exec_mode 
    ON model_requirements(execution_mode);

CREATE INDEX IF NOT EXISTS idx_model_requirements_policy 
    ON model_requirements(workload_policy);

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. SCHEDULER DECISIONS LOG (audit trail for every placement decision)
-- Records: what was decided, why, alternatives considered, outcome.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS scheduler_decisions (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id            UUID NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    runtime_id          UUID REFERENCES agent_runtimes(id) ON DELETE SET NULL,
    node_id             UUID REFERENCES nodes(id) ON DELETE SET NULL,
    -- Decision metadata
    decision_type       VARCHAR(30) NOT NULL CHECK (decision_type IN 
                          ('placement', 'preemption', 'queue', 'reject', 'reschedule')),
    score               NUMERIC(8,4) NOT NULL DEFAULT 0,
    reason              TEXT NOT NULL DEFAULT '',
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

CREATE INDEX IF NOT EXISTS idx_scheduler_decisions_runtime
    ON scheduler_decisions(runtime_id, decided_at DESC)
    WHERE runtime_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_scheduler_decisions_node
    ON scheduler_decisions(node_id, decided_at DESC)
    WHERE node_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_scheduler_decisions_outcome 
    ON scheduler_decisions(outcome, decided_at DESC);

CREATE INDEX IF NOT EXISTS idx_scheduler_decisions_type
    ON scheduler_decisions(decision_type, decided_at DESC);


-- ─────────────────────────────────────────────────────────────────────────────
-- 5. EXTEND DEPLOYMENT_QUEUE (from migration 011, add scheduler columns)
-- deployment_queue already exists; we extend it with scheduler-specific metadata.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE deployment_queue 
    ADD COLUMN IF NOT EXISTS required_vram_mb   BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS required_ram_mb    BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS required_cpu       INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS execution_mode     VARCHAR(20) NOT NULL DEFAULT 'auto',
    ADD COLUMN IF NOT EXISTS prefer_node_id     UUID REFERENCES nodes(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS last_error         TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS next_retry_at      TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_deploy_queue_retry
    ON deployment_queue(status, next_retry_at)
    WHERE status = 'pending' AND next_retry_at IS NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. EXTEND AGENT_RUNTIMES (scheduler-assigned resources)
-- Add columns to track what resources the scheduler assigned to this runtime.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE agent_runtimes
    ADD COLUMN IF NOT EXISTS placement_decision_id UUID REFERENCES scheduler_decisions(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_agent_runtimes_placement
    ON agent_runtimes(placement_decision_id)
    WHERE placement_decision_id IS NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 7. SCHEDULER METRICS VIEW (real-time capacity dashboard)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE VIEW scheduler_node_metrics AS
SELECT
    n.id AS node_id,
    n.hostname,
    n.status,
    n.total_cpu,
    n.total_ram_mb,
    n.total_vram_mb,
    -- Active runtime count
    COUNT(ar.id) FILTER (WHERE ar.state IN ('active', 'warm', 'ready')) AS active_runtimes,
    -- Resource utilization (from most recent telemetry)
    COALESCE(nt.cpu_util_pct, 0) AS cpu_util_pct,
    COALESCE(nt.ram_used_mb, 0) AS ram_used_mb,
    COALESCE(nt.ram_avail_mb, n.total_ram_mb) AS ram_avail_mb,
    -- GPU utilization (max across all devices)
    COALESCE(MAX(d.utilization_pct), 0) AS max_gpu_util_pct,
    -- Free VRAM (total - used)
    COALESCE(SUM(d.vram_mb), 0) AS total_vram_mb_devices,
    COALESCE(SUM(gt.memory_used_mb), 0) AS used_vram_mb,
    COALESCE(SUM(d.vram_mb) - SUM(COALESCE(gt.memory_used_mb, 0)), 0) AS free_vram_mb,
    -- Last heartbeat
    n.last_heartbeat_at,
    EXTRACT(EPOCH FROM (NOW() - n.last_heartbeat_at)) AS heartbeat_age_secs
FROM nodes n
LEFT JOIN agent_runtimes ar ON ar.node_id = n.id
LEFT JOIN LATERAL (
    SELECT cpu_util_pct, ram_used_mb, ram_avail_mb
    FROM node_telemetry
    WHERE node_id = n.id
    ORDER BY recorded_at DESC
    LIMIT 1
) nt ON TRUE
LEFT JOIN gpu_nodes gn ON gn.node_id = n.id
LEFT JOIN gpu_devices d ON d.node_id = gn.id
LEFT JOIN LATERAL (
    SELECT memory_used_mb
    FROM gpu_telemetry
    WHERE device_id = d.id
    ORDER BY recorded_at DESC
    LIMIT 1
) gt ON TRUE
GROUP BY n.id, n.hostname, n.status, n.total_cpu, n.total_ram_mb, n.total_vram_mb,
         n.last_heartbeat_at, nt.cpu_util_pct, nt.ram_used_mb, nt.ram_avail_mb;


-- ─────────────────────────────────────────────────────────────────────────────
-- 8. HELPER FUNCTIONS FOR SCHEDULER
-- ─────────────────────────────────────────────────────────────────────────────

-- Compute available VRAM on a node (total - used - reserved)
CREATE OR REPLACE FUNCTION scheduler_available_vram_mb(p_node_id UUID)
RETURNS BIGINT AS $$
DECLARE
    total_vram   BIGINT;
    used_vram    BIGINT;
    reserved     BIGINT;
BEGIN
    -- Total VRAM from devices
    SELECT COALESCE(SUM(d.vram_mb), 0)
    INTO total_vram
    FROM gpu_devices d
    JOIN gpu_nodes gn ON gn.id = d.node_id
    WHERE gn.node_id = p_node_id AND d.status = 'available';
    
    -- Used VRAM from telemetry
    SELECT COALESCE(SUM(gt.memory_used_mb), 0)
    INTO used_vram
    FROM gpu_devices d
    JOIN gpu_nodes gn ON gn.id = d.node_id
    LEFT JOIN LATERAL (
        SELECT memory_used_mb FROM gpu_telemetry
        WHERE device_id = d.id ORDER BY recorded_at DESC LIMIT 1
    ) gt ON TRUE
    WHERE gn.node_id = p_node_id;
    
    -- Reserved VRAM from project reservations
    SELECT COALESCE(SUM(pr.reserved_vram_mb), 0)
    INTO reserved
    FROM project_reservations pr
    JOIN agent_runtimes ar ON ar.project_id = pr.project_id
    WHERE ar.node_id = p_node_id
      AND ar.state IN ('ready', 'active', 'warm', 'idle', 'loading_model');
    
    RETURN total_vram - used_vram - reserved;
END;
$$ LANGUAGE plpgsql STABLE;

-- Check if node can accommodate requirement
CREATE OR REPLACE FUNCTION scheduler_can_fit(
    p_node_id UUID,
    p_required_vram_mb BIGINT,
    p_required_ram_mb BIGINT,
    p_required_cpu INTEGER
)
RETURNS BOOLEAN AS $$
DECLARE
    avail_vram BIGINT;
    avail_ram  BIGINT;
    avail_cpu  INTEGER;
BEGIN
    -- Check VRAM
    IF p_required_vram_mb > 0 THEN
        avail_vram := scheduler_available_vram_mb(p_node_id);
        IF avail_vram < p_required_vram_mb THEN
            RETURN FALSE;
        END IF;
    END IF;
    
    -- Check RAM (from latest telemetry)
    IF p_required_ram_mb > 0 THEN
        SELECT ram_avail_mb INTO avail_ram
        FROM node_telemetry
        WHERE node_id = p_node_id
        ORDER BY recorded_at DESC LIMIT 1;
        
        IF COALESCE(avail_ram, 0) < p_required_ram_mb THEN
            RETURN FALSE;
        END IF;
    END IF;
    
    -- Check CPU (total - allocated)
    IF p_required_cpu > 0 THEN
        SELECT n.total_cpu - COALESCE(SUM(ca.cpu_cores), 0)
        INTO avail_cpu
        FROM nodes n
        LEFT JOIN cpu_allocations ca 
            ON ca.node_id = n.id AND ca.released_at IS NULL
        WHERE n.id = p_node_id
        GROUP BY n.id, n.total_cpu;
        
        IF COALESCE(avail_cpu, 0) < p_required_cpu THEN
            RETURN FALSE;
        END IF;
    END IF;
    
    RETURN TRUE;
END;
$$ LANGUAGE plpgsql STABLE;

COMMIT;
