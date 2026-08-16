-- Forensic audit (Case File 003, round 6): runtime_replica_status previously
-- filtered only on models.enabled = TRUE. Production data showed rows where
-- enabled=TRUE but lifecycle='deleted' (EnableModel re-enables a model
-- without clearing a stale 'deleted' lifecycle), which let the HA
-- reconciler's under-replication path treat a soft-deleted model as
-- legitimately under-replicated and keep spawning replacements for it.
--
-- This redefines the view identically to migration 029, adding the missing
-- lifecycle check. No other column or semantic changes.
DROP VIEW IF EXISTS runtime_replica_status CASCADE;

CREATE VIEW runtime_replica_status AS
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
  AND COALESCE(m.lifecycle,'active') != 'deleted'
GROUP BY m.id, m.name,
         rs.desired_replicas, rs.min_available, rs.placement_policy, rs.auto_recover,
         rs.max_surge, rs.health_retry_interval_s, rs.replacement_start_timeout_s,
         rs.drain_timeout_s, rs.termination_grace_s;
