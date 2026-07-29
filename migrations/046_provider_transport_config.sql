-- NexusLLM Migration 046 — Per-Provider HTTP Transport Configuration
--
-- Goal: Allow each external model endpoint to carry its own HTTP transport
--       settings (proxy, TLS, timeouts, connection pool).
--
-- Architecture:
--   - One HTTP client per endpoint, built at registry load time from these columns.
--   - Changing one provider's proxy never affects any other provider.
--   - No HTTP_PROXY / HTTPS_PROXY env vars are consulted — all config is DB-driven.
--   - Local (self-hosted) models are unaffected; all new columns default to zero /
--     NULL which causes BuildProviderClient() to apply safe production defaults.
--
-- Column naming: all columns are prefixed provider_ to avoid confusion with the
-- existing upstream_proxy (legacy col added by migration 041, kept for backward
-- compat). The registry prefers provider_proxy_url and falls back to upstream_proxy.
--
-- All statements are idempotent (safe to re-run).

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- Proxy settings
-- ─────────────────────────────────────────────────────────────────────────────

-- provider_proxy_url: full proxy URL for this endpoint only.
-- Supported schemes: http://, https://, socks5://.
-- Credentials may be embedded: http://user:pass@proxy.corp:3128
-- NULL = direct connection (no proxy). HTTP_PROXY env var is NEVER used.
ALTER TABLE model_endpoints
    ADD COLUMN IF NOT EXISTS provider_proxy_url TEXT;

-- ─────────────────────────────────────────────────────────────────────────────
-- TLS settings
-- ─────────────────────────────────────────────────────────────────────────────

-- provider_tls_insecure_skip_verify: disable server cert verification.
-- USE ONLY in controlled corporate environments with MITM proxies.
-- Default: FALSE (always verify).
ALTER TABLE model_endpoints
    ADD COLUMN IF NOT EXISTS provider_tls_insecure_skip_verify BOOLEAN NOT NULL DEFAULT FALSE;

-- provider_tls_root_ca_pem: PEM-encoded root CA bundle to trust in addition
-- to system roots. Used when a corporate proxy presents a self-signed cert.
-- NULL = use system root CAs only.
ALTER TABLE model_endpoints
    ADD COLUMN IF NOT EXISTS provider_tls_root_ca_pem TEXT;

-- ─────────────────────────────────────────────────────────────────────────────
-- Timeout settings (all in seconds; 0 = use BuildProviderClient() defaults)
-- ─────────────────────────────────────────────────────────────────────────────

-- TCP dial + TLS handshake timeout. Default when 0: 10 s.
ALTER TABLE model_endpoints
    ADD COLUMN IF NOT EXISTS provider_connect_timeout_seconds INT NOT NULL DEFAULT 0;

-- Non-streaming response body read timeout. Default when 0: unlimited.
-- Streaming responses (SSE) are never limited by this — context deadline applies.
ALTER TABLE model_endpoints
    ADD COLUMN IF NOT EXISTS provider_read_timeout_seconds INT NOT NULL DEFAULT 0;

-- Keep-alive idle connection pool timeout. Default when 0: 90 s.
ALTER TABLE model_endpoints
    ADD COLUMN IF NOT EXISTS provider_idle_conn_timeout_seconds INT NOT NULL DEFAULT 0;

-- Max wait for response headers after request is sent. Default when 0: 30 s.
-- Set to -1 to disable entirely (not recommended for non-streaming calls).
ALTER TABLE model_endpoints
    ADD COLUMN IF NOT EXISTS provider_response_header_timeout_seconds INT NOT NULL DEFAULT 0;

-- ─────────────────────────────────────────────────────────────────────────────
-- Connection pool settings (0 = use BuildProviderClient() defaults)
-- ─────────────────────────────────────────────────────────────────────────────

-- Maximum idle keep-alive connections per host in the pool. Default when 0: 32.
ALTER TABLE model_endpoints
    ADD COLUMN IF NOT EXISTS provider_max_idle_conns_per_host INT NOT NULL DEFAULT 0;

-- Maximum total connections (idle + active) per host. 0 = unlimited.
ALTER TABLE model_endpoints
    ADD COLUMN IF NOT EXISTS provider_max_conns_per_host INT NOT NULL DEFAULT 0;

-- ─────────────────────────────────────────────────────────────────────────────
-- Protocol settings
-- ─────────────────────────────────────────────────────────────────────────────

-- Disable HTTP/2 negotiation. Set TRUE only for providers with known HTTP/2 issues.
-- Default: FALSE (HTTP/2 enabled when server advertises it via ALPN).
ALTER TABLE model_endpoints
    ADD COLUMN IF NOT EXISTS provider_disable_http2 BOOLEAN NOT NULL DEFAULT FALSE;

-- ─────────────────────────────────────────────────────────────────────────────
-- Index: fast lookup for provider endpoints that have a proxy configured
-- ─────────────────────────────────────────────────────────────────────────────

CREATE INDEX IF NOT EXISTS idx_model_endpoints_provider_proxy
    ON model_endpoints(id)
    WHERE provider_proxy_url IS NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- Comment documentation on the universal_models view column
-- (view itself is not recreated here — transport config is on model_endpoints)
-- ─────────────────────────────────────────────────────────────────────────────

COMMENT ON COLUMN model_endpoints.provider_proxy_url IS
    'Outbound proxy for this provider endpoint only. Supported schemes: http, https, socks5. Credentials may be embedded (http://user:pass@host:port). NULL = direct connection. HTTP_PROXY env var is never consulted.';

COMMENT ON COLUMN model_endpoints.provider_tls_insecure_skip_verify IS
    'Disable TLS certificate verification. Use only behind corporate MITM proxies. Never set TRUE against public provider APIs.';

COMMENT ON COLUMN model_endpoints.provider_tls_root_ca_pem IS
    'PEM-encoded root CA bundle appended to system roots. Use when corporate proxy presents a self-signed certificate.';

COMMIT;
