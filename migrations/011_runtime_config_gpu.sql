-- NexusLLM Migration 011 — Store GPU devices in model_runtime_configs
--
-- Adds gpu_devices to model_runtime_configs so the lazy-load activator
-- can use the admin-configured GPU assignment on every cold start,
-- without relying on a pre-existing agent_runtimes row.
BEGIN;

ALTER TABLE model_runtime_configs
    ADD COLUMN IF NOT EXISTS gpu_devices  JSONB NOT NULL DEFAULT '[]'::jsonb;

-- Also store the node_id override at the runtime-config level so the
-- activator always knows which node to deploy on, even when no prior
-- agent_runtimes row exists.
ALTER TABLE model_runtime_configs
    ADD COLUMN IF NOT EXISTS node_id  UUID REFERENCES nodes(id) ON DELETE SET NULL;

COMMIT;
