-- NexusLLM Runtime Layer — Migration 003
-- Extends the model registry with versioning, multi-endpoint pools,
-- backend type, runtime config, and health state.
--
-- Run after: 001_initial.sql, 002_seed_data.sql

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- Drop & replace the old single-endpoint models table.
-- The old table had one vllm_endpoint column; we now normalise to:
--   models  →  model_versions  →  model_endpoints  (pool)
--   models  →  model_runtime_configs
-- ─────────────────────────────────────────────────────────────────────────────

-- 1. Rename old table so we can migrate its data.
ALTER TABLE models RENAME TO models_v1;

-- 2. New models table (logical identity of a model family).
CREATE TABLE models (
    id            UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    name          VARCHAR(255) NOT NULL UNIQUE,          -- "gemma-27b"
    display_name  VARCHAR(255) NOT NULL,
    provider      VARCHAR(100) NOT NULL DEFAULT 'local', -- local | openai | anthropic | huggingface
    backend_type  VARCHAR(50)  NOT NULL DEFAULT 'vllm',  -- vllm | ollama | tgi | openai_compat
    max_context   INTEGER      NOT NULL DEFAULT 8192,
    max_output    INTEGER      NOT NULL DEFAULT 4096,
    enabled       BOOLEAN      NOT NULL DEFAULT TRUE,
    tags          JSONB        NOT NULL DEFAULT '[]',    -- ["chat","instruct"]
    metadata      JSONB        NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_models_name        ON models(name);
CREATE INDEX idx_models_backend     ON models(backend_type);
CREATE INDEX idx_models_enabled     ON models(enabled);

CREATE TRIGGER set_models_updated_at
BEFORE UPDATE ON models
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- 3. Model versions (immutable snapshots of a model at a point in time).
CREATE TABLE model_versions (
    id            UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id      UUID         NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    version       VARCHAR(100) NOT NULL,                -- "1.0", "2024-04-01", "latest"
    description   TEXT,
    is_default    BOOLEAN      NOT NULL DEFAULT FALSE,  -- which version receives traffic by default
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE(model_id, version)
);
CREATE INDEX idx_model_versions_model_id ON model_versions(model_id);

-- Only one default version per model (partial unique index).
CREATE UNIQUE INDEX idx_model_versions_default
    ON model_versions(model_id)
    WHERE is_default = TRUE;

-- 4. Model endpoints — the actual backend instances for a model version.
--    A model may have many endpoints (pool). The runtime watcher updates
--    health_status and last_checked_at.
CREATE TABLE model_endpoints (
    id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id        UUID         NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    version_id      UUID         REFERENCES model_versions(id) ON DELETE SET NULL,
    host            VARCHAR(512) NOT NULL,              -- "localhost" or "gpu01.internal"
    port            INTEGER      NOT NULL,              -- 8000
    base_path       VARCHAR(255) NOT NULL DEFAULT '/v1',
    weight          INTEGER      NOT NULL DEFAULT 100,  -- for weighted routing (100 = normal)
    priority        INTEGER      NOT NULL DEFAULT 1,    -- 1=primary, 2=secondary (active/passive)
    health_status   VARCHAR(20)  NOT NULL DEFAULT 'unknown'
                    CHECK (health_status IN ('healthy','degraded','down','unknown','draining')),
    is_enabled      BOOLEAN      NOT NULL DEFAULT TRUE,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    last_checked_at TIMESTAMPTZ,
    last_success_at TIMESTAMPTZ,
    response_time_ms INTEGER,                           -- last observed latency
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE(host, port, model_id)
);
CREATE INDEX idx_endpoints_model_id     ON model_endpoints(model_id);
CREATE INDEX idx_endpoints_health       ON model_endpoints(health_status);
CREATE INDEX idx_endpoints_enabled      ON model_endpoints(is_enabled);

CREATE TRIGGER set_model_endpoints_updated_at
BEFORE UPDATE ON model_endpoints
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- 5. Runtime configs — per-model tuning knobs passed to the backend.
CREATE TABLE model_runtime_configs (
    id              UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id        UUID    NOT NULL REFERENCES models(id) ON DELETE CASCADE UNIQUE,
    gpu_memory_util NUMERIC(4,3) NOT NULL DEFAULT 0.90, -- vLLM: --gpu-memory-utilization
    tensor_parallel INTEGER      NOT NULL DEFAULT 1,    -- vLLM: --tensor-parallel-size
    max_batch_size  INTEGER      NOT NULL DEFAULT 256,
    dtype           VARCHAR(20)  NOT NULL DEFAULT 'auto',  -- auto | float16 | bfloat16
    quantization    VARCHAR(20),                         -- awq | gptq | squeezellm | NULL
    extra_args      JSONB        NOT NULL DEFAULT '{}', -- arbitrary backend-specific flags
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TRIGGER set_model_runtime_configs_updated_at
BEFORE UPDATE ON model_runtime_configs
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- 6. Endpoint health history — rolling window of health check results.
CREATE TABLE endpoint_health_log (
    id          BIGSERIAL   PRIMARY KEY,
    endpoint_id UUID        NOT NULL REFERENCES model_endpoints(id) ON DELETE CASCADE,
    status      VARCHAR(20) NOT NULL,
    latency_ms  INTEGER,
    error_msg   TEXT,
    checked_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_ep_health_log_endpoint_time
    ON endpoint_health_log(endpoint_id, checked_at DESC);

-- 7. Migrate existing models_v1 rows into the new schema.
INSERT INTO models (id, name, display_name, backend_type, max_output, enabled, created_at, updated_at)
SELECT id, name, display_name, 'vllm', max_tokens, active, created_at, updated_at
FROM models_v1;

-- Create a default version for each migrated model.
INSERT INTO model_versions (model_id, version, is_default)
SELECT id, 'v1', TRUE FROM models;

-- Migrate the single endpoint from models_v1.
INSERT INTO model_endpoints (model_id, host, port, base_path, health_status)
SELECT
    m.id,
    CASE
        WHEN position(':' IN regexp_replace(mv1.vllm_endpoint, 'https?://', '')) > 0
            THEN split_part(regexp_replace(mv1.vllm_endpoint, 'https?://', ''), ':', 1)
        ELSE regexp_replace(mv1.vllm_endpoint, 'https?://', '')
    END AS host,
    COALESCE(
        NULLIF(split_part(
            regexp_replace(
                regexp_replace(mv1.vllm_endpoint, 'https?://', ''),
                '/.*', ''
            ), ':', 2
        ), '')::INTEGER,
        8000
    ) AS port,
    '/v1',
    'unknown'
FROM models_v1 mv1
JOIN models m ON m.name = mv1.name;

-- 8. Fix foreign key in team_model_permissions to point to new models table.
--    (The UUIDs are preserved so no data change needed; constraint just points to
--     the table which now has the same rows.)

-- 9. Drop old table.
DROP TABLE models_v1;

COMMIT;
