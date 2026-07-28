-- NexusLLM Migration 044 — External / Cloud Provider Models
--
-- Goal: Make external AI providers (OpenAI, Anthropic, Gemini, Azure OpenAI,
--       OpenRouter, Groq, Together, Mistral, Cohere, DeepSeek) first-class
--       models in the single model registry.
--
-- Architecture contract:
--   • ONE model table. No second table.
--   • A model is backed by a local runtime OR a remote provider.
--   • The gateway cannot tell the difference — routing is identical.
--   • Policy engine, rate limits, quota, audit, and usage tracking are
--     always applied. Nothing bypasses the pipeline.
--
-- Changes:
--   1. Add provider metadata columns to models table (all nullable — local
--      models are unaffected; NULL = local runtime, non-NULL = provider).
--   2. Add provider_is_external flag for fast O(1) routing decisions.
--   3. Add provider_api_version for Azure OpenAI deployments.
--   4. Add provider_extra_config JSONB for provider-specific overrides.
--   5. Add upstream_model_name to model_endpoints if not already present
--      (migration 042 added it — this is a safe no-op).
--   6. Seed provider_name from backend_type for any existing cloud endpoints
--      that already use openai_compat with an upstream_base_url.
--   7. Update the backend_type CHECK constraint to include provider types.
--   8. Update the universal_models view.
--
-- All statements are idempotent (safe to re-run).
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. PROVIDER METADATA on models
-- ─────────────────────────────────────────────────────────────────────────────

-- provider_name: canonical provider identifier matching BackendType.
-- Matches: openai_provider | anthropic_provider | google_provider |
--          azure_openai_provider | openrouter_provider | groq_provider |
--          together_provider | mistral_provider | cohere_provider |
--          deepseek_provider | NULL (local model)
ALTER TABLE models
    ADD COLUMN IF NOT EXISTS provider_name          VARCHAR(64),
    ADD COLUMN IF NOT EXISTS provider_is_external   BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS provider_api_version   VARCHAR(32),
    ADD COLUMN IF NOT EXISTS provider_extra_config  JSONB   NOT NULL DEFAULT '{}';

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. PROVIDER METADATA on model_endpoints
--    upstream_model_name already added by migration 042.
--    Add provider_timeout_seconds and provider_max_retries here so that
--    the registry can apply per-endpoint retry/timeout policies.
-- ─────────────────────────────────────────────────────────────────────────────

ALTER TABLE model_endpoints
    ADD COLUMN IF NOT EXISTS provider_timeout_seconds  INT     NOT NULL DEFAULT 120,
    ADD COLUMN IF NOT EXISTS provider_max_retries      INT     NOT NULL DEFAULT 2,
    ADD COLUMN IF NOT EXISTS provider_extra_headers    JSONB   NOT NULL DEFAULT '{}';

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. UPSTREAM MODEL NAME — idempotent (migration 042 may have added it already)
-- ─────────────────────────────────────────────────────────────────────────────

ALTER TABLE model_endpoints
    ADD COLUMN IF NOT EXISTS upstream_model_name TEXT;

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. SEED provider_is_external and provider_name for any model whose
--    endpoint already has an upstream_base_url set (i.e. existing cloud
--    models registered before this migration).
-- ─────────────────────────────────────────────────────────────────────────────

UPDATE models m
SET provider_is_external = TRUE,
    provider_name = CASE
        WHEN m.backend_type = 'openai_provider'       THEN 'openai_provider'
        WHEN m.backend_type = 'anthropic_provider'    THEN 'anthropic_provider'
        WHEN m.backend_type = 'google_provider'       THEN 'google_provider'
        WHEN m.backend_type = 'azure_openai_provider' THEN 'azure_openai_provider'
        WHEN m.backend_type = 'openrouter_provider'   THEN 'openrouter_provider'
        WHEN m.backend_type = 'groq_provider'         THEN 'groq_provider'
        WHEN m.backend_type = 'together_provider'     THEN 'together_provider'
        WHEN m.backend_type = 'mistral_provider'      THEN 'mistral_provider'
        WHEN m.backend_type = 'cohere_provider'       THEN 'cohere_provider'
        WHEN m.backend_type = 'deepseek_provider'     THEN 'deepseek_provider'
        -- Legacy openai_compat models that have an upstream_base_url
        WHEN m.backend_type = 'openai_compat'
             AND EXISTS (
                 SELECT 1 FROM model_endpoints me
                 WHERE me.model_id = m.id
                   AND me.upstream_base_url IS NOT NULL
                   AND me.upstream_base_url != ''
             )
             THEN 'openai_provider'
        ELSE NULL
    END
WHERE m.provider_is_external = FALSE;

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. UPDATE backend_type CHECK to include all provider types
-- ─────────────────────────────────────────────────────────────────────────────

ALTER TABLE models DROP CONSTRAINT IF EXISTS models_backend_type_check;
ALTER TABLE models ADD CONSTRAINT models_backend_type_check
    CHECK (backend_type IN (
        -- Local / self-hosted
        'vllm', 'tgi', 'llamacpp', 'openai_compat', 'cpu_native',
        -- External / cloud providers
        'openai_provider',
        'anthropic_provider',
        'google_provider',
        'azure_openai_provider',
        'openrouter_provider',
        'groq_provider',
        'together_provider',
        'mistral_provider',
        'cohere_provider',
        'deepseek_provider'
    ));

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. INDEX for fast external-model lookup (gateway hot path)
-- ─────────────────────────────────────────────────────────────────────────────

CREATE INDEX IF NOT EXISTS idx_models_provider_external
    ON models(provider_is_external)
    WHERE provider_is_external = TRUE;

CREATE INDEX IF NOT EXISTS idx_models_provider_name
    ON models(provider_name)
    WHERE provider_name IS NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 7. PROVIDER DEFAULT ENDPOINTS
--    Canonical base URLs for each provider.
--    Stored so the admin UI can pre-fill the endpoint URL field
--    and so the watcher knows where to health-check.
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS provider_defaults (
    provider_name      VARCHAR(64)  PRIMARY KEY,
    display_name       VARCHAR(128) NOT NULL,
    default_base_url   TEXT         NOT NULL,
    health_path        VARCHAR(255) NOT NULL DEFAULT '/v1/models',
    docs_url           TEXT,
    supports_streaming BOOLEAN      NOT NULL DEFAULT TRUE,
    supports_functions BOOLEAN      NOT NULL DEFAULT TRUE,
    supports_vision    BOOLEAN      NOT NULL DEFAULT FALSE,
    supports_embedding BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

INSERT INTO provider_defaults
    (provider_name, display_name, default_base_url, health_path,
     docs_url, supports_streaming, supports_functions, supports_vision, supports_embedding)
VALUES
    ('openai_provider',       'OpenAI',            'https://api.openai.com',
     '/v1/models',            'https://platform.openai.com/docs',    TRUE, TRUE, TRUE,  TRUE),
    ('anthropic_provider',    'Anthropic',          'https://api.anthropic.com',
     '/v1/models',            'https://docs.anthropic.com',          TRUE, TRUE, TRUE,  FALSE),
    ('google_provider',       'Google Gemini',      'https://generativelanguage.googleapis.com',
     '/v1beta/openai/models', 'https://ai.google.dev/docs',          TRUE, TRUE, TRUE,  TRUE),
    ('azure_openai_provider', 'Azure OpenAI',       'https://YOUR_RESOURCE.openai.azure.com',
     '/openai/deployments?api-version=2024-02-01',
     'https://learn.microsoft.com/azure/ai-services/openai',         TRUE, TRUE, TRUE,  TRUE),
    ('openrouter_provider',   'OpenRouter',         'https://openrouter.ai',
     '/api/v1/models',        'https://openrouter.ai/docs',          TRUE, TRUE, FALSE, FALSE),
    ('groq_provider',         'Groq',               'https://api.groq.com',
     '/openai/v1/models',     'https://console.groq.com/docs',       TRUE, TRUE, FALSE, FALSE),
    ('together_provider',     'Together AI',        'https://api.together.xyz',
     '/v1/models',            'https://docs.together.ai',            TRUE, TRUE, FALSE, TRUE),
    ('mistral_provider',      'Mistral AI',         'https://api.mistral.ai',
     '/v1/models',            'https://docs.mistral.ai',             TRUE, TRUE, FALSE, TRUE),
    ('cohere_provider',       'Cohere',             'https://api.cohere.com',
     '/v1/models',            'https://docs.cohere.com',             TRUE, TRUE, FALSE, TRUE),
    ('deepseek_provider',     'DeepSeek',           'https://api.deepseek.com',
     '/v1/models',            'https://platform.deepseek.com/api-docs', TRUE, TRUE, FALSE, FALSE)
ON CONFLICT (provider_name) DO UPDATE SET
    display_name       = EXCLUDED.display_name,
    default_base_url   = EXCLUDED.default_base_url,
    health_path        = EXCLUDED.health_path,
    docs_url           = EXCLUDED.docs_url,
    supports_streaming = EXCLUDED.supports_streaming,
    supports_functions = EXCLUDED.supports_functions,
    supports_vision    = EXCLUDED.supports_vision,
    supports_embedding = EXCLUDED.supports_embedding;

-- ─────────────────────────────────────────────────────────────────────────────
-- 8. REFRESH universal_models view (adds provider columns)
-- ─────────────────────────────────────────────────────────────────────────────

DROP VIEW IF EXISTS universal_models;
CREATE OR REPLACE VIEW universal_models AS
SELECT
    m.id,
    m.name,
    m.display_name,
    m.provider,
    m.backend_type,
    m.service_type,
    m.capabilities,
    m.max_context,
    m.max_output,
    m.enabled,
    COALESCE(m.lifecycle, 'active')       AS lifecycle,
    m.provider_is_external,
    m.provider_name,
    m.provider_api_version,
    m.provider_extra_config,
    m.supports_thinking,
    m.thinking_enabled,
    m.min_thinking_tokens,
    m.created_at,
    m.updated_at,
    -- Endpoint summary
    COUNT(me.id)                                                   AS endpoint_count,
    COUNT(me.id) FILTER (WHERE me.health_status = 'healthy')       AS healthy_endpoint_count,
    COUNT(me.id) FILTER (WHERE me.is_enabled = TRUE)               AS enabled_endpoint_count,
    -- Cloud endpoint details (first enabled endpoint)
    MIN(me.upstream_base_url)                                      AS upstream_base_url,
    BOOL_OR(me.upstream_api_key IS NOT NULL
            AND me.upstream_api_key != '')                         AS has_upstream_key,
    MIN(me.upstream_model_name)                                    AS upstream_model_name,
    MIN(me.provider_timeout_seconds)                               AS provider_timeout_seconds,
    MIN(me.provider_max_retries)                                   AS provider_max_retries
FROM models m
LEFT JOIN model_endpoints me ON me.model_id = m.id AND me.is_enabled = TRUE
WHERE COALESCE(m.lifecycle, 'active') != 'deleted'
GROUP BY m.id;

COMMIT;
