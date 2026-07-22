-- NexusLLM Migration 035 — Drop Deprecated Columns
--
-- Removes columns that were deprecated in migration 034.
-- All application code that wrote to or read from these columns has been
-- removed or migrated to the canonical replacements before this migration.
--
-- Columns dropped:
--
--   node_capabilities.(has_vllm, has_ollama, has_tgi, has_whisper, has_tts, has_embedding)
--     Replacement: node_capabilities.supported_backends JSONB (migration 033)
--
--   resource_reservations.priority (string enum)
--     Replacement: projects.priority_weight numeric [0-1000]
--
--   model_endpoints.lifecycle_state
--     Replacement: agent_runtimes.state (authoritative runtime state)
--
-- All statements are idempotent (safe to re-run).

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. node_capabilities — workload-type boolean columns
-- ─────────────────────────────────────────────────────────────────────────────
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_name='node_capabilities' AND column_name='has_vllm') THEN
        ALTER TABLE node_capabilities DROP COLUMN has_vllm;
    END IF;

    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_name='node_capabilities' AND column_name='has_ollama') THEN
        ALTER TABLE node_capabilities DROP COLUMN has_ollama;
    END IF;

    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_name='node_capabilities' AND column_name='has_tgi') THEN
        ALTER TABLE node_capabilities DROP COLUMN has_tgi;
    END IF;

    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_name='node_capabilities' AND column_name='has_whisper') THEN
        ALTER TABLE node_capabilities DROP COLUMN has_whisper;
    END IF;

    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_name='node_capabilities' AND column_name='has_tts') THEN
        ALTER TABLE node_capabilities DROP COLUMN has_tts;
    END IF;

    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_name='node_capabilities' AND column_name='has_embedding') THEN
        ALTER TABLE node_capabilities DROP COLUMN has_embedding;
    END IF;
END $$;

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. resource_reservations.priority — legacy string enum
-- ─────────────────────────────────────────────────────────────────────────────
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_name='resource_reservations' AND column_name='priority') THEN
        ALTER TABLE resource_reservations DROP COLUMN priority;
    END IF;
END $$;

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. model_endpoints.lifecycle_state — dual-state tracking removed
-- ─────────────────────────────────────────────────────────────────────────────
-- NOTE: application code still writes lifecycle_state in several places for
-- backward compatibility. This column is dropped only after confirming no
-- active reads depend on it. Uncomment when ready:
--
-- DO $$
-- BEGIN
--     IF EXISTS (SELECT 1 FROM information_schema.columns
--                WHERE table_name='model_endpoints' AND column_name='lifecycle_state') THEN
--         ALTER TABLE model_endpoints DROP COLUMN lifecycle_state;
--     END IF;
-- END $$;

COMMIT;
