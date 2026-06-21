-- NexusLLM Enterprise Platform — Migration 005 (idempotent)
BEGIN;

-- GPU Inventory
CREATE TABLE IF NOT EXISTS gpu_nodes (
    id           UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    name         VARCHAR(255) NOT NULL UNIQUE,
    host         VARCHAR(512) NOT NULL,
    driver_type  VARCHAR(50)  NOT NULL DEFAULT 'docker',
    total_vram_mb INTEGER     NOT NULL DEFAULT 0,
    is_available BOOLEAN      NOT NULL DEFAULT TRUE,
    labels       JSONB        NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS gpu_devices (
    id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id          UUID         NOT NULL REFERENCES gpu_nodes(id) ON DELETE CASCADE,
    device_index     INTEGER      NOT NULL,
    name             VARCHAR(255) NOT NULL,
    vram_mb          INTEGER      NOT NULL,
    status           VARCHAR(20)  NOT NULL DEFAULT 'available'
                     CHECK (status IN ('available','allocated','error','maintenance')),
    utilization_pct  INTEGER      NOT NULL DEFAULT 0,
    temperature_c    INTEGER      NOT NULL DEFAULT 0,
    power_draw_w     INTEGER      NOT NULL DEFAULT 0,
    last_seen_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE(node_id, device_index)
);
CREATE INDEX IF NOT EXISTS idx_gpu_devices_node   ON gpu_devices(node_id);
CREATE INDEX IF NOT EXISTS idx_gpu_devices_status ON gpu_devices(status);

CREATE TABLE IF NOT EXISTS gpu_allocations (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    endpoint_id       UUID        NOT NULL REFERENCES model_endpoints(id) ON DELETE CASCADE,
    gpu_device_id     UUID        NOT NULL REFERENCES gpu_devices(id) ON DELETE CASCADE,
    vram_allocated_mb INTEGER     NOT NULL,
    allocated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    released_at       TIMESTAMPTZ,
    UNIQUE(gpu_device_id, endpoint_id)
);
CREATE INDEX IF NOT EXISTS idx_gpu_alloc_endpoint ON gpu_allocations(endpoint_id);
CREATE INDEX IF NOT EXISTS idx_gpu_alloc_device   ON gpu_allocations(gpu_device_id);

-- Usage events (partitioned by month)
CREATE TABLE IF NOT EXISTS usage_events (
    id                UUID         NOT NULL DEFAULT gen_random_uuid(),
    org_id            UUID         NOT NULL,
    team_id           UUID         NOT NULL,
    api_key_id        UUID,
    model_id          UUID,
    model_name        VARCHAR(255) NOT NULL,
    endpoint_id       UUID,
    request_id        VARCHAR(255),
    prompt_tokens     INTEGER      NOT NULL DEFAULT 0,
    completion_tokens INTEGER      NOT NULL DEFAULT 0,
    total_tokens      INTEGER      NOT NULL DEFAULT 0,
    latency_ms        INTEGER      NOT NULL DEFAULT 0,
    ttft_ms           INTEGER      NOT NULL DEFAULT 0,
    queue_wait_ms     INTEGER      NOT NULL DEFAULT 0,
    status            VARCHAR(20)  NOT NULL DEFAULT 'success',
    error_code        VARCHAR(100),
    cost_usd          NUMERIC(12,8) NOT NULL DEFAULT 0,
    gpu_time_ms       INTEGER      NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

CREATE TABLE IF NOT EXISTS usage_events_2025
    PARTITION OF usage_events FOR VALUES FROM ('2025-01-01') TO ('2026-01-01');
CREATE TABLE IF NOT EXISTS usage_events_2026
    PARTITION OF usage_events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
CREATE TABLE IF NOT EXISTS usage_events_2027
    PARTITION OF usage_events FOR VALUES FROM ('2027-01-01') TO ('2028-01-01');

CREATE INDEX IF NOT EXISTS idx_usage_team_time  ON usage_events(team_id,  created_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_org_time   ON usage_events(org_id,   created_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_model_time ON usage_events(model_name, created_at DESC);

CREATE TABLE IF NOT EXISTS usage_hourly (
    team_id           UUID         NOT NULL,
    model_name        VARCHAR(255) NOT NULL,
    hour              TIMESTAMPTZ  NOT NULL,
    request_count     BIGINT       NOT NULL DEFAULT 0,
    error_count       BIGINT       NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT       NOT NULL DEFAULT 0,
    completion_tokens BIGINT       NOT NULL DEFAULT 0,
    total_tokens      BIGINT       NOT NULL DEFAULT 0,
    cost_usd          NUMERIC(16,8) NOT NULL DEFAULT 0,
    PRIMARY KEY (team_id, model_name, hour)
);

CREATE TABLE IF NOT EXISTS usage_daily (
    team_id           UUID         NOT NULL,
    model_name        VARCHAR(255) NOT NULL,
    day               DATE         NOT NULL,
    request_count     BIGINT       NOT NULL DEFAULT 0,
    error_count       BIGINT       NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT       NOT NULL DEFAULT 0,
    completion_tokens BIGINT       NOT NULL DEFAULT 0,
    total_tokens      BIGINT       NOT NULL DEFAULT 0,
    cost_usd          NUMERIC(16,8) NOT NULL DEFAULT 0,
    PRIMARY KEY (team_id, model_name, day)
);

-- Prompt Policies
CREATE TABLE IF NOT EXISTS prompt_policies (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    scope                VARCHAR(20) NOT NULL CHECK (scope IN ('org','team','model')),
    scope_id             UUID        NOT NULL,
    name                 VARCHAR(255) NOT NULL,
    priority             INTEGER     NOT NULL DEFAULT 100,
    enabled              BOOLEAN     NOT NULL DEFAULT TRUE,
    system_prompt        TEXT,
    system_prompt_mode   VARCHAR(20) NOT NULL DEFAULT 'prepend'
                         CHECK (system_prompt_mode IN ('prepend','append','replace','none')),
    max_temperature      NUMERIC(4,2),
    max_tokens_override  INTEGER,
    enable_pii_detection BOOLEAN     NOT NULL DEFAULT FALSE,
    enable_moderation    BOOLEAN     NOT NULL DEFAULT FALSE,
    allowed_tools        JSONB       NOT NULL DEFAULT '[]',
    denied_tools         JSONB       NOT NULL DEFAULT '[]',
    output_filters       JSONB       NOT NULL DEFAULT '[]',
    input_deny_list      JSONB       NOT NULL DEFAULT '[]',
    input_allow_list     JSONB       NOT NULL DEFAULT '[]',
    metadata             JSONB       NOT NULL DEFAULT '{}',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_prompt_policy_scope ON prompt_policies(scope, scope_id, priority);

-- Model Aliases
CREATE TABLE IF NOT EXISTS model_aliases (
    id         UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    alias      VARCHAR(255) NOT NULL,
    model_id   UUID         NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    scope      VARCHAR(20)  NOT NULL CHECK (scope IN ('global','org','team')),
    scope_id   UUID,
    enabled    BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE(alias, scope, scope_id)
);
CREATE INDEX IF NOT EXISTS idx_aliases_alias ON model_aliases(alias);
CREATE INDEX IF NOT EXISTS idx_aliases_scope ON model_aliases(scope, scope_id);

-- Gateway Policies
CREATE TABLE IF NOT EXISTS gateway_policies (
    id                    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    scope                 VARCHAR(20) NOT NULL CHECK (scope IN ('org','team','api_key')),
    scope_id              UUID        NOT NULL,
    max_temperature       NUMERIC(4,2),
    max_context_tokens    INTEGER,
    max_output_tokens     INTEGER,
    allowed_models        JSONB       NOT NULL DEFAULT '[]',
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

-- Budgets
CREATE TABLE IF NOT EXISTS budgets (
    id             UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    scope          VARCHAR(20)  NOT NULL CHECK (scope IN ('org','team')),
    scope_id       UUID         NOT NULL,
    period         VARCHAR(10)  NOT NULL DEFAULT 'monthly',
    limit_usd      NUMERIC(12,4) NOT NULL,
    soft_limit_pct INTEGER      NOT NULL DEFAULT 80,
    hard_limit_pct INTEGER      NOT NULL DEFAULT 100,
    action_on_hard VARCHAR(20)  NOT NULL DEFAULT 'throttle',
    notify_emails  JSONB        NOT NULL DEFAULT '[]',
    enabled        BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE(scope, scope_id, period)
);

COMMIT;
