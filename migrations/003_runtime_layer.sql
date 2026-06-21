-- NexusLLM Runtime Layer — Migration 003 (idempotent)
BEGIN;

-- Extend models table with new columns (ignore if already exist)
ALTER TABLE models ADD COLUMN IF NOT EXISTS provider     VARCHAR(100) NOT NULL DEFAULT 'local';
ALTER TABLE models ADD COLUMN IF NOT EXISTS backend_type VARCHAR(50)  NOT NULL DEFAULT 'vllm';
ALTER TABLE models ADD COLUMN IF NOT EXISTS max_context  INTEGER      NOT NULL DEFAULT 8192;
ALTER TABLE models ADD COLUMN IF NOT EXISTS max_output   INTEGER      NOT NULL DEFAULT 4096;
ALTER TABLE models ADD COLUMN IF NOT EXISTS enabled      BOOLEAN      NOT NULL DEFAULT TRUE;
ALTER TABLE models ADD COLUMN IF NOT EXISTS tags         JSONB        NOT NULL DEFAULT '[]';
ALTER TABLE models ADD COLUMN IF NOT EXISTS metadata     JSONB        NOT NULL DEFAULT '{}';

-- Rename active→enabled if old column exists
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.columns
             WHERE table_name='models' AND column_name='active') THEN
    UPDATE models SET enabled = active WHERE TRUE;
  END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_models_backend ON models(backend_type);
CREATE INDEX IF NOT EXISTS idx_models_enabled ON models(enabled);

-- Model versions
CREATE TABLE IF NOT EXISTS model_versions (
    id         UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id   UUID         NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    version    VARCHAR(100) NOT NULL,
    description TEXT,
    is_default BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE(model_id, version)
);
CREATE INDEX IF NOT EXISTS idx_model_versions_model_id ON model_versions(model_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_model_versions_default
    ON model_versions(model_id) WHERE is_default = TRUE;

-- Model endpoints (pool)
CREATE TABLE IF NOT EXISTS model_endpoints (
    id                   UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id             UUID         NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    version_id           UUID         REFERENCES model_versions(id) ON DELETE SET NULL,
    host                 VARCHAR(512) NOT NULL,
    port                 INTEGER      NOT NULL,
    base_path            VARCHAR(255) NOT NULL DEFAULT '/v1',
    weight               INTEGER      NOT NULL DEFAULT 100,
    priority             INTEGER      NOT NULL DEFAULT 1,
    health_status        VARCHAR(20)  NOT NULL DEFAULT 'unknown'
                         CHECK (health_status IN ('healthy','degraded','down','unknown','draining')),
    is_enabled           BOOLEAN      NOT NULL DEFAULT TRUE,
    consecutive_failures INTEGER      NOT NULL DEFAULT 0,
    last_checked_at      TIMESTAMPTZ,
    last_success_at      TIMESTAMPTZ,
    response_time_ms     INTEGER,
    lifecycle_state      VARCHAR(30)  NOT NULL DEFAULT 'registered'
                         CHECK (lifecycle_state IN (
                             'registered','downloading','loading','warm','active',
                             'idle','unloading','unloaded','failed','draining'
                         )),
    container_id         VARCHAR(255) NOT NULL DEFAULT '',
    runtime_image        VARCHAR(512) NOT NULL DEFAULT 'vllm/vllm-openai:latest',
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE(host, port, model_id)
);
CREATE INDEX IF NOT EXISTS idx_endpoints_model_id ON model_endpoints(model_id);
CREATE INDEX IF NOT EXISTS idx_endpoints_health   ON model_endpoints(health_status);
CREATE INDEX IF NOT EXISTS idx_endpoints_enabled  ON model_endpoints(is_enabled);

CREATE OR REPLACE FUNCTION trigger_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN NEW.updated_at = NOW(); RETURN NEW; END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS set_model_endpoints_updated_at ON model_endpoints;
CREATE TRIGGER set_model_endpoints_updated_at
BEFORE UPDATE ON model_endpoints
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- Runtime configs
CREATE TABLE IF NOT EXISTS model_runtime_configs (
    id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id        UUID         NOT NULL REFERENCES models(id) ON DELETE CASCADE UNIQUE,
    gpu_memory_util NUMERIC(4,3) NOT NULL DEFAULT 0.90,
    tensor_parallel INTEGER      NOT NULL DEFAULT 1,
    max_batch_size  INTEGER      NOT NULL DEFAULT 256,
    dtype           VARCHAR(20)  NOT NULL DEFAULT 'auto',
    quantization    VARCHAR(20),
    extra_args      JSONB        NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Endpoint health log
CREATE TABLE IF NOT EXISTS endpoint_health_log (
    id          BIGSERIAL   PRIMARY KEY,
    endpoint_id UUID        NOT NULL REFERENCES model_endpoints(id) ON DELETE CASCADE,
    status      VARCHAR(20) NOT NULL,
    latency_ms  INTEGER,
    error_msg   TEXT,
    checked_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_ep_health_log_endpoint_time
    ON endpoint_health_log(endpoint_id, checked_at DESC);

-- Model lifecycle events
CREATE TABLE IF NOT EXISTS model_lifecycle_events (
    id          BIGSERIAL   PRIMARY KEY,
    endpoint_id UUID        NOT NULL REFERENCES model_endpoints(id) ON DELETE CASCADE,
    from_state  VARCHAR(30),
    to_state    VARCHAR(30) NOT NULL,
    reason      TEXT,
    actor       VARCHAR(100),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_lifecycle_ep_time
    ON model_lifecycle_events(endpoint_id, created_at DESC);

-- Controller operations log
CREATE TABLE IF NOT EXISTS controller_operations (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id     UUID        NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    endpoint_id  UUID        REFERENCES model_endpoints(id) ON DELETE SET NULL,
    operation    VARCHAR(50) NOT NULL,
    status       VARCHAR(20) NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','running','success','failed','cancelled')),
    initiated_by VARCHAR(100),
    started_at   TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    error_msg    TEXT,
    metadata     JSONB NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_ctrl_ops_model  ON controller_operations(model_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_ctrl_ops_status ON controller_operations(status);

-- Seed a default version for each model that doesn't have one
INSERT INTO model_versions (model_id, version, is_default)
SELECT id, 'v1', TRUE FROM models
WHERE id NOT IN (SELECT model_id FROM model_versions)
ON CONFLICT DO NOTHING;

COMMIT;
