-- Migration 008 — Node Model Cache
-- Tracks which model weights are locally cached on each node.
-- The control plane uses this to prefer nodes that already have a model
-- (avoiding re-download) and to show cache status in the UI.
BEGIN;

CREATE TABLE IF NOT EXISTS node_model_cache (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id        UUID        NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    -- Model identity
    model_ref      VARCHAR(512) NOT NULL,  -- HF repo ID or Ollama model name
    backend        VARCHAR(50)  NOT NULL,  -- vllm | ollama | tgi
    -- Cache state
    is_cached      BOOLEAN     NOT NULL DEFAULT FALSE,
    size_bytes     BIGINT      NOT NULL DEFAULT 0,
    -- Timing
    cached_at      TIMESTAMPTZ,
    last_verified  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Constraint: one entry per node+model+backend
    UNIQUE(node_id, model_ref, backend)
);
CREATE INDEX IF NOT EXISTS idx_node_model_cache_node    ON node_model_cache(node_id);
CREATE INDEX IF NOT EXISTS idx_node_model_cache_model   ON node_model_cache(model_ref);
CREATE INDEX IF NOT EXISTS idx_node_model_cache_cached  ON node_model_cache(is_cached);

COMMIT;
