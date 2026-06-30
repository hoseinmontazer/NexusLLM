-- NexusLLM Migration 026 — extra_args on model_runtime_configs
--
-- Adds extra_args JSONB column to model_runtime_configs so operators can pass
-- arbitrary flags to the backend at startup (e.g. ["-thk","0"] for Qwen3
-- to disable chain-of-thought thinking mode, or ["--rope-scale","2"] etc.)
-- These are appended verbatim to the docker run command after all other args.

BEGIN;

ALTER TABLE model_runtime_configs
    ADD COLUMN IF NOT EXISTS extra_args JSONB NOT NULL DEFAULT '[]'::jsonb;

-- Fix rows that got '{}' (empty object) instead of '[]' (empty array).
-- This can happen when the column was added to a table that already had rows
-- with a non-null default constraint, or if an INSERT omitted the column.
UPDATE model_runtime_configs
SET extra_args = '[]'::jsonb
WHERE extra_args IS NULL
   OR jsonb_typeof(extra_args) != 'array';

COMMIT;
