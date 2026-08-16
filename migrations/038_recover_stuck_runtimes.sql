-- NexusLLM Migration 037 — Recover Runtimes Stuck in loading_model
--
-- Root cause: the health watcher was disabling model_endpoints (is_enabled=FALSE)
-- when health checks returned "connection refused" during model loading.
-- For large models (e.g. 120B GGUF) this takes several minutes, far exceeding
-- the 3-consecutive-failures circuit breaker threshold.
-- Once is_enabled=FALSE, the registry stopped loading the endpoint, the watcher
-- stopped probing it, and the model became permanently unreachable even after
-- the container became healthy.
--
-- This migration:
--   1. Re-enables model_endpoints that were disabled while their runtime is
--      in a loading or healthy state.
--   2. Resets agent_runtimes.state from 'unhealthy' back to 'loading_model'
--      for runtimes whose container is actually running and healthy (bind_port > 0,
--      container not explicitly stopped).
--   3. Syncs model_endpoints.port/host from the actual bind_port/bind_host
--      reported by the agent (critical when port=0 → OS-allocated port).
--
-- Safe to re-run (idempotent).

BEGIN;

-- ── Step 1: Re-enable endpoints whose agent_runtime is healthy or loading ─────
UPDATE model_endpoints me
SET is_enabled     = TRUE,
    lifecycle_state = 'active',
    health_status  = CASE
                         WHEN ar.state IN ('ready','active','warm','idle') THEN 'healthy'
                         ELSE 'unknown'
                     END,
    port           = CASE WHEN ar.bind_port > 0 THEN ar.bind_port ELSE me.port END,
    host           = CASE WHEN ar.bind_host IS NOT NULL AND ar.bind_host != ''
                          THEN ar.bind_host ELSE me.host END,
    updated_at     = NOW()
FROM agent_runtimes ar
WHERE ar.endpoint_id = me.id
  AND ar.bind_port > 0
  AND ar.state IN ('ready','active','warm','idle','loading_model','waiting_ready','unhealthy')
  AND me.is_enabled = FALSE;

-- ── Step 2: Reset 'unhealthy' runtimes that have a running container ──────────
-- 'unhealthy' was set by the watcher's circuit breaker firing during loading.
-- If the container is actually listening (bind_port > 0 and agent set it),
-- reset to 'loading_model' so the watcher can re-probe and promote to 'ready'.
-- error_msg is NOT NULL DEFAULT '' (migration 010) — clear it to '', not NULL.
UPDATE agent_runtimes
SET state      = 'loading_model',
    error_msg  = '',
    updated_at = NOW()
WHERE state     = 'unhealthy'
  AND bind_port > 0
  AND bind_host != ''
  AND updated_at < NOW() - INTERVAL '30 seconds'; -- don't touch freshly-set unhealthy rows

-- ── Step 3: Log what was recovered ───────────────────────────────────────────
DO $$
DECLARE
    ep_count  INT;
    rt_count  INT;
BEGIN
    SELECT COUNT(*) INTO ep_count
    FROM model_endpoints me
    JOIN agent_runtimes ar ON ar.endpoint_id = me.id
    WHERE me.is_enabled = TRUE
      AND ar.state IN ('loading_model','ready');

    SELECT COUNT(*) INTO rt_count
    FROM agent_runtimes
    WHERE state = 'loading_model' AND bind_port > 0;

    RAISE NOTICE 'Recovery 037: % endpoints re-enabled, % runtimes in loading_model with port',
        ep_count, rt_count;
END $$;

COMMIT;
