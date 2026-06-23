-- NexusLLM Migration 014 — CPU/GPU execution mode
--
-- Adds execution_mode to model_runtime_configs and agent_runtimes so that:
--   - CPU-only deployments never request GPU resources from Docker
--   - GPU deployments validate availability before starting
--   - AUTO deployments detect node capability at dispatch time
--
-- execution_mode values:
--   cpu   — never use GPUs; do not pass --gpus to Docker; n_gpu_layers forced to 0
--   gpu   — always use GPUs; fail if no GPU available
--   auto  — detect at dispatch: use GPU if available, else fall back to CPU
--
-- effective_mode is written by the agent after resolving 'auto' at runtime.
BEGIN;

-- ── model_runtime_configs: desired execution mode (set by operator) ───────────
ALTER TABLE model_runtime_configs
    ADD COLUMN IF NOT EXISTS execution_mode VARCHAR(10) NOT NULL DEFAULT 'auto'
        CHECK (execution_mode IN ('cpu', 'gpu', 'auto'));

-- ── agent_runtimes: what was actually resolved for this runtime instance ──────
ALTER TABLE agent_runtimes
    ADD COLUMN IF NOT EXISTS requested_mode  VARCHAR(10) NOT NULL DEFAULT 'auto'
        CHECK (requested_mode IN ('cpu', 'gpu', 'auto'));
ALTER TABLE agent_runtimes
    ADD COLUMN IF NOT EXISTS effective_mode  VARCHAR(10) NOT NULL DEFAULT 'cpu'
        CHECK (effective_mode IN ('cpu', 'gpu'));

-- ── node_capabilities: expose GPU availability for auto-resolution ────────────
-- gpu_available is derived from gpu_count > 0 but stored explicitly so
-- the scheduler can filter without joining gpu_devices.
ALTER TABLE node_capabilities
    ADD COLUMN IF NOT EXISTS gpu_available BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE node_capabilities
    ADD COLUMN IF NOT EXISTS gpu_vram_mb   BIGINT  NOT NULL DEFAULT 0;

-- Keep gpu_available in sync with gpu_count (backfill existing rows)
UPDATE node_capabilities SET gpu_available = (gpu_count > 0) WHERE gpu_available = FALSE;

COMMIT;
