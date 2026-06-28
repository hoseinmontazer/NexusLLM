-- NexusLLM Migration 024 — Kubernetes-like Placement v2
-- Adds:
--   * cordon status for nodes (online but accepts no new workloads)
--   * placement_strategy on deployment_queue and model_runtime_configs
--   * accelerator_type requirement (cpu | gpu | any)
--   * anti_affinity hard constraint enforced by HA reconciler
--   * node labels / taints (jsonb)
-- All idempotent.
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. Node: add cordon flag + taint support
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE nodes
    ADD COLUMN IF NOT EXISTS cordoned        BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS cordon_reason   TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS cordoned_at     TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS taints          JSONB       NOT NULL DEFAULT '[]';

-- A cordoned node keeps its status=online but receives no new placements.
-- The scheduler checks cordoned=FALSE before considering a node as a candidate.

CREATE INDEX IF NOT EXISTS idx_nodes_cordoned ON nodes(cordoned) WHERE cordoned = FALSE;

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. model_runtime_configs: placement strategy + accelerator
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE model_runtime_configs
    ADD COLUMN IF NOT EXISTS placement_strategy VARCHAR(20) NOT NULL DEFAULT 'auto',
    ADD COLUMN IF NOT EXISTS accelerator_type   VARCHAR(10) NOT NULL DEFAULT 'any',
    ADD COLUMN IF NOT EXISTS pinned_node_id     UUID REFERENCES nodes(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS replica_distribution VARCHAR(20) NOT NULL DEFAULT 'spread';

-- Valid placement_strategy values: auto | pinned | spread | packed
-- Valid accelerator_type values:   cpu | gpu | any
-- Valid replica_distribution:      spread | pack | anti_affinity

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. deployment_queue: placement strategy fields
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE deployment_queue
    ADD COLUMN IF NOT EXISTS placement_strategy VARCHAR(20) NOT NULL DEFAULT 'auto',
    ADD COLUMN IF NOT EXISTS accelerator_type   VARCHAR(10) NOT NULL DEFAULT 'any',
    ADD COLUMN IF NOT EXISTS pinned_node_id     UUID REFERENCES nodes(id) ON DELETE SET NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. model_replica_specs: default to anti_affinity for HA replicas >= 2
-- ─────────────────────────────────────────────────────────────────────────────
-- Existing spread records are fine. Just add accelerator_type column.
ALTER TABLE model_replica_specs
    ADD COLUMN IF NOT EXISTS accelerator_type VARCHAR(10) NOT NULL DEFAULT 'any';

-- Update existing specs with desired_replicas >= 2 to use spread by default
UPDATE model_replica_specs
SET placement_policy = 'spread'
WHERE desired_replicas >= 2
  AND placement_policy = 'spread'; -- already correct, no-op

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. scheduler_decisions: track placement strategy used
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE scheduler_decisions
    ADD COLUMN IF NOT EXISTS placement_strategy VARCHAR(20) NOT NULL DEFAULT 'auto',
    ADD COLUMN IF NOT EXISTS accelerator_type   VARCHAR(10) NOT NULL DEFAULT 'any';

COMMIT;
