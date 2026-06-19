-- Migration 006: Add controller columns to model_endpoints
-- These are needed by the Model Controller to track running containers.
BEGIN;

ALTER TABLE model_endpoints
    ADD COLUMN IF NOT EXISTS container_id   VARCHAR(255) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS runtime_image  VARCHAR(512) NOT NULL DEFAULT 'vllm/vllm-openai:latest';

COMMIT;
