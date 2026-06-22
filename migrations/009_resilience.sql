-- NexusLLM Migration 009 — Resilience & Lifecycle
-- 1. Runtime resource requirements
-- 2. Node offline detection (extended node states)
-- 3. Runtime health & recovery states
-- 4. Model archive & lifecycle
-- 5. Agent capability reporting (structured)
-- All idempotent.
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. RUNTIME RESOURCE REQUIREMENTS
-- Every model/service declares what it needs before scheduling.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS runtime_requirements (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id         UUID        NOT NULL REFERENCES models(id) ON DELETE CASCADE UNIQUE,

    -- Execution type: GPU or CPU
    execution_type   VARCHAR(20) NOT NULL DEFAULT 'GPU'
                     CHECK (execution_type IN ('GPU', 'CPU', 'ANY')),

    -- GPU requirements (execution_type = GPU)
    required_vram_mb BIGINT      NOT NULL DEFAULT 0,   -- minimum VRAM in MB
    gpu_count        INTEGER     NOT NULL DEFAULT 1,   -- number of GPUs needed

    -- CPU requirements (execution_type = CPU)
    required_cpu     INTEGER     NOT NULL DEFAULT 0,   -- number of CPU cores
    required_memory_mb BIGINT    NOT NULL DEFAULT 0,   -- RAM in MB

    -- Node capability requirements
    requires_docker  BOOLEAN     NOT NULL DEFAULT TRUE,
    requires_gpu     BOOLEAN     NOT NULL DEFAULT FALSE,
    requires_vllm    BOOLEAN     NOT NULL DEFAULT FALSE,
    requires_ollama  BOOLEAN     NOT NULL DEFAULT FALSE,
    requires_tts     BOOLEAN     NOT NULL DEFAULT FALSE,
    requires_whisper BOOLEAN     NOT NULL DEFAULT FALSE,

    -- Priority hint for scheduler
    priority         VARCHAR(20) NOT NULL DEFAULT 'normal'
                     CHECK (priority IN ('critical','high','normal','low','best_effort')),

    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
DROP TRIGGER IF EXISTS set_runtime_req_updated_at ON runtime_requirements;
CREATE TRIGGER set_runtime_req_updated_at
    BEFORE UPDATE ON runtime_requirements
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE INDEX IF NOT EXISTS idx_runtime_req_model ON runtime_requirements(model_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. NODE OFFLINE DETECTION — extended node states
-- ─────────────────────────────────────────────────────────────────────────────

-- Extend nodes.status to include UNHEALTHY and DRAINING
-- (ONLINE, OFFLINE, DEGRADED, MAINTENANCE already exist — add UNHEALTHY)
ALTER TABLE nodes DROP CONSTRAINT IF EXISTS nodes_status_check;
ALTER TABLE nodes ADD CONSTRAINT nodes_status_check
    CHECK (status IN ('online','offline','degraded','maintenance','unknown','unhealthy','draining'));

-- Node health events audit log
CREATE TABLE IF NOT EXISTS node_health_events (
    id          BIGSERIAL   PRIMARY KEY,
    node_id     UUID        NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    from_status VARCHAR(20),
    to_status   VARCHAR(20) NOT NULL,
    reason      TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_node_health_node_time
    ON node_health_events(node_id, created_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. RUNTIME HEALTH & RECOVERY STATES
-- Extended state machine for agent_runtimes.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE agent_runtimes DROP CONSTRAINT IF EXISTS agent_runtimes_state_check;
ALTER TABLE agent_runtimes ADD CONSTRAINT agent_runtimes_state_check
    CHECK (state IN (
        'pending',    -- task dispatched, not yet started
        'pulling',    -- pulling model weights
        'starting',   -- container starting
        'loading',    -- model loading into VRAM
        'warm',       -- loaded, ready
        'active',     -- actively serving requests
        'idle',       -- loaded but no recent traffic
        'unhealthy',  -- health check failed but container running
        'stopping',   -- drain in progress
        'stopped',    -- container stopped cleanly
        'unloaded',   -- weights evicted
        'failed',     -- error during start/load
        'lost',       -- node went OFFLINE while runtime was running
        'archived',   -- administratively archived
        'deleted'     -- fully removed
    ));

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. MODEL ARCHIVE & LIFECYCLE
-- ─────────────────────────────────────────────────────────────────────────────

-- Add lifecycle state to models
ALTER TABLE models ADD COLUMN IF NOT EXISTS lifecycle VARCHAR(20) NOT NULL DEFAULT 'active'
    CHECK (lifecycle IN ('active', 'archived', 'deleted'));

CREATE INDEX IF NOT EXISTS idx_models_lifecycle ON models(lifecycle);

-- Model lifecycle events audit
CREATE TABLE IF NOT EXISTS model_lifecycle_events_v2 (
    id         BIGSERIAL   PRIMARY KEY,
    model_id   UUID        NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    from_state VARCHAR(20),
    to_state   VARCHAR(20) NOT NULL,
    reason     TEXT,
    actor      VARCHAR(100),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_model_lc_events_model
    ON model_lifecycle_events_v2(model_id, created_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. STRUCTURED AGENT CAPABILITIES
-- node_capabilities already exists; extend it with all needed flags.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE node_capabilities ADD COLUMN IF NOT EXISTS has_podman    BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE node_capabilities ADD COLUMN IF NOT EXISTS has_tgi       BOOLEAN NOT NULL DEFAULT FALSE;
-- Extra arbitrary caps stored as JSONB
ALTER TABLE node_capabilities ADD COLUMN IF NOT EXISTS extra         JSONB   NOT NULL DEFAULT '{}';

COMMIT;
