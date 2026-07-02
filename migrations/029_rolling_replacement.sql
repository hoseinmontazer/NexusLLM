-- NexusLLM Migration 029 — Kubernetes-style Rolling Replacement
--
-- Implements zero-downtime rolling replacement for runtime replicas:
--
--   UNHEALTHY → CREATE REPLACEMENT → WAIT FOR REPLACEMENT READY
--             → DRAIN OLD RUNTIME → STOP OLD RUNTIME
--
-- Changes:
--   1. Add 'draining' and 'unhealthy' to agent_runtimes state constraint.
--   2. Add rolling-replacement config columns to model_replica_specs.
--   3. Add replaced_by FK so the reconciler can track which runtime is the
--      replacement for a draining one.
--   4. Update runtime_replica_status view to include draining count and
--      expose max_surge and rolling-replacement config.
--   5. Update claim_replica_slot() to allow surge:
--      non_terminal_excluding_draining < desired + max_surge
--
-- All statements are idempotent.
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. EXTEND agent_runtimes state constraint
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE agent_runtimes DROP CONSTRAINT IF EXISTS agent_runtimes_state_check;
ALTER TABLE agent_runtimes ADD CONSTRAINT agent_runtimes_state_check
    CHECK (state IN (
        'created', 'validating', 'downloading', 'starting',
        'loading_model', 'waiting_ready', 'ready',
        'idle', 'stopping', 'stopped',
        'pending', 'pulling', 'loading', 'warm', 'active',
        'unhealthy',   -- health check failed; replacement will be started
        'draining',    -- replacement is READY; old runtime serves in-flight requests, no new routing
        'failed', 'unloaded', 'lost', 'archived', 'deleted',
        'recovering'
    ));

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. Add replaced_by FK to agent_runtimes
--    When an unhealthy runtime is being replaced, replaced_by points to the
--    new (replacement) runtime. The reconciler uses this to avoid spawning
--    multiple replacements.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE agent_runtimes
    ADD COLUMN IF NOT EXISTS replaced_by UUID
        REFERENCES agent_runtimes(id) ON DELETE SET NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. Add rolling-replacement configuration to model_replica_specs
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE model_replica_specs
    -- Seconds to wait between health failure and starting replacement
    ADD COLUMN IF NOT EXISTS health_retry_interval_s  INTEGER NOT NULL DEFAULT 30,
    -- Seconds replacement has to become READY before it is itself marked failed
    ADD COLUMN IF NOT EXISTS replacement_start_timeout_s INTEGER NOT NULL DEFAULT 900,
    -- Seconds a draining runtime waits for in-flight requests to finish
    ADD COLUMN IF NOT EXISTS drain_timeout_s          INTEGER NOT NULL DEFAULT 30,
    -- Seconds after STOP_RUNTIME task dispatched before forceful removal
    ADD COLUMN IF NOT EXISTS termination_grace_s      INTEGER NOT NULL DEFAULT 15;

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. Update runtime_replica_status view
--    Add:  draining_replicas, unhealthy_replicas
--    Expose: max_surge, drain_timeout_s, health_retry_interval_s
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE VIEW runtime_replica_status AS
SELECT
    m.id                                                    AS model_id,
    m.name                                                  AS model_name,
    COALESCE(rs.desired_replicas, 1)                        AS desired_replicas,
    COALESCE(rs.min_available, 1)                           AS min_available,
    COALESCE(rs.placement_policy, 'spread')                 AS placement_policy,
    COALESCE(rs.auto_recover, TRUE)                         AS auto_recover,
    COALESCE(rs.max_surge, 1)                               AS max_surge,
    COALESCE(rs.health_retry_interval_s, 30)                AS health_retry_interval_s,
    COALESCE(rs.replacement_start_timeout_s, 900)           AS replacement_start_timeout_s,
    COALESCE(rs.drain_timeout_s, 30)                        AS drain_timeout_s,
    COALESCE(rs.termination_grace_s, 15)                    AS termination_grace_s,

    -- Active (currently serving — used for routing)
    COUNT(ar.id) FILTER (
        WHERE ar.state IN ('active','warm','ready')
    )                                                       AS active_replicas,

    -- Starting (in pipeline — counts toward non-terminal but not yet serving)
    COUNT(ar.id) FILTER (
        WHERE ar.state IN ('created','validating','downloading',
                           'starting','loading_model','waiting_ready',
                           'pending','pulling','loading','recovering')
    )                                                       AS starting_replicas,

    -- Idle (loaded but no traffic)
    COUNT(ar.id) FILTER (
        WHERE ar.state = 'idle'
    )                                                       AS idle_replicas,

    -- Unhealthy (health check failed; awaiting replacement)
    COUNT(ar.id) FILTER (
        WHERE ar.state = 'unhealthy'
    )                                                       AS unhealthy_replicas,

    -- Draining (replacement READY; old runtime finishing in-flight requests)
    COUNT(ar.id) FILTER (
        WHERE ar.state = 'draining'
    )                                                       AS draining_replicas,

    -- Lost (node went offline)
    COUNT(ar.id) FILTER (
        WHERE ar.state = 'lost'
    )                                                       AS lost_replicas,

    -- Node distribution
    COUNT(DISTINCT ar.node_id) FILTER (
        WHERE ar.state IN ('active','warm','ready','idle','loading_model','waiting_ready')
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
GROUP BY m.id, m.name,
         rs.desired_replicas, rs.min_available, rs.placement_policy, rs.auto_recover,
         rs.max_surge, rs.health_retry_interval_s, rs.replacement_start_timeout_s,
         rs.drain_timeout_s, rs.termination_grace_s;

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. Update claim_replica_slot() to understand surge
--
-- During rolling replacement the total non-terminal count temporarily exceeds
-- desired_replicas because a replacement is starting while the old (now
-- unhealthy/draining) one is still alive.  Without surge awareness the guard
-- would block the replacement slot.
--
-- New rule:
--   count(non_terminal, excluding draining) < desired + max_surge
--
-- Draining rows are excluded because they are already being torn down and
-- should not consume a "forward-looking" replica slot.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION claim_replica_slot(
    p_model_id       UUID,
    p_desired        INTEGER,
    p_max_surge      INTEGER DEFAULT 1
) RETURNS BOOLEAN
LANGUAGE plpgsql AS $$
DECLARE
    v_non_terminal INTEGER;
    v_limit        INTEGER;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext(p_model_id::text));

    -- Count live (non-terminal, non-draining) rows.
    -- Draining rows are on their way out and must not block new replacements.
    SELECT COUNT(*) INTO v_non_terminal
    FROM agent_runtimes
    WHERE model_id = p_model_id
      AND state NOT IN ('stopped','deleted','archived','unloaded','lost','draining');

    -- Allow up to desired + max_surge rows during rolling replacement.
    v_limit := p_desired + COALESCE(p_max_surge, 1);

    RETURN v_non_terminal < v_limit;
END;
$$;

COMMENT ON FUNCTION claim_replica_slot(UUID, INTEGER, INTEGER) IS
'Atomically checks whether a new replica slot is available.
During rolling replacement the limit is desired + max_surge.
Draining rows are excluded from the count because they are being torn down.';

COMMIT;
