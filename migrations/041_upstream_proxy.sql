-- NexusLLM Migration 041 — Per-endpoint upstream proxy for cloud AI calls
--
-- Adds an optional upstream_proxy column to model_endpoints.
-- When set, the gateway routes all outbound HTTP calls for that endpoint
-- through the specified proxy (e.g. http://squid.internal:3128).
--
-- Two configurations are supported:
--   1. No proxy  — upstream_proxy is NULL or empty string (local / direct cloud)
--   2. With proxy — upstream_proxy = "http://host:port" or "socks5://host:port"
--
-- A global fallback is also available via the NEXUS_UPSTREAM_PROXY environment
-- variable, which applies to all cloud endpoints that do NOT set their own proxy.
--
-- All statements are idempotent (safe to re-run).
BEGIN;

ALTER TABLE model_endpoints
    ADD COLUMN IF NOT EXISTS upstream_proxy VARCHAR(512) NOT NULL DEFAULT '';

COMMENT ON COLUMN model_endpoints.upstream_proxy IS
    'Optional HTTP/SOCKS5 proxy URL for outbound upstream calls. '
    'Empty = direct connection. Example: http://proxy.corp:3128';

COMMIT;
