-- NexusLLM Migration 026 — extra_args on model_runtime_configs
--
-- Adds extra_args JSONB column to model_runtime_configs so operators can pass
-- arbitrary flags to the backend at startup (e.g. ["-thk","0"] for Qwen3
-- to disable chain-of-thought thinking mode, or ["--rope-scale","2"] etc.)
-- These are appended verbatim to the docker run command after all other args.

BEGIN;

ALTER TABLE model_runtime_configs
    ADD COLUMN IF NOT EXISTS extra_args JSONB NOT NULL DEFAULT '[]'::jsonb;

COMMIT;
