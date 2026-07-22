-- NexusLLM Migration 034 — Deprecate Legacy Schema Pieces
--
-- This migration marks legacy columns as deprecated by:
--   1. Adding a COMMENT ON COLUMN so operators can see the deprecation in
--      pg_catalog / schema introspection tools.
--   2. Setting default values to NULL / empty where safe.
--   3. NOT dropping any columns — existing data and old API clients remain
--      functional. Columns will be dropped in a future release (035+) once
--      all readers have been migrated.
--
-- Deprecated items:
--
--   models.vllm_endpoint
--     Reason: predates multi-endpoint architecture; the endpoint is now stored
--             in model_endpoints.host + model_endpoints.port.
--     Action: stop writing; clear default; retain for read compat.
--
--   node_capabilities boolean workload columns
--     (has_vllm, has_ollama, has_tgi, has_whisper, has_tts, has_embedding)
--     Reason: workload-type awareness belongs to models, not nodes. Replaced by
--             node_capabilities.supported_backends JSONB (migration 033).
--     Action: stop writing; retain for read compat.
--
--   resource_reservations.priority (string enum)
--     Reason: predates numeric priority_weight. Scheduling uses
--             projects.priority_weight exclusively. The string enum is dead data.
--     Action: set default to 'normal'; retain for read compat.
--
-- All statements are idempotent (safe to re-run).

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. models.vllm_endpoint — deprecated
-- ─────────────────────────────────────────────────────────────────────────────

-- Remove the NOT NULL constraint if present so new rows don't require it.
ALTER TABLE models
    ALTER COLUMN vllm_endpoint DROP NOT NULL;

-- Clear the default so new INSERT statements don't need to provide it.
ALTER TABLE models
    ALTER COLUMN vllm_endpoint SET DEFAULT NULL;

COMMENT ON COLUMN models.vllm_endpoint IS
    'DEPRECATED since migration 034. Use model_endpoints.host + model_endpoints.port. '
    'Will be removed in migration 036+.';

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. node_capabilities — workload-type boolean columns deprecated
-- ─────────────────────────────────────────────────────────────────────────────
-- Only comment them if the columns exist (older clusters may have been
-- created before migration 005 added them).

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'node_capabilities' AND column_name = 'has_whisper'
    ) THEN
        COMMENT ON COLUMN node_capabilities.has_whisper IS
            'DEPRECATED since migration 034. Workload types belong to models, not nodes. '
            'Use node_capabilities.supported_backends JSONB instead. Will be removed in migration 036+.';
    END IF;

    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'node_capabilities' AND column_name = 'has_tts'
    ) THEN
        COMMENT ON COLUMN node_capabilities.has_tts IS
            'DEPRECATED since migration 034. See has_whisper comment.';
    END IF;

    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'node_capabilities' AND column_name = 'has_embedding'
    ) THEN
        COMMENT ON COLUMN node_capabilities.has_embedding IS
            'DEPRECATED since migration 034. See has_whisper comment.';
    END IF;

    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'node_capabilities' AND column_name = 'has_vllm'
    ) THEN
        COMMENT ON COLUMN node_capabilities.has_vllm IS
            'DEPRECATED since migration 034. See has_whisper comment.';
    END IF;

    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'node_capabilities' AND column_name = 'has_ollama'
    ) THEN
        COMMENT ON COLUMN node_capabilities.has_ollama IS
            'DEPRECATED since migration 034. See has_whisper comment.';
    END IF;

    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'node_capabilities' AND column_name = 'has_tgi'
    ) THEN
        COMMENT ON COLUMN node_capabilities.has_tgi IS
            'DEPRECATED since migration 034. See has_whisper comment.';
    END IF;
END $$;

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. resource_reservations.priority — legacy string enum
-- ─────────────────────────────────────────────────────────────────────────────

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'resource_reservations' AND column_name = 'priority'
    ) THEN
        COMMENT ON COLUMN resource_reservations.priority IS
            'DEPRECATED since migration 034. Scheduling uses projects.priority_weight (numeric 0-1000). '
            'This string enum column is no longer read by any scheduling code. '
            'Will be removed in migration 036+.';

        -- Set a stable default so new rows inserted without this field don''t fail.
        ALTER TABLE resource_reservations
            ALTER COLUMN priority SET DEFAULT 'normal';
    END IF;
END $$;

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. model_endpoints.lifecycle_state — dual-state tracking deprecation
-- ─────────────────────────────────────────────────────────────────────────────
-- The authoritative runtime state is agent_runtimes.state.
-- model_endpoints.lifecycle_state is a legacy column that was written
-- by ModelController and RuntimeHandler. New code uses agent_runtimes.state.
-- The column is preserved for legacy API clients but will be removed in 036+.

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'model_endpoints' AND column_name = 'lifecycle_state'
    ) THEN
        COMMENT ON COLUMN model_endpoints.lifecycle_state IS
            'DEPRECATED since migration 034. The authoritative runtime state is '
            'agent_runtimes.state. This column is kept for backward compat with '
            'API clients that still read it. Will be removed in migration 036+.';
    END IF;
END $$;

COMMIT;
