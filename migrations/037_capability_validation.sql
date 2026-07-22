-- NexusLLM Migration 037 — Model Capability Validation
--
-- Goal: Ensure every model has a populated capabilities column so the gateway
--       can validate API endpoint compatibility before routing requests to any
--       runtime backend.
--
-- Background:
--   Migration 033 added the capabilities JSONB column and seeded it from
--   service_type. This migration ensures there are no gaps (models created
--   before 033 ran, or via raw SQL inserts that skipped the column).
--
-- Changes:
--   1. Ensure capabilities column exists (idempotent — 033 already adds it).
--   2. Backfill any rows where capabilities is still empty/null using the same
--      service_type → capability mapping as migration 033.
--   3. Add a partial index on capabilities for models with an empty list so
--      the backfill query can be efficient on re-run.
--   4. Seed the 'responses' capability for models that currently have 'chat'
--      (the /v1/responses endpoint is chat-compatible for all LLM models).
--   5. Add the moderation capability seed for MODERATION service_type models.
--
-- All statements are idempotent (safe to re-run).

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. Ensure column exists (no-op if migration 033 already ran)
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE models ADD COLUMN IF NOT EXISTS capabilities JSONB NOT NULL DEFAULT '[]';

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. Backfill any models missing capabilities
--    Uses the canonical service_type → capability mapping.
--    Models that already have a non-empty capabilities value are untouched.
-- ─────────────────────────────────────────────────────────────────────────────
UPDATE models SET capabilities =
    CASE service_type
        WHEN 'CHAT'             THEN '["chat","completion"]'::jsonb
        WHEN 'EMBEDDING'        THEN '["embedding"]'::jsonb
        WHEN 'RERANK'           THEN '["rerank"]'::jsonb
        WHEN 'STT'              THEN '["transcription"]'::jsonb
        WHEN 'TTS'              THEN '["speech"]'::jsonb
        WHEN 'OCR'              THEN '["ocr"]'::jsonb
        WHEN 'VISION'           THEN '["chat","vision"]'::jsonb
        WHEN 'IMAGE_GENERATION' THEN '["image_generation"]'::jsonb
        WHEN 'MODERATION'       THEN '["moderation"]'::jsonb
        WHEN 'AGENT'            THEN '["chat","completion"]'::jsonb
        WHEN 'MCP'              THEN '["chat","completion"]'::jsonb
        ELSE                         '[]'::jsonb
    END
WHERE capabilities = '[]'::jsonb
   OR capabilities IS NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. Ensure GIN index for fast gateway capability lookups
-- ─────────────────────────────────────────────────────────────────────────────
CREATE INDEX IF NOT EXISTS idx_models_capabilities ON models USING GIN (capabilities);

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. Add 'responses' capability to all CHAT-class models
--    The /v1/responses endpoint is OpenAI-compatible chat — any model that
--    supports 'chat' should also support 'responses'.
-- ─────────────────────────────────────────────────────────────────────────────
UPDATE models
SET capabilities = capabilities || '["responses"]'::jsonb
WHERE capabilities @> '["chat"]'::jsonb
  AND NOT capabilities @> '["responses"]'::jsonb;

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. Confirm: log a summary of capability distribution
--    (This is a comment — the SELECT below is for manual inspection only)
--
-- SELECT
--     service_type,
--     capabilities,
--     COUNT(*) AS model_count
-- FROM models
-- GROUP BY service_type, capabilities
-- ORDER BY service_type, model_count DESC;
-- ─────────────────────────────────────────────────────────────────────────────

COMMIT;
