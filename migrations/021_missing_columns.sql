-- NexusLLM Migration 021 — Missing columns catch-up
-- Adds columns that were referenced in code but never added via migrations.
-- All idempotent.
BEGIN;

-- model_runtime_configs.max_model_len — used by vLLM backend
ALTER TABLE model_runtime_configs
    ADD COLUMN IF NOT EXISTS max_model_len INTEGER NOT NULL DEFAULT 0;

COMMIT;
