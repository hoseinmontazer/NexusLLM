-- NexusLLM Migration 042 — upstream_model_name for cloud model routing
--
-- Cloud providers (OpenRouter, OpenAI, Anthropic, etc.) expect the `model`
-- field in the request body to be their own model ID, not NexusLLM's internal
-- name.  Example: OpenRouter needs "meta-llama/llama-3.1-405b-instruct" but
-- the NexusLLM model might be named "llama-3-405b".
--
-- When upstream_model_name is set on an endpoint, the gateway substitutes it
-- into req.model before forwarding to the upstream, then restores the original
-- name in the response so clients always see the NexusLLM model name.
--
-- When empty (default), behaviour is unchanged — req.model is forwarded as-is.
--
-- All statements are idempotent (safe to re-run).
BEGIN;

ALTER TABLE model_endpoints
    ADD COLUMN IF NOT EXISTS upstream_model_name VARCHAR(512) NOT NULL DEFAULT '';

COMMENT ON COLUMN model_endpoints.upstream_model_name IS
    'Model ID to send to the upstream provider in req.model. '
    'Empty = forward NexusLLM model name unchanged. '
    'Example: "meta-llama/llama-3.1-405b-instruct" for OpenRouter.';

COMMIT;
