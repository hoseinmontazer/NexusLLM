-- NexusLLM Migration 007 — Production Node Agent
-- Introduces:
--   - node_tokens     : per-node JWT auth credentials
--   - agent_tasks     : command queue from control plane → agent
--   - agent_runtimes  : first-class runtime entity tracked per node
--   - node_capabilities: what each agent can do
-- All idempotent.
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. NODE TOKENS
-- Each node gets a JWT token issued by the control plane on registration.
-- The agent presents this token on every API call.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS node_tokens (
    id          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id     UUID         NOT NULL REFERENCES nodes(id) ON DELETE CASCADE UNIQUE,
    token_hash  VARCHAR(64)  NOT NULL,       -- SHA-256 of the token
    issued_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    expires_at  TIMESTAMPTZ,                 -- NULL = no expiry
    revoked     BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_node_tokens_hash    ON node_tokens(token_hash);
CREATE INDEX IF NOT EXISTS idx_node_tokens_node_id ON node_tokens(node_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. NODE CAPABILITIES
-- Reported by the agent on registration / heartbeat.
-- The scheduler uses these to filter eligible nodes for each service type.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS node_capabilities (
    node_id       UUID     PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    -- Container runtimes
    has_docker    BOOLEAN  NOT NULL DEFAULT FALSE,
    has_podman    BOOLEAN  NOT NULL DEFAULT FALSE,
    -- AI backends
    has_vllm      BOOLEAN  NOT NULL DEFAULT FALSE,
    has_ollama    BOOLEAN  NOT NULL DEFAULT FALSE,
    has_tgi       BOOLEAN  NOT NULL DEFAULT FALSE,
    -- CPU service backends
    has_whisper   BOOLEAN  NOT NULL DEFAULT FALSE,
    has_tts       BOOLEAN  NOT NULL DEFAULT FALSE,
    has_embedding BOOLEAN  NOT NULL DEFAULT FALSE,
    -- GPU present
    has_gpu       BOOLEAN  NOT NULL DEFAULT FALSE,
    gpu_count     INTEGER  NOT NULL DEFAULT 0,
    -- Raw JSON for arbitrary future capabilities
    extra         JSONB    NOT NULL DEFAULT '{}',
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. AGENT TASKS
-- The task queue. Control Plane writes tasks here; the agent polls and executes.
-- One task per work unit. The agent acks completion by updating status + result.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS agent_tasks (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id      UUID        NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,

    -- Task classification
    task_type    VARCHAR(50) NOT NULL
                 CHECK (task_type IN (
                     -- Runtime lifecycle
                     'DEPLOY_RUNTIME',
                     'STOP_RUNTIME',
                     'RESTART_RUNTIME',
                     'DELETE_RUNTIME',
                     'WARM_RUNTIME',
                     'UNLOAD_RUNTIME',
                     -- Model management
                     'PULL_MODEL',
                     'DELETE_MODEL',
                     'VERIFY_MODEL',
                     -- Observability
                     'COLLECT_INVENTORY',
                     'HEALTH_CHECK'
                 )),

    -- Payload: task-specific JSON parameters
    -- For DEPLOY_RUNTIME: {runtime_spec, endpoint_id, model_id, ...}
    -- For PULL_MODEL:     {model_id, source, hf_repo, ...}
    -- For HEALTH_CHECK:   {runtime_ids: [...]}
    payload      JSONB       NOT NULL DEFAULT '{}',

    -- Execution state
    status       VARCHAR(20) NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','claimed','running','success','failed','cancelled','timeout')),

    -- Priority (higher = processed first within same node)
    priority     INTEGER     NOT NULL DEFAULT 50,

    -- Who created this task
    created_by   VARCHAR(100),

    -- Timing
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    claimed_at   TIMESTAMPTZ,          -- when agent picked it up
    started_at   TIMESTAMPTZ,          -- when execution began
    completed_at TIMESTAMPTZ,
    timeout_at   TIMESTAMPTZ,          -- task expires if not claimed by this time

    -- Result
    result       JSONB,                -- success: {"container_id":"..."} etc.
    error_msg    TEXT,

    -- Idempotency: control plane can retry by checking this key
    idempotency_key VARCHAR(128) UNIQUE
);
CREATE INDEX IF NOT EXISTS idx_agent_tasks_node_pending
    ON agent_tasks(node_id, status, priority DESC, created_at)
    WHERE status IN ('pending','claimed');
CREATE INDEX IF NOT EXISTS idx_agent_tasks_status     ON agent_tasks(status);
CREATE INDEX IF NOT EXISTS idx_agent_tasks_created    ON agent_tasks(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_tasks_type       ON agent_tasks(task_type);

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. AGENT RUNTIMES
-- A runtime is a first-class entity: a running AI service instance on a node.
-- Decoupled from model_endpoints so the agent can track its own state.
-- model_endpoints still exists for gateway routing; agent_runtimes is the
-- ground truth for what's actually running on each node.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS agent_runtimes (
    id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id         UUID         NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    endpoint_id     UUID         REFERENCES model_endpoints(id) ON DELETE SET NULL,
    model_id        UUID         REFERENCES models(id) ON DELETE SET NULL,

    -- Runtime identity
    runtime_name    VARCHAR(255) NOT NULL,   -- e.g. "nexus-qwen3-32b"
    backend         VARCHAR(50)  NOT NULL,   -- vllm | ollama | tgi | cpu_native | whisper
    container_id    VARCHAR(255) NOT NULL DEFAULT '',

    -- State machine
    state           VARCHAR(30)  NOT NULL DEFAULT 'pending'
                    CHECK (state IN (
                        'pending',      -- task dispatched, not yet started
                        'pulling',      -- pulling model weights
                        'starting',     -- container starting
                        'loading',      -- model loading into VRAM
                        'warm',         -- loaded, not yet serving
                        'active',       -- serving requests
                        'idle',         -- loaded but no recent requests
                        'stopping',     -- drain in progress
                        'stopped',      -- container stopped, weights retained
                        'unloaded',     -- weights evicted from VRAM
                        'failed',       -- error state
                        'deleted'       -- fully removed
                    )),

    -- Assigned resources (decided by control plane, NOT the agent)
    gpu_ids         JSONB        NOT NULL DEFAULT '[]',    -- [0,1]
    cpu_affinity    VARCHAR(100) NOT NULL DEFAULT '',      -- "0-31"
    memory_limit    VARCHAR(20)  NOT NULL DEFAULT '',      -- "64g"
    numa_node       INTEGER      NOT NULL DEFAULT -1,

    -- Network
    bind_host       VARCHAR(255) NOT NULL DEFAULT 'localhost',
    bind_port       INTEGER      NOT NULL DEFAULT 0,

    -- Health
    health_status   VARCHAR(20)  NOT NULL DEFAULT 'unknown'
                    CHECK (health_status IN ('healthy','degraded','down','unknown')),
    last_health_at  TIMESTAMPTZ,

    -- Timestamps
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    started_at      TIMESTAMPTZ,
    stopped_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_agent_runtimes_node     ON agent_runtimes(node_id, state);
CREATE INDEX IF NOT EXISTS idx_agent_runtimes_endpoint ON agent_runtimes(endpoint_id);
CREATE INDEX IF NOT EXISTS idx_agent_runtimes_state    ON agent_runtimes(state);

DROP TRIGGER IF EXISTS set_agent_runtimes_updated_at ON agent_runtimes;
CREATE TRIGGER set_agent_runtimes_updated_at
    BEFORE UPDATE ON agent_runtimes
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. TASK → RUNTIME LINK
-- One task may produce or affect one runtime.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE agent_tasks ADD COLUMN IF NOT EXISTS runtime_id UUID REFERENCES agent_runtimes(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_agent_tasks_runtime ON agent_tasks(runtime_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. EXTEND nodes TABLE
-- Add node_token_id for fast auth lookup and ip field for routing.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS ip_address  INET;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS capabilities JSONB NOT NULL DEFAULT '{}';

COMMIT;
