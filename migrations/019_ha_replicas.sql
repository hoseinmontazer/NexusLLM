-- NexusLLM Migration 019 — High Availability & Runtime Replicas
-- Adds: replica specs, placement policies, runtime_replicas view, HA state
-- The reconciler uses model_replica_specs as desired state and agent_runtimes
-- as actual state. All statements are idempotent.
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. MODEL REPLICA SPECS (desired state)
-- Per-model declaration of how many replicas should run and how to place them.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS model_replica_specs (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id          UUID        NOT NULL UNIQUE REFERENCES models(id) ON DELETE CASCADE,

    -- Replica counts
    desired_replicas  INTEGER     NOT NULL DEFAULT 1 CHECK (desired_replicas BETWEEN 0 AND 32),
    min_available     INTEGER     NOT NULL DEFAULT 1 CHECK (min_available BETWEEN 0 AND 32),

    -- Placement policy
    --   spread      — prefer different nodes for each replica (resilience)
    --   pack        — prefer same node (minimise resource fragmentation)
    --   anti_affinity — hard requirement: never two replicas on same node
    placement_policy  VARCHAR(20) NOT NULL DEFAULT 'spread'
                      CHECK (placement_policy IN ('spread','pack','anti_affinity')),

    -- HA config
    auto_recover      BOOLEAN     NOT NULL DEFAULT TRUE,  -- recover lost replicas automatically
    recovery_delay_s  INTEGER     NOT NULL DEFAULT 30,    -- seconds before recovery attempt
    max_surge         INTEGER     NOT NULL DEFAULT 1,     -- extra replicas allowed during failover

    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

DROP TRIGGER IF EXISTS set_model_replica_specs_updated_at ON model_replica_specs;
CREATE TRIGGER set_model_replica_specs_updated_at
    BEFORE UPDATE ON model_replica_specs
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE INDEX IF NOT EXISTS idx_replica_specs_model ON model_replica_specs(model_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. EXTEND agent_runtimes WITH replica identity
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE agent_runtimes
    ADD COLUMN IF NOT EXISTS replica_index     INTEGER,       -- 0-based index in replica set
    ADD COLUMN IF NOT EXISTS is_primary        BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS recovery_attempt  INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS recovered_from    UUID    REFERENCES agent_runtimes(id) ON DELETE SET NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. EXTEND agent_runtimes state constraint — add RECOVERING
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE agent_runtimes DROP CONSTRAINT IF EXISTS agent_runtimes_state_check;
ALTER TABLE agent_runtimes ADD CONSTRAINT agent_runtimes_state_check
    CHECK (state IN (
        'created', 'validating', 'downloading', 'starting',
        'loading_model', 'waiting_ready', 'ready',
        'idle', 'stopping', 'stopped',
        'pending', 'pulling', 'loading', 'warm', 'active',
        'unhealthy', 'failed', 'unloaded', 'lost', 'archived', 'deleted',
        'recovering'   -- replica lost and recovery has been triggered
    ));

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. RUNTIME RECOVERY LOG
-- Audit trail for every automatic recovery action.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS runtime_recovery_log (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id            UUID        NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    model_name          TEXT        NOT NULL DEFAULT '',
    lost_runtime_id     UUID        REFERENCES agent_runtimes(id) ON DELETE SET NULL,
    lost_node_id        UUID        REFERENCES nodes(id) ON DELETE SET NULL,
    new_runtime_id      UUID        REFERENCES agent_runtimes(id) ON DELETE SET NULL,
    new_node_id         UUID        REFERENCES nodes(id) ON DELETE SET NULL,
    replica_index       INTEGER,
    trigger             VARCHAR(30) NOT NULL DEFAULT 'node_offline'
                        CHECK (trigger IN ('node_offline','health_fail','manual','reconcile')),
    status              VARCHAR(20) NOT NULL DEFAULT 'initiated'
                        CHECK (status IN ('initiated','success','failed','skipped')),
    reason              TEXT        NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at        TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_recovery_log_model  ON runtime_recovery_log(model_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_recovery_log_status ON runtime_recovery_log(status, created_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. RECONCILER STATE (singleton heartbeat + last sweep metadata)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS reconciler_state (
    singleton           BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton = TRUE),
    last_sweep_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    models_checked      INTEGER     NOT NULL DEFAULT 0,
    recoveries_triggered INTEGER    NOT NULL DEFAULT 0,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO reconciler_state (singleton) VALUES (TRUE) ON CONFLICT DO NOTHING;

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. REPLICA STATUS VIEW (live desired vs actual reconciliation view)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE VIEW runtime_replica_status AS
SELECT
    m.id                                                    AS model_id,
    m.name                                                  AS model_name,
    COALESCE(rs.desired_replicas, 1)                        AS desired_replicas,
    COALESCE(rs.min_available, 1)                           AS min_available,
    COALESCE(rs.placement_policy, 'spread')                 AS placement_policy,
    COALESCE(rs.auto_recover, TRUE)                         AS auto_recover,

    -- Active (currently serving)
    COUNT(ar.id) FILTER (
        WHERE ar.state IN ('active','warm','ready')
    )                                                       AS active_replicas,

    -- Starting (in pipeline)
    COUNT(ar.id) FILTER (
        WHERE ar.state IN ('created','validating','downloading',
                           'starting','loading_model','waiting_ready',
                           'pending','pulling','loading','recovering')
    )                                                       AS starting_replicas,

    -- Idle (loaded but no traffic)
    COUNT(ar.id) FILTER (
        WHERE ar.state = 'idle'
    )                                                       AS idle_replicas,

    -- Lost (node went offline)
    COUNT(ar.id) FILTER (
        WHERE ar.state = 'lost'
    )                                                       AS lost_replicas,

    -- Node distribution (distinct nodes with active/starting runtimes)
    COUNT(DISTINCT ar.node_id) FILTER (
        WHERE ar.state IN ('active','warm','ready','idle',
                           'loading_model','waiting_ready')
    )                                                       AS node_count,

    -- Health: OK when active+idle >= min_available
    CASE
        WHEN COUNT(ar.id) FILTER (WHERE ar.state IN ('active','warm','ready','idle'))
             >= COALESCE(rs.min_available, 1)
        THEN 'healthy'
        WHEN COUNT(ar.id) FILTER (WHERE ar.state IN ('active','warm','ready','idle')) > 0
        THEN 'degraded'
        ELSE 'unavailable'
    END                                                     AS ha_status

FROM models m
LEFT JOIN model_replica_specs rs ON rs.model_id = m.id
LEFT JOIN agent_runtimes ar      ON ar.model_id = m.id
                                 AND ar.state NOT IN ('stopped','deleted','archived','unloaded','failed')
WHERE m.enabled = TRUE
GROUP BY m.id, m.name, rs.desired_replicas, rs.min_available,
         rs.placement_policy, rs.auto_recover;

-- ─────────────────────────────────────────────────────────────────────────────
-- 7. SEED DEFAULT REPLICA SPECS for all existing models
-- Defaults: desired=1, min_available=1, spread, auto_recover=true
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO model_replica_specs (model_id, desired_replicas, min_available)
SELECT id, 1, 1 FROM models
WHERE id NOT IN (SELECT model_id FROM model_replica_specs)
ON CONFLICT DO NOTHING;

COMMIT;
