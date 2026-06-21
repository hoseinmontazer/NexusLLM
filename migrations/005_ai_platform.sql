-- NexusLLM AI Platform — Migration 005
-- Extends the platform from an LLM gateway into a full AI resource orchestrator.
-- Idempotent (safe to re-run).
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. CLUSTER NODES
-- Represents physical or virtual servers. Single-server deployments have one
-- row. The abstraction exists so multi-server expansion later requires only
-- new rows here, not API changes.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS nodes (
    id            UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    hostname      VARCHAR(255) NOT NULL UNIQUE,
    display_name  VARCHAR(255) NOT NULL DEFAULT '',
    -- Hardware totals (as reported by node agent)
    total_cpu     INTEGER      NOT NULL DEFAULT 0,   -- logical vCPUs
    total_ram_mb  BIGINT       NOT NULL DEFAULT 0,   -- total RAM in MB
    total_vram_mb BIGINT       NOT NULL DEFAULT 0,   -- sum of all GPU VRAM
    -- Observed state
    status        VARCHAR(20)  NOT NULL DEFAULT 'unknown'
                  CHECK (status IN ('online','offline','degraded','maintenance','unknown')),
    labels        JSONB        NOT NULL DEFAULT '{}', -- arbitrary key/value tags
    -- Agent metadata
    agent_version VARCHAR(50),
    last_heartbeat_at TIMESTAMPTZ,
    -- Timestamps
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_nodes_status    ON nodes(status);
CREATE INDEX IF NOT EXISTS idx_nodes_hostname  ON nodes(hostname);
DROP TRIGGER IF EXISTS set_nodes_updated_at ON nodes;
CREATE TRIGGER set_nodes_updated_at
    BEFORE UPDATE ON nodes FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. EXTENDED GPU NODES — link to cluster nodes
-- gpu_nodes already exists; we extend it to reference the nodes table.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE gpu_nodes ADD COLUMN IF NOT EXISTS node_id UUID REFERENCES nodes(id) ON DELETE SET NULL;

-- Extend gpu_devices with richer telemetry
ALTER TABLE gpu_devices ADD COLUMN IF NOT EXISTS numa_node      INTEGER     NOT NULL DEFAULT 0;
ALTER TABLE gpu_devices ADD COLUMN IF NOT EXISTS pcie_bus_id    VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE gpu_devices ADD COLUMN IF NOT EXISTS compute_cap    VARCHAR(20) NOT NULL DEFAULT '';

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. NODE HARDWARE TELEMETRY
-- Time-series snapshots reported by the node agent.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS node_telemetry (
    id             BIGSERIAL   PRIMARY KEY,
    node_id        UUID        NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    -- CPU
    cpu_cores_total   INTEGER  NOT NULL DEFAULT 0,
    cpu_cores_used    INTEGER  NOT NULL DEFAULT 0,
    cpu_util_pct      NUMERIC(5,2) NOT NULL DEFAULT 0,
    -- RAM
    ram_total_mb      BIGINT   NOT NULL DEFAULT 0,
    ram_used_mb       BIGINT   NOT NULL DEFAULT 0,
    ram_avail_mb      BIGINT   NOT NULL DEFAULT 0,
    -- NUMA
    numa_nodes        INTEGER  NOT NULL DEFAULT 1,
    numa_topology     JSONB    NOT NULL DEFAULT '{}',
    -- Disk
    disk_total_gb     BIGINT   NOT NULL DEFAULT 0,
    disk_used_gb      BIGINT   NOT NULL DEFAULT 0,
    -- Network
    net_rx_mbps       NUMERIC(10,2) NOT NULL DEFAULT 0,
    net_tx_mbps       NUMERIC(10,2) NOT NULL DEFAULT 0,
    -- Recorded at
    recorded_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_node_telemetry_node_time
    ON node_telemetry(node_id, recorded_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. GPU TELEMETRY (per-device time-series)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS gpu_telemetry (
    id              BIGSERIAL   PRIMARY KEY,
    device_id       UUID        NOT NULL REFERENCES gpu_devices(id) ON DELETE CASCADE,
    utilization_pct INTEGER     NOT NULL DEFAULT 0,
    memory_used_mb  INTEGER     NOT NULL DEFAULT 0,
    memory_total_mb INTEGER     NOT NULL DEFAULT 0,
    temperature_c   INTEGER     NOT NULL DEFAULT 0,
    power_draw_w    INTEGER     NOT NULL DEFAULT 0,
    power_limit_w   INTEGER     NOT NULL DEFAULT 0,
    fan_speed_pct   INTEGER     NOT NULL DEFAULT 0,
    recorded_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_gpu_telemetry_device_time
    ON gpu_telemetry(device_id, recorded_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. RUNTIME TYPES
-- Extend model_endpoints with runtime_type to distinguish GPU vs CPU workloads.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE model_endpoints ADD COLUMN IF NOT EXISTS runtime_type VARCHAR(20) NOT NULL DEFAULT 'GPU_RUNTIME'
    CHECK (runtime_type IN ('GPU_RUNTIME','CPU_RUNTIME'));
ALTER TABLE model_endpoints ADD COLUMN IF NOT EXISTS node_id UUID REFERENCES nodes(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_endpoints_node_id ON model_endpoints(node_id);
CREATE INDEX IF NOT EXISTS idx_endpoints_runtime_type ON model_endpoints(runtime_type);

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. AI SERVICE REGISTRY
-- Extends the model registry to cover all AI service types beyond LLMs.
-- service_type is the primary routing key used by the gateway.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE models ADD COLUMN IF NOT EXISTS service_type VARCHAR(20) NOT NULL DEFAULT 'CHAT'
    CHECK (service_type IN (
        'CHAT',       -- LLM chat completion (existing)
        'EMBEDDING',  -- Dense embedding models
        'RERANK',     -- Cross-encoder rerankers
        'STT',        -- Speech-to-text / transcription
        'TTS',        -- Text-to-speech
        'OCR',        -- Optical character recognition
        'AGENT',      -- Agent runtimes / long-running services
        'MCP'         -- Model Context Protocol servers
    ));

ALTER TABLE models ADD COLUMN IF NOT EXISTS runtime_type VARCHAR(20) NOT NULL DEFAULT 'GPU_RUNTIME'
    CHECK (runtime_type IN ('GPU_RUNTIME','CPU_RUNTIME'));

CREATE INDEX IF NOT EXISTS idx_models_service_type  ON models(service_type);
CREATE INDEX IF NOT EXISTS idx_models_runtime_type  ON models(runtime_type);

-- ─────────────────────────────────────────────────────────────────────────────
-- 7. RESOURCE RESERVATIONS
-- Services declare minimum/maximum resource envelopes.
-- The placement engine uses these to decide where to schedule a service.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS resource_reservations (
    id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id        UUID         NOT NULL REFERENCES models(id) ON DELETE CASCADE UNIQUE,
    -- GPU resources
    min_vram_mb     BIGINT       NOT NULL DEFAULT 0,
    max_vram_mb     BIGINT       NOT NULL DEFAULT 0,
    -- CPU resources
    cpu_cores       INTEGER      NOT NULL DEFAULT 0,   -- 0 = no CPU affinity enforced
    numa_node_pref  INTEGER      NOT NULL DEFAULT -1,  -- -1 = no preference
    -- RAM
    ram_mb          BIGINT       NOT NULL DEFAULT 0,   -- 0 = no limit
    -- Priority for placement decisions
    priority        VARCHAR(20)  NOT NULL DEFAULT 'normal'
                    CHECK (priority IN ('critical','high','normal','low','best_effort')),
    -- Scheduling hint: prefer GPU or CPU
    preferred_runtime VARCHAR(20) NOT NULL DEFAULT 'GPU_RUNTIME'
                    CHECK (preferred_runtime IN ('GPU_RUNTIME','CPU_RUNTIME','ANY')),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
DROP TRIGGER IF EXISTS set_reservations_updated_at ON resource_reservations;
CREATE TRIGGER set_reservations_updated_at
    BEFORE UPDATE ON resource_reservations FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- 8. PLACEMENT DECISIONS (audit log for the placement engine)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS placement_decisions (
    id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id        UUID         NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    endpoint_id     UUID         REFERENCES model_endpoints(id) ON DELETE SET NULL,
    node_id         UUID         REFERENCES nodes(id) ON DELETE SET NULL,
    -- What the engine decided
    gpu_devices     JSONB        NOT NULL DEFAULT '[]',  -- []int device indices
    cpu_cores       INTEGER      NOT NULL DEFAULT 0,
    numa_node       INTEGER      NOT NULL DEFAULT -1,
    ram_mb          BIGINT       NOT NULL DEFAULT 0,
    -- Why
    strategy        VARCHAR(50)  NOT NULL DEFAULT 'auto',
    score           NUMERIC(8,4) NOT NULL DEFAULT 0,
    reason          TEXT,
    -- Outcome
    applied         BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_placement_model ON placement_decisions(model_id, created_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 9. CPU ALLOCATIONS
-- Tracks CPU core reservations for CPU_RUNTIME services (analogous to
-- gpu_allocations for GPU workloads).
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS cpu_allocations (
    id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    endpoint_id     UUID         NOT NULL REFERENCES model_endpoints(id) ON DELETE CASCADE,
    node_id         UUID         REFERENCES nodes(id) ON DELETE SET NULL,
    cpu_cores       INTEGER      NOT NULL DEFAULT 0,
    numa_node       INTEGER      NOT NULL DEFAULT -1,
    ram_mb          BIGINT       NOT NULL DEFAULT 0,
    released_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_cpu_alloc_endpoint ON cpu_allocations(endpoint_id);
CREATE INDEX IF NOT EXISTS idx_cpu_alloc_node     ON cpu_allocations(node_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- 10. NODE AGENT INVENTORY SNAPSHOTS
-- Agent pushes a full hardware snapshot on startup and periodically.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS node_inventory_snapshots (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id     UUID        NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    snapshot    JSONB       NOT NULL DEFAULT '{}',
    agent_ver   VARCHAR(50) NOT NULL DEFAULT '',
    reported_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_inv_snapshot_node_time
    ON node_inventory_snapshots(node_id, reported_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 11. EXTEND model_runtime_configs for CPU runtime params
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE model_runtime_configs ADD COLUMN IF NOT EXISTS cpu_threads   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE model_runtime_configs ADD COLUMN IF NOT EXISTS numa_node     INTEGER NOT NULL DEFAULT -1;
ALTER TABLE model_runtime_configs ADD COLUMN IF NOT EXISTS memory_limit  VARCHAR(20) NOT NULL DEFAULT '';

COMMIT;
