-- NexusLLM Migration 012 — Unified startup pipeline states
--
-- Extends agent_runtimes.state to include the full unified startup pipeline:
--
--   CREATED → VALIDATING → DOWNLOADING → STARTING →
--   LOADING_MODEL → WAITING_READY → READY
--
-- Also ensures model_runtime_configs.node_id and gpu_devices exist
-- (migration 011_runtime_config_gpu may not have run on all environments).
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. Extend agent_runtimes state constraint
-- ─────────────────────────────────────────────────────────────────────────────
-- Drop the old constraint and replace it with one that includes all pipeline
-- states.  IF NOT EXISTS is not supported for constraints in older Postgres,
-- so we drop first (idempotent via IF EXISTS).
ALTER TABLE agent_runtimes DROP CONSTRAINT IF EXISTS agent_runtimes_state_check;
ALTER TABLE agent_runtimes ADD CONSTRAINT agent_runtimes_state_check
    CHECK (state IN (
        -- Unified startup pipeline (new)
        'created',
        'validating',
        'downloading',
        'starting',
        'loading_model',
        'waiting_ready',
        'ready',
        -- Operational states
        'idle',
        'stopping',
        'stopped',
        -- Legacy / compat states kept for backward compatibility
        'pending',       -- old "created" equivalent
        'pulling',       -- old "downloading" equivalent
        'loading',       -- old "loading_model" equivalent
        'warm',          -- old "ready/idle" equivalent
        'active',        -- old "ready" equivalent
        -- Error / terminal states
        'unhealthy',
        'failed',
        'unloaded',
        'lost',
        'archived',
        'deleted'
    ));

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. Update partial index to cover new ready states
-- ─────────────────────────────────────────────────────────────────────────────
DROP INDEX IF EXISTS idx_agent_runtimes_state_last_used;
CREATE INDEX IF NOT EXISTS idx_agent_runtimes_state_last_used
    ON agent_runtimes(state, last_used_at)
    WHERE state IN ('ready', 'active', 'warm', 'idle', 'loading_model');

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. Ensure model_runtime_configs.node_id exists
--    (from 011_runtime_config_gpu.sql — may not have run)
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE model_runtime_configs
    ADD COLUMN IF NOT EXISTS node_id     UUID REFERENCES nodes(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS gpu_devices JSONB NOT NULL DEFAULT '[]'::jsonb;

COMMIT;
