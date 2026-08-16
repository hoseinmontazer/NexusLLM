-- NexusLLM Migration 036 — Remove Service Abstraction Remnants
--
-- Completes the removal of the AI Service abstraction introduced in migration 005
-- and deprecated in migrations 033–035.
--
-- Changes:
--
--   1. models.runtime_type — dropped
--      Reason: replaced by model_runtime_configs.execution_mode (cpu|gpu|auto).
--              Scheduling uses execution_mode exclusively. runtime_type was a
--              legacy string enum (GPU_RUNTIME|CPU_RUNTIME) with no active readers.
--      Safe to drop: no application code sets or reads this column post-033.
--
--   2. model_endpoints.runtime_type — dropped
--      Reason: same as above; the column was added in migration 005 and superseded
--              by agent_runtimes.effective_mode.
--
--   3. resource_reservations table — dropped
--      Reason: the legacy placement engine (internal/placement) that read this table
--              was removed in this release. The scheduler (internal/scheduler) uses
--              model_runtime_configs.required_vram_mb for resource requirements.
--              The GET/PUT /models/:id/reservation API still exists for clients that
--              may write here, but no scheduling code reads this table anymore.
--              Dropping it removes the orphaned data store.
--              NOTE: The reservation API endpoints remain on /models/:id/reservation
--                    but will return 404 once this migration runs (table gone).
--                    Those endpoints will be removed in migration 037.
--
--   4. cpu_allocations table — dropped
--      Reason: only written by placement.Engine.applyCPU() which was removed.
--              No reads exist anywhere in the codebase.
--
-- All statements are idempotent (safe to re-run).

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. models.runtime_type
-- ─────────────────────────────────────────────────────────────────────────────
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_name='models' AND column_name='runtime_type') THEN
        ALTER TABLE models DROP COLUMN runtime_type;
    END IF;
END $$;

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. model_endpoints.runtime_type
-- ─────────────────────────────────────────────────────────────────────────────
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_name='model_endpoints' AND column_name='runtime_type') THEN
        ALTER TABLE model_endpoints DROP COLUMN runtime_type;
    END IF;
END $$;

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. resource_reservations table
-- ─────────────────────────────────────────────────────────────────────────────
DROP TABLE IF EXISTS resource_reservations;

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. cpu_allocations table
-- ─────────────────────────────────────────────────────────────────────────────
DROP TABLE IF EXISTS cpu_allocations;

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. Refresh universal_models view — remove runtime_type reference
--    This view was originally created in migration 033 with runtime_type.
--    Now that the column is dropped, recreate it without that field.
-- ─────────────────────────────────────────────────────────────────────────────
-- DROP first (not CREATE OR REPLACE) — this view's column set differs from
-- migration 033's and from later migrations' (044), so a re-run against a
-- DB where either version is already live would otherwise fail with
-- "cannot drop columns from view".
DROP VIEW IF EXISTS universal_models CASCADE;
CREATE VIEW universal_models AS
SELECT
    m.id,
    m.name,
    m.display_name,
    m.service_type,
    m.backend_type,
    m.provider,
    m.enabled,
    m.capabilities,
    m.tags,
    m.created_at,
    m.updated_at,
    COUNT(me.id)                                              AS endpoint_count,
    COUNT(me.id) FILTER (WHERE me.health_status = 'healthy') AS healthy_count,
    COUNT(ar.id) FILTER (
        WHERE ar.state IN ('ready','active','warm','idle')
    )                                                         AS running_replicas,
    COUNT(ar.id) FILTER (
        WHERE ar.state IN ('created','validating','downloading','starting',
                           'loading_model','waiting_ready','pending','recovering')
    )                                                         AS starting_replicas,
    COALESCE(rs.desired_replicas, 1)                          AS desired_replicas,
    COALESCE(rs.auto_recover, TRUE)                           AS auto_recover
FROM models m
LEFT JOIN model_endpoints me      ON me.model_id = m.id AND me.is_enabled = TRUE
LEFT JOIN agent_runtimes ar       ON ar.model_id = m.id
                                  AND ar.state NOT IN ('stopped','deleted','failed','archived')
LEFT JOIN model_replica_specs rs  ON rs.model_id = m.id
GROUP BY m.id, m.name, m.display_name, m.service_type, m.backend_type,
         m.provider, m.enabled, m.capabilities, m.tags,
         m.created_at, m.updated_at,
         rs.desired_replicas, rs.auto_recover;

COMMIT;
