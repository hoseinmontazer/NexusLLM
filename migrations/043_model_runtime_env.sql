-- NexusLLM Migration 043 — env vars for model_runtime_configs
--
-- Adds an `env` JSONB column to model_runtime_configs so operators can
-- configure environment variables for lazy-loaded containers.
--
-- These env vars are passed to the container at startup exactly as configured,
-- allowing per-model customisation of CPU-native services like faster-whisper:
--
--   {
--     "WHISPER__MODEL":            "Systran/faster-whisper-large-v3",
--     "WHISPER__INFERENCE_DEVICE": "cpu",
--     "UVICORN_PORT":              "8100"
--   }
--
-- The PORT env var is always overridden by the agent after port scanning,
-- so operators should use UVICORN_PORT (or the service-specific var) to
-- set a preferred starting port — the agent will scan forward if it's busy.
--
-- All statements are idempotent (safe to re-run).
BEGIN;

ALTER TABLE model_runtime_configs
    ADD COLUMN IF NOT EXISTS env JSONB NOT NULL DEFAULT '{}'::jsonb;

COMMENT ON COLUMN model_runtime_configs.env IS
    'Environment variables to pass to the container at startup. '
    'Keys and values are strings. Example: {"UVICORN_PORT":"8100","WHISPER__MODEL":"Systran/faster-whisper-large-v3"}. '
    'The agent always overrides PORT after port scanning, so use the service-specific '
    'port var (e.g. UVICORN_PORT) to request a preferred port.';

-- Fix any rows that got NULL instead of empty object.
UPDATE model_runtime_configs
SET env = '{}'::jsonb
WHERE env IS NULL
   OR jsonb_typeof(env) != 'object';

COMMIT;
