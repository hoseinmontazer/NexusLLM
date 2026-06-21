-- Migration 006: Ensure controller columns exist (idempotent — already in 003)
-- This is a no-op if migration 003 ran correctly.
BEGIN;
ALTER TABLE model_endpoints ADD COLUMN IF NOT EXISTS container_id  VARCHAR(255) NOT NULL DEFAULT '';
ALTER TABLE model_endpoints ADD COLUMN IF NOT EXISTS runtime_image VARCHAR(512) NOT NULL DEFAULT 'vllm/vllm-openai:latest';
COMMIT;
