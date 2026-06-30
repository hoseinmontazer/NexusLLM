-- NexusLLM Migration 027 — Thinking/Reasoning mode support
--
-- Adds:
--   1. models.supports_thinking   — capability flag (set by operator at deploy time)
--   2. models.thinking_enabled    — deployment default (true = send thinking params)
--   3. models.min_thinking_tokens — auto-disable thinking below this max_tokens threshold

BEGIN;

ALTER TABLE models
    ADD COLUMN IF NOT EXISTS supports_thinking  BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS thinking_enabled   BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS min_thinking_tokens INTEGER NOT NULL DEFAULT 500;

-- Mark known reasoning models based on name pattern (best-effort backfill).
-- Operators can always update manually via PUT /admin/v1/models/:id.
UPDATE models
SET supports_thinking = TRUE,
    thinking_enabled  = TRUE
WHERE name ILIKE '%qwen3%'
   OR name ILIKE '%deepseek%r1%'
   OR name ILIKE '%deepseek%r2%'
   OR name ILIKE '%o1%'
   OR name ILIKE '%o3%'
   OR name ILIKE '%gemini%thinking%';

COMMIT;
