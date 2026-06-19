-- NexusLLM Enterprise Platform — Migration 005
-- Adds: GPU inventory, model lifecycle, usage tracking, prompt policies,
--       model aliases, AI gateway policy layer, model controller state.
-- Run after: 001, 002, 003, 004

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. GPU INVENTORY
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE gpu_nodes (
    id           UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    name         VARCHAR(255) NOT NULL UNIQUE,   -- "gpu-server-01", "k8s-node-gpu-01"
    host         VARCHAR(512) NOT NULL,
    driver_type  VARCHAR(50)  NOT NULL DEFAULT 'docker', -- docker | kubernetes | bare_metal
    total_vram_mb INTEGER     NOT NULL DEFAULT 0,
    is_available BOOLEAN      NOT NULL DEFAULT TRUE,
    labels       JSONB        NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE gpu_devices (
    id            UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id       UUID         NOT NULL REFERENCES gpu_nodes(id) ON DELETE CASCADE,
    device_index  INTEGER      NOT NULL,          -- 0, 1, 2 …
    name          VARCHAR(255) NOT NULL,           -- "NVIDIA A100 80GB"
    vram_mb       INTEGER      NOT NULL,
    status        VARCHAR(20)  NOT NULL DEFAULT 'available'
                  CHECK (status IN ('available','allocated','error','maintenance')),
    utilization_pct  INTEGER   NOT NULL DEFAULT 0 CHECK (utilization_pct BETWEEN 0 AND 100),
    temperature_c    INTEGER   NOT NULL DEFAULT 0,
    power_draw_w     INTEGER   NOT NULL DEFAULT 0,
    last_seen_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(node_id, device_index)
);
CREATE INDEX idx_gpu_devices_node    ON gpu_devices(node_id);
CREATE INDEX idx_gpu_devices_status  ON gpu_devices(status);

CREATE TABLE gpu_allocations (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    endpoint_id     UUID        NOT NULL REFERENCES model_endpoints(id) ON DELETE CASCADE,
    gpu_device_id   UUID        NOT NULL REFERENCES gpu_devices(id) ON DELETE CASCADE,
    vram_allocated_mb INTEGER   NOT NULL,
    allocated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    released_at     TIMESTAMPTZ,
    UNIQUE(gpu_device_id, endpoint_id)
);
CREATE INDEX idx_gpu_alloc_endpoint ON gpu_allocations(endpoint_id);
CREATE INDEX idx_gpu_alloc_device   ON gpu_allocations(gpu_device_id);

CREATE TRIGGER set_gpu_nodes_updated_at
BEFORE UPDATE ON gpu_nodes FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();
CREATE TRIGGER set_gpu_devices_updated_at
BEFORE UPDATE ON gpu_devices FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. MODEL LIFECYCLE STATE
-- ─────────────────────────────────────────────────────────────────────────────

-- Extend model_endpoints with lifecycle state machine column
ALTER TABLE model_endpoints
    ADD COLUMN IF NOT EXISTS lifecycle_state VARCHAR(30) NOT NULL DEFAULT 'registered'
        CHECK (lifecycle_state IN (
            'registered','downloading','loading','warm','active',
            'idle','unloading','unloaded','failed','draining'
        ));

CREATE TABLE model_lifecycle_events (
    id           BIGSERIAL   PRIMARY KEY,
    endpoint_id  UUID        NOT NULL REFERENCES model_endpoints(id) ON DELETE CASCADE,
    from_state   VARCHAR(30),
    to_state     VARCHAR(30) NOT NULL,
    reason       TEXT,
    actor        VARCHAR(100),   -- "system" | "admin:<user_id>" | "controller"
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_lifecycle_ep_time ON model_lifecycle_events(endpoint_id, created_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. MODEL CONTROLLER OPERATIONS LOG
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE controller_operations (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id     UUID        NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    endpoint_id  UUID        REFERENCES model_endpoints(id) ON DELETE SET NULL,
    operation    VARCHAR(50) NOT NULL,  -- start|stop|restart|drain|upgrade|rollback
    status       VARCHAR(20) NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','running','success','failed','cancelled')),
    initiated_by VARCHAR(100),
    started_at   TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    error_msg    TEXT,
    metadata     JSONB NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_ctrl_ops_model  ON controller_operations(model_id, created_at DESC);
CREATE INDEX idx_ctrl_ops_status ON controller_operations(status);

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. USAGE TRACKING (ClickHouse-style schema in Postgres for single-node dev)
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE usage_events (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id            UUID        NOT NULL,
    team_id           UUID        NOT NULL,
    api_key_id        UUID,
    model_id          UUID,
    model_name        VARCHAR(255) NOT NULL,
    endpoint_id       UUID,
    request_id        VARCHAR(255),
    prompt_tokens     INTEGER     NOT NULL DEFAULT 0,
    completion_tokens INTEGER     NOT NULL DEFAULT 0,
    total_tokens      INTEGER     NOT NULL DEFAULT 0,
    latency_ms        INTEGER     NOT NULL DEFAULT 0,
    ttft_ms           INTEGER     NOT NULL DEFAULT 0,
    queue_wait_ms     INTEGER     NOT NULL DEFAULT 0,
    status            VARCHAR(20) NOT NULL DEFAULT 'success'
                      CHECK (status IN ('success','error','timeout','rejected','cancelled')),
    error_code        VARCHAR(100),
    cost_usd          NUMERIC(12,8) NOT NULL DEFAULT 0,
    gpu_time_ms       INTEGER     NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
) PARTITION BY RANGE (created_at);

CREATE TABLE usage_events_2024 PARTITION OF usage_events
    FOR VALUES FROM ('2024-01-01') TO ('2025-01-01');
CREATE TABLE usage_events_2025 PARTITION OF usage_events
    FOR VALUES FROM ('2025-01-01') TO ('2026-01-01');
CREATE TABLE usage_events_2026 PARTITION OF usage_events
    FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');

CREATE INDEX idx_usage_team_time   ON usage_events(team_id, created_at DESC);
CREATE INDEX idx_usage_org_time    ON usage_events(org_id,  created_at DESC);
CREATE INDEX idx_usage_model_time  ON usage_events(model_name, created_at DESC);

-- Hourly aggregation (materialized by background job)
CREATE TABLE usage_hourly (
    team_id           UUID        NOT NULL,
    model_name        VARCHAR(255) NOT NULL,
    hour              TIMESTAMPTZ NOT NULL,  -- truncated to hour
    request_count     BIGINT      NOT NULL DEFAULT 0,
    error_count       BIGINT      NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT      NOT NULL DEFAULT 0,
    completion_tokens BIGINT      NOT NULL DEFAULT 0,
    total_tokens      BIGINT      NOT NULL DEFAULT 0,
    total_latency_ms  BIGINT      NOT NULL DEFAULT 0,
    cost_usd          NUMERIC(16,8) NOT NULL DEFAULT 0,
    PRIMARY KEY (team_id, model_name, hour)
);

-- Daily aggregation
CREATE TABLE usage_daily (
    team_id           UUID        NOT NULL,
    model_name        VARCHAR(255) NOT NULL,
    day               DATE        NOT NULL,
    request_count     BIGINT      NOT NULL DEFAULT 0,
    error_count       BIGINT      NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT      NOT NULL DEFAULT 0,
    completion_tokens BIGINT      NOT NULL DEFAULT 0,
    total_tokens      BIGINT      NOT NULL DEFAULT 0,
    cost_usd          NUMERIC(16,8) NOT NULL DEFAULT 0,
    PRIMARY KEY (team_id, model_name, day)
);

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. PROMPT POLICY ENGINE
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE prompt_policies (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    scope               VARCHAR(20) NOT NULL CHECK (scope IN ('org','team','model')),
    scope_id            UUID        NOT NULL,  -- org_id | team_id | model_id
    name                VARCHAR(255) NOT NULL,
    priority            INTEGER     NOT NULL DEFAULT 100, -- lower = runs first
    enabled             BOOLEAN     NOT NULL DEFAULT TRUE,
    -- system prompt injection
    system_prompt       TEXT,
    system_prompt_mode  VARCHAR(20) NOT NULL DEFAULT 'prepend'
                        CHECK (system_prompt_mode IN ('prepend','append','replace','none')),
    -- content controls
    max_temperature     NUMERIC(4,2),
    max_tokens_override INTEGER,
    -- moderation
    enable_pii_detection  BOOLEAN   NOT NULL DEFAULT FALSE,
    enable_moderation     BOOLEAN   NOT NULL DEFAULT FALSE,
    -- tool/MCP restrictions
    allowed_tools       JSONB       NOT NULL DEFAULT '[]',  -- [] = all allowed
    denied_tools        JSONB       NOT NULL DEFAULT '[]',
    -- output filtering
    output_filters      JSONB       NOT NULL DEFAULT '[]',  -- [{type:"regex",pattern:"..."}]
    -- deny/allow word lists
    input_deny_list     JSONB       NOT NULL DEFAULT '[]',
    input_allow_list    JSONB       NOT NULL DEFAULT '[]',
    metadata            JSONB       NOT NULL DEFAULT '{}',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_prompt_policy_scope ON prompt_policies(scope, scope_id, priority);

CREATE TRIGGER set_prompt_policies_updated_at
BEFORE UPDATE ON prompt_policies FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. MODEL ALIASES (Virtual Models)
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE model_aliases (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    alias       VARCHAR(255) NOT NULL,            -- "gpt-4o", "reasoning", "coding"
    model_id    UUID        NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    scope       VARCHAR(20) NOT NULL CHECK (scope IN ('global','org','team')),
    scope_id    UUID,                             -- NULL for global
    enabled     BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(alias, scope, scope_id)
);
CREATE INDEX idx_aliases_alias  ON model_aliases(alias);
CREATE INDEX idx_aliases_scope  ON model_aliases(scope, scope_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- 7. AI GATEWAY POLICY LAYER
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE gateway_policies (
    id                    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    scope                 VARCHAR(20) NOT NULL CHECK (scope IN ('org','team','api_key')),
    scope_id              UUID        NOT NULL,
    max_temperature       NUMERIC(4,2),           -- NULL = no limit
    max_context_tokens    INTEGER,
    max_output_tokens     INTEGER,
    allowed_models        JSONB       NOT NULL DEFAULT '[]',  -- [] = inherit from team ACL
    denied_models         JSONB       NOT NULL DEFAULT '[]',
    allowed_tool_names    JSONB       NOT NULL DEFAULT '[]',
    denied_tool_names     JSONB       NOT NULL DEFAULT '[]',
    stream_allowed        BOOLEAN     NOT NULL DEFAULT TRUE,
    function_call_allowed BOOLEAN     NOT NULL DEFAULT TRUE,
    enabled               BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(scope, scope_id)
);

CREATE TRIGGER set_gateway_policies_updated_at
BEFORE UPDATE ON gateway_policies FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

COMMIT;
