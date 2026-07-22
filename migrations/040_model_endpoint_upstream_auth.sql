-- NexusLLM Migration 040 — Upstream API Key for External / Cloud Models
--
-- Goal: Allow cloud model endpoints (OpenAI API, Google Gemini, Anthropic, etc.)
--       to be registered directly in NexusLLM without requiring an intermediate
--       proxy. The gateway injects the upstream_api_key as Authorization: Bearer
--       when forwarding requests to the backend.
--
-- This enables the pattern:
--   1. Register cloud model: POST /admin/v1/models with upstream_api_key
--   2. Grant to team
--   3. Client uses NexusLLM key — gateway injects cloud key transparently
--
-- Security: upstream_api_key is stored in plain text in the DB (same as
--   hf_token in model_runtime_configs). Never expose it in API responses.
--   Use environment-level DB encryption or a secrets manager for production.
--
-- All statements are idempotent (safe to re-run).

BEGIN;

-- Add upstream_api_key to model_endpoints.
-- NULL = no upstream auth (local models, default behaviour unchanged).
ALTER TABLE model_endpoints
    ADD COLUMN IF NOT EXISTS upstream_api_key TEXT;

-- Add upstream_base_url so the gateway can route to a cloud API's full URL
-- instead of constructing http://host:port.
-- NULL = use existing host:port construction (default behaviour unchanged).
ALTER TABLE model_endpoints
    ADD COLUMN IF NOT EXISTS upstream_base_url TEXT;

-- Index for fast lookup (gateway reads this on every request resolution).
-- Partial index — only rows that actually have a key set.
CREATE INDEX IF NOT EXISTS idx_model_endpoints_has_upstream_key
    ON model_endpoints(id)
    WHERE upstream_api_key IS NOT NULL;

COMMIT;
