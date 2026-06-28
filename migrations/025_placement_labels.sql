-- NexusLLM Migration 025 — Node Labels & Placement Modes
-- Adds:
--   * placement_mode enum: auto | specific_node | node_group | label_selector
--   * node_selector jsonb on model_runtime_configs (e.g. {"accelerator":"h200"})
--   * node_group_id text field for grouped deployments
--   * structured labels jsonb on nodes (already exists as jsonb, just document convention)
--   * placement_mode + node_selector on deployment_queue for queued retries
-- All idempotent.
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. model_runtime_configs — placement mode + selector
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE model_runtime_configs
    ADD COLUMN IF NOT EXISTS placement_mode   VARCHAR(20)  NOT NULL DEFAULT 'auto',
    ADD COLUMN IF NOT EXISTS node_selector    JSONB        NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS node_group_id    TEXT         NOT NULL DEFAULT '';

-- placement_mode values: auto | specific_node | node_group | label_selector

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. deployment_queue — carry selector for retry attempts
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE deployment_queue
    ADD COLUMN IF NOT EXISTS placement_mode   VARCHAR(20)  NOT NULL DEFAULT 'auto',
    ADD COLUMN IF NOT EXISTS node_selector    JSONB        NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS node_group_id    TEXT         NOT NULL DEFAULT '';

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. nodes — ensure labels column is jsonb (already exists from migration 001)
--    Add a GIN index for fast label-selector queries
-- ─────────────────────────────────────────────────────────────────────────────
CREATE INDEX IF NOT EXISTS idx_nodes_labels_gin ON nodes USING GIN (labels jsonb_path_ops)
    WHERE status IN ('online', 'degraded') AND cordoned = FALSE;

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. scheduler_decisions — record which placement mode was used
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE scheduler_decisions
    ADD COLUMN IF NOT EXISTS placement_mode   VARCHAR(20)  NOT NULL DEFAULT 'auto',
    ADD COLUMN IF NOT EXISTS node_selector    JSONB        NOT NULL DEFAULT '{}';

COMMIT;
