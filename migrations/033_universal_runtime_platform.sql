-- NexusLLM Migration 033 — Universal Runtime Platform
--
-- Goal: Make every AI workload (LLM, STT, TTS, OCR, Embedding, Rerank,
--       Vision, ImageGen, ...) a first-class managed model.
--       Eliminate the architectural split between "models" and "services".
--
-- Changes:
--   1. Expand service_type CHECK to include future workload types
--   2. Fix models.backend_type default (was 'vllm' — wrong for non-LLM)
--   3. Add model capabilities column (what API endpoints this model supports)
--   4. Add universal deployment fields to model_runtime_configs
--   5. Add health_path + api_base_path to model_endpoints for adapter routing
--   6. Relax node_capabilities to be generic (JSONB-based)
--   7. Migrate existing service_type CHAT models → capabilities includes 'chat'
--
-- All statements are idempotent (safe to re-run).
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. EXPAND service_type to cover all current and future AI workload types
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE models DROP CONSTRAINT IF EXISTS models_service_type_check;
ALTER TABLE models ADD CONSTRAINT models_service_type_check
    CHECK (service_type IN (
        -- Current types
        'CHAT',
        'EMBEDDING',
        'RERANK',
        'STT',
        'TTS',
        'OCR',
        'AGENT',
        'MCP',
        -- New types (pluggable — add here as needed)
        'VISION',
        'IMAGE_GENERATION',
        'MODERATION',
        'TRANSLATION',
        'SPEECH_TRANSLATION',
        'VIDEO_GENERATION',
        'AUDIO_GENERATION',
        'CODE_EXECUTION',
        'CUSTOM'
    ));

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. FIX models.backend_type default
--    'vllm' was wrong for STT/TTS/OCR/Embedding workloads.
--    'openai_compat' is the universal safe default — every backend that exposes
--    a /v1/* HTTP API is compatible with it.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE models ALTER COLUMN backend_type SET DEFAULT 'openai_compat';

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. MODEL CAPABILITIES
--    Each model declares which API endpoints it supports.
--    The gateway validates the requested endpoint against model capabilities
--    before routing — prevents routing a chat request to a whisper model.
--
--    Capability identifiers (match the Gateway route names):
--      chat              — POST /v1/chat/completions
--      completion        — POST /v1/completions (legacy)
--      embedding         — POST /v1/embeddings
--      rerank            — POST /v1/rerank
--      transcription     — POST /v1/audio/transcriptions
--      speech            — POST /v1/audio/speech
--      ocr               — POST /v1/ocr  (and /v1/images/ocr future)
--      vision            — POST /v1/chat/completions with image input
--      image_generation  — POST /v1/images/generations
--      moderation        — POST /v1/moderations
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE models ADD COLUMN IF NOT EXISTS capabilities JSONB NOT NULL DEFAULT '[]';

-- Seed capabilities from existing service_type
-- This ensures existing models work without any application changes.
UPDATE models SET capabilities =
    CASE service_type
        WHEN 'CHAT'      THEN '["chat","completion"]'::jsonb
        WHEN 'EMBEDDING' THEN '["embedding"]'::jsonb
        WHEN 'RERANK'    THEN '["rerank"]'::jsonb
        WHEN 'STT'       THEN '["transcription"]'::jsonb
        WHEN 'TTS'       THEN '["speech"]'::jsonb
        WHEN 'OCR'       THEN '["ocr"]'::jsonb
        WHEN 'VISION'    THEN '["chat","vision"]'::jsonb
        WHEN 'IMAGE_GENERATION' THEN '["image_generation"]'::jsonb
        WHEN 'AGENT'     THEN '["chat","completion"]'::jsonb
        WHEN 'MCP'       THEN '["chat","completion"]'::jsonb
        ELSE             '[]'::jsonb
    END
WHERE capabilities = '[]'::jsonb OR capabilities IS NULL;

CREATE INDEX IF NOT EXISTS idx_models_capabilities ON models USING GIN (capabilities);

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. UNIVERSAL DEPLOYMENT FIELDS in model_runtime_configs
--    These fields allow any model type to be deployed identically:
--    health_path   — custom health check path (default: /health)
--    api_base_path — where the model's API lives (default: /v1)
--    volume_mounts — arbitrary volume mounts beyond the models volume
--    env_vars      — arbitrary environment variables
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE model_runtime_configs
    ADD COLUMN IF NOT EXISTS health_path    VARCHAR(255) NOT NULL DEFAULT '/health',
    ADD COLUMN IF NOT EXISTS api_base_path  VARCHAR(255) NOT NULL DEFAULT '/v1',
    ADD COLUMN IF NOT EXISTS volume_mounts  JSONB        NOT NULL DEFAULT '[]',
    ADD COLUMN IF NOT EXISTS env_vars       JSONB        NOT NULL DEFAULT '{}';

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. HEALTH PATH + API BASE on model_endpoints
--    Allows the runtime watcher and proxy to use the correct health endpoint
--    without knowing the model type.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE model_endpoints
    ADD COLUMN IF NOT EXISTS health_path   VARCHAR(255) NOT NULL DEFAULT '/health',
    ADD COLUMN IF NOT EXISTS api_base_path VARCHAR(255) NOT NULL DEFAULT '/v1';

-- Seed health_path from known backend types
UPDATE model_endpoints me
SET health_path = CASE
    WHEN m.backend_type = 'ollama'   THEN '/'
    WHEN m.backend_type IN ('vllm','tgi','llamacpp','openai_compat','cpu_native') THEN '/health'
    ELSE '/health'
END
FROM models m
WHERE m.id = me.model_id
  AND me.health_path = '/health';  -- only touch rows still at default

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. GENERIC NODE CAPABILITIES
--    Old boolean columns (has_vllm, has_ollama, has_tgi, has_whisper, etc.)
--    require a schema migration for every new backend.
--    Add a JSONB column for extensible backend support reporting.
--    Old columns are kept for backward compat but new code uses supported_backends.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE node_capabilities
    ADD COLUMN IF NOT EXISTS supported_backends JSONB NOT NULL DEFAULT '[]';

-- Seed supported_backends from existing boolean columns (only if they still exist).
-- These columns are dropped in migration 035; skip silently when they're gone.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_name='node_capabilities' AND column_name='has_vllm') THEN
        UPDATE node_capabilities
        SET supported_backends = (
            SELECT jsonb_agg(b) FROM (
                SELECT 'vllm'      AS b WHERE has_vllm      = TRUE UNION ALL
                SELECT 'ollama'    AS b WHERE has_ollama    = TRUE UNION ALL
                SELECT 'tgi'       AS b WHERE has_tgi       = TRUE UNION ALL
                SELECT 'whisper'   AS b WHERE has_whisper   = TRUE UNION ALL
                SELECT 'tts'       AS b WHERE has_tts       = TRUE UNION ALL
                SELECT 'embedding' AS b WHERE has_embedding = TRUE
            ) t
        )
        WHERE supported_backends = '[]'::jsonb;
    END IF;
END $$;

-- ─────────────────────────────────────────────────────────────────────────────
-- 7. NORMALIZE: ensure all CHAT models have workload_policy
--    STT/TTS/OCR/Embedding models default to 'lazy_load' too (they will get
--    idle-eviction support in this release).
-- ─────────────────────────────────────────────────────────────────────────────
UPDATE model_runtime_configs mrc
SET workload_policy = COALESCE(NULLIF(workload_policy, ''), 'lazy_load')
WHERE workload_policy IS NULL OR workload_policy = '';

-- ─────────────────────────────────────────────────────────────────────────────
-- 8. VIEW: universal_models
--    Single view that returns every model with its deployment and health status.
--    Replaces the need to query models + services separately.
-- ─────────────────────────────────────────────────────────────────────────────
-- CREATE OR REPLACE VIEW cannot change a view's column set, only append to
-- it — and later migrations (036, 044) redefine this same view with a
-- different column set. A re-run against a DB where a later migration's
-- version is already live fails with "cannot drop columns from view". DROP
-- first so this migration is order-independent of what currently exists.
DROP VIEW IF EXISTS universal_models CASCADE;
CREATE VIEW universal_models AS
SELECT
    m.id,
    m.name,
    m.display_name,
    m.service_type,
    m.backend_type,
    m.provider,
    m.enabled,
    m.capabilities,
    m.tags,
    m.created_at,
    m.updated_at,
    -- Endpoint summary
    COUNT(me.id)                                              AS endpoint_count,
    COUNT(me.id) FILTER (WHERE me.health_status = 'healthy') AS healthy_count,
    -- Runtime summary (from agent_runtimes for managed models)
    COUNT(ar.id) FILTER (
        WHERE ar.state IN ('ready','active','warm','idle')
    )                                                         AS running_replicas,
    COUNT(ar.id) FILTER (
        WHERE ar.state IN ('created','validating','downloading','starting',
                           'loading_model','waiting_ready','pending','recovering')
    )                                                         AS starting_replicas,
    -- HA
    COALESCE(rs.desired_replicas, 1)                          AS desired_replicas,
    COALESCE(rs.auto_recover, TRUE)                           AS auto_recover
FROM models m
LEFT JOIN model_endpoints me      ON me.model_id = m.id AND me.is_enabled = TRUE
LEFT JOIN agent_runtimes ar       ON ar.model_id = m.id
                                  AND ar.state NOT IN ('stopped','deleted','failed','archived')
LEFT JOIN model_replica_specs rs  ON rs.model_id = m.id
GROUP BY m.id, m.name, m.display_name, m.service_type, m.backend_type,
         m.provider, m.enabled, m.capabilities, m.tags,
         m.created_at, m.updated_at,
         rs.desired_replicas, rs.auto_recover;

COMMIT;
