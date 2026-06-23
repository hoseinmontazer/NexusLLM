-- NexusLLM Migration 016 — Workload policy
--
-- Unifies LLM models and AI services under a single execution model.
-- WorkloadPolicy is the ONLY behavioral difference between workload types:
--
--   lazy_load  — start on first request, stop when idle (LLMs)
--   always_on  — start on deploy, never idle-evict, restart on crash (services)
--
-- All workloads use the same execution path: StartModel() → Node Agent → Docker.
BEGIN;

ALTER TABLE model_runtime_configs
    ADD COLUMN IF NOT EXISTS workload_policy VARCHAR(20) NOT NULL DEFAULT 'lazy_load';

ALTER TABLE model_runtime_configs
    DROP CONSTRAINT IF EXISTS model_runtime_configs_workload_policy_check;
ALTER TABLE model_runtime_configs
    ADD CONSTRAINT model_runtime_configs_workload_policy_check
        CHECK (workload_policy IN ('lazy_load', 'always_on'));

ALTER TABLE agent_runtimes
    ADD COLUMN IF NOT EXISTS workload_policy VARCHAR(20) NOT NULL DEFAULT 'lazy_load';

ALTER TABLE agent_runtimes
    DROP CONSTRAINT IF EXISTS agent_runtimes_workload_policy_check;
ALTER TABLE agent_runtimes
    ADD CONSTRAINT agent_runtimes_workload_policy_check
        CHECK (workload_policy IN ('lazy_load', 'always_on'));

-- Backfill: existing AI services (non-CHAT) become always_on.
UPDATE model_runtime_configs mrc
SET workload_policy = 'always_on'
FROM models m
WHERE mrc.model_id = m.id
  AND m.service_type IS NOT NULL
  AND m.service_type <> ''
  AND m.service_type <> 'CHAT';

COMMIT;
