-- NexusLLM Migration 015 — Catch-up schema for all columns added in 011–014
--
-- Run this on any environment that may have missed individual migrations.
-- Every statement uses ADD COLUMN IF NOT EXISTS / DROP CONSTRAINT IF EXISTS
-- so it is safe to run multiple times.
BEGIN;

-- ── model_runtime_configs ─────────────────────────────────────────────────────
-- Added by 011_runtime_config_gpu.sql
ALTER TABLE model_runtime_configs
    ADD COLUMN IF NOT EXISTS gpu_devices  JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS node_id      UUID  REFERENCES nodes(id) ON DELETE SET NULL;

-- Added by 014_execution_mode.sql
ALTER TABLE model_runtime_configs
    ADD COLUMN IF NOT EXISTS execution_mode VARCHAR(10) NOT NULL DEFAULT 'auto';

ALTER TABLE model_runtime_configs
    DROP CONSTRAINT IF EXISTS model_runtime_configs_execution_mode_check;
ALTER TABLE model_runtime_configs
    ADD CONSTRAINT model_runtime_configs_execution_mode_check
        CHECK (execution_mode IN ('cpu', 'gpu', 'auto'));

-- ── agent_runtimes ────────────────────────────────────────────────────────────
-- Added by 014_execution_mode.sql
ALTER TABLE agent_runtimes
    ADD COLUMN IF NOT EXISTS requested_mode VARCHAR(10) NOT NULL DEFAULT 'auto',
    ADD COLUMN IF NOT EXISTS effective_mode VARCHAR(10) NOT NULL DEFAULT 'cpu';

ALTER TABLE agent_runtimes
    DROP CONSTRAINT IF EXISTS agent_runtimes_requested_mode_check;
ALTER TABLE agent_runtimes
    DROP CONSTRAINT IF EXISTS agent_runtimes_effective_mode_check;
ALTER TABLE agent_runtimes
    ADD CONSTRAINT agent_runtimes_requested_mode_check
        CHECK (requested_mode IN ('cpu', 'gpu', 'auto')),
    ADD CONSTRAINT agent_runtimes_effective_mode_check
        CHECK (effective_mode IN ('cpu', 'gpu'));

-- ── agent_runtimes state constraint — include all pipeline states ─────────────
-- Added by 012_unified_startup_states.sql
ALTER TABLE agent_runtimes DROP CONSTRAINT IF EXISTS agent_runtimes_state_check;
ALTER TABLE agent_runtimes ADD CONSTRAINT agent_runtimes_state_check
    CHECK (state IN (
        'created', 'validating', 'downloading', 'starting',
        'loading_model', 'waiting_ready', 'ready',
        'idle', 'stopping', 'stopped',
        'pending', 'pulling', 'loading', 'warm', 'active',
        'unhealthy', 'failed', 'unloaded', 'lost', 'archived', 'deleted'
    ));

-- ── agent_tasks task_type constraint — include START_MODEL ────────────────────
-- Added by 013_start_model_task_type.sql
ALTER TABLE agent_tasks DROP CONSTRAINT IF EXISTS agent_tasks_task_type_check;
ALTER TABLE agent_tasks ADD CONSTRAINT agent_tasks_task_type_check
    CHECK (task_type IN (
        'START_MODEL',
        'DEPLOY_RUNTIME', 'STOP_RUNTIME', 'RESTART_RUNTIME',
        'DELETE_RUNTIME', 'WARM_RUNTIME', 'UNLOAD_RUNTIME',
        'PULL_MODEL', 'DELETE_MODEL', 'VERIFY_MODEL',
        'COLLECT_INVENTORY', 'HEALTH_CHECK'
    ));

-- ── node_capabilities ─────────────────────────────────────────────────────────
-- Added by 014_execution_mode.sql
ALTER TABLE node_capabilities
    ADD COLUMN IF NOT EXISTS gpu_available BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS gpu_vram_mb   BIGINT  NOT NULL DEFAULT 0;

UPDATE node_capabilities SET gpu_available = (gpu_count > 0) WHERE gpu_available = FALSE AND gpu_count > 0;

COMMIT;
