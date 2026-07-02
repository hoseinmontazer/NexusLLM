-- NexusLLM Migration 030 — State constraint & view catch-up
--
-- Problem: migrations 009 and 010 each DROP and re-ADD the
-- agent_runtimes_state_check constraint with a partial list of states.
-- When the database already contains rows whose state was added by a later
-- migration (e.g. 'created', 'validating', 'ready', 'downloading',
-- 'loading_model', 'waiting_ready', 'draining', 'unhealthy', 'recovering'),
-- PostgreSQL rejects the ADD CONSTRAINT because existing rows violate it.
--
-- This migration replaces the constraint with the full cumulative union of
-- every state value accepted by any migration (001–029), and rebuilds the
-- project_runtime_summary view cleanly.
--
-- All statements are idempotent.
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. Replace agent_runtimes state constraint with the full cumulative list
--    (union of every value ever accepted across migrations 009–029).
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE agent_runtimes DROP CONSTRAINT IF EXISTS agent_runtimes_state_check;
ALTER TABLE agent_runtimes ADD CONSTRAINT agent_runtimes_state_check
    CHECK (state IN (
        -- original / legacy states (007, 009, 010)
        'pending', 'pulling', 'starting', 'loading', 'warm', 'active', 'idle',
        'stopping', 'stopped', 'unloaded', 'failed', 'lost', 'archived', 'deleted',
        -- unified startup pipeline (012)
        'created', 'validating', 'downloading', 'loading_model', 'waiting_ready', 'ready',
        -- HA recovery (019)
        'recovering',
        -- rolling replacement (029)
        'unhealthy', 'draining'
    ));

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. Rebuild project_runtime_summary view
--
--    Migration 011 uses CREATE OR REPLACE VIEW. If the view was previously
--    created with a different column list (e.g. by a partial earlier run),
--    PostgreSQL rejects the replacement with "cannot drop columns from view".
--    DROP + CREATE is the only safe path when the column list changes.
-- ─────────────────────────────────────────────────────────────────────────────
DROP VIEW IF EXISTS project_runtime_summary CASCADE;

CREATE VIEW project_runtime_summary AS
SELECT
    p.id                                                 AS project_id,
    p.name                                               AS project_name,
    p.priority,
    project_priority_score(p.priority)                   AS priority_score,
    COUNT(ar.id) FILTER (
        WHERE ar.state IN ('active','warm','ready')
    )                                                    AS active_runtime_count,
    COALESCE(pr.reserved_vram_mb, 0)                     AS reserved_vram_mb,
    COALESCE(pr.reserved_cpu_cores, 0)                   AS reserved_cpu_cores,
    COALESCE(pr.reserved_memory_mb, 0)                   AS reserved_memory_mb,
    COALESCE(pc.always_running, FALSE)                   AS always_running,
    COALESCE(pc.protected, FALSE)                        AS protected,
    COALESCE(pc.minimum_replicas, 0)                     AS minimum_replicas,
    COALESCE(pc.admission_policy, 'queue')               AS admission_policy
FROM projects p
LEFT JOIN agent_runtimes ar         ON ar.project_id = p.id
LEFT JOIN project_reservations pr   ON pr.project_id = p.id
LEFT JOIN project_configurations pc ON pc.project_id = p.id
GROUP BY p.id, p.name, p.priority,
         pr.reserved_vram_mb, pr.reserved_cpu_cores, pr.reserved_memory_mb,
         pc.always_running, pc.protected, pc.minimum_replicas, pc.admission_policy;

COMMIT;
