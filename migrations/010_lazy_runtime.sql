-- NexusLLM Migration 010 — Lazy-Load Runtime Manager
--
-- Adds columns needed by the runtimemgr package:
--   1. model_runtime_configs — llama.cpp GGUF source + idle timeout
--   2. agent_runtimes        — last_used_at for idle tracking
--   3. agent_runtimes        — bind_host / error_msg columns
--
-- All statements are idempotent (IF NOT EXISTS / DO NOTHING).
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. model_runtime_configs — llama.cpp source and idle config
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE model_runtime_configs
    ADD COLUMN IF NOT EXISTS gguf_path         TEXT,          -- /models/gemma2-2b-Q4_K_M.gguf
    ADD COLUMN IF NOT EXISTS hf_repo           TEXT,          -- bartowski/gemma-2-2b-it-GGUF
    ADD COLUMN IF NOT EXISTS hf_file           TEXT,          -- gemma-2-2b-it-Q4_K_M.gguf
    ADD COLUMN IF NOT EXISTS hf_token          TEXT,          -- HF token for gated repos
    ADD COLUMN IF NOT EXISTS ctx_size          INTEGER NOT NULL DEFAULT 4096,
    ADD COLUMN IF NOT EXISTS n_gpu_layers      INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS cpu_threads       INTEGER,       -- NULL = auto-detect
    ADD COLUMN IF NOT EXISTS memory_limit      TEXT,          -- docker --memory e.g. "8g"
    ADD COLUMN IF NOT EXISTS models_volume     TEXT,          -- named vol or host path
    ADD COLUMN IF NOT EXISTS idle_timeout_secs INTEGER;       -- NULL = use cluster default

-- Ensure all text columns that may be NULL are actually nullable (idempotent)
ALTER TABLE model_runtime_configs
    ALTER COLUMN gguf_path     DROP NOT NULL,
    ALTER COLUMN hf_repo       DROP NOT NULL,
    ALTER COLUMN hf_file       DROP NOT NULL,
    ALTER COLUMN hf_token      DROP NOT NULL,
    ALTER COLUMN memory_limit  DROP NOT NULL,
    ALTER COLUMN models_volume DROP NOT NULL,
    ALTER COLUMN cpu_threads   DROP NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. agent_runtimes — idle tracking and runtime details
-- ─────────────────────────────────────────────────────────────────────────────
-- bind_host and bind_port already exist in migration 007.
-- We only add the new columns.
ALTER TABLE agent_runtimes
    ADD COLUMN IF NOT EXISTS last_used_at  TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS error_msg     TEXT         NOT NULL DEFAULT '';

-- Index for idle manager queries (state + last_used_at).
CREATE INDEX IF NOT EXISTS idx_agent_runtimes_state_last_used
    ON agent_runtimes(state, last_used_at)
    WHERE state IN ('active','warm','idle');

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. Extend agent_runtimes state constraint to include lazy-load states
-- ─────────────────────────────────────────────────────────────────────────────
-- Migration 009 already extended this; we add 'downloading' for the
-- runtimemgr's PULL_MODEL-in-progress state.
ALTER TABLE agent_runtimes DROP CONSTRAINT IF EXISTS agent_runtimes_state_check;
ALTER TABLE agent_runtimes ADD CONSTRAINT agent_runtimes_state_check
    CHECK (state IN (
        'pending','pulling','starting','loading','warm','active','idle',
        'unhealthy','stopping','stopped','unloaded','failed',
        'lost','archived','deleted','downloading'
    ));

COMMIT;
