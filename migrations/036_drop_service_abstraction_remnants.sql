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

COMMIT;
