-- Migration 032: Fix claim_replica_slot to treat 'failed' as terminal.
--
-- Problem: when a container is manually removed (docker rm) or crashes,
-- the containerWatchLoop marks its agent_runtimes row as 'failed'.
-- The claim_replica_slot() function did not exclude 'failed' rows from its
-- non-terminal count, so v_non_terminal >= desired was true even though no
-- container was actually running.  This permanently blocked EnsureRunning
-- from enqueueing a new START_MODEL — the cold-start path returned
-- ErrAtCapacity and waitForReady polled a failed row forever, never recovering.
--
-- Fix: add 'failed' and 'unhealthy' to the exclusion list so these states are
-- treated as terminal for slot-guard purposes.
--
-- 'unhealthy' is included because rolling replacement has already been
-- triggered for unhealthy replicas; they should not consume a slot that would
-- block a clean cold-start on an unrelated trigger.

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

    -- Count live (non-terminal) rows.
    -- Excluded states are either already gone or on their way out and must not
    -- block new container starts:
    --   stopped/deleted/archived/unloaded/lost — definitively terminal
    --   draining  — being torn down after rolling replacement
    --   failed    — container exited unexpectedly or was manually removed;
    --               a new start must be allowed immediately
    --   unhealthy — rolling replacement already in progress for this replica
    SELECT COUNT(*) INTO v_non_terminal
    FROM agent_runtimes
    WHERE model_id = p_model_id
      AND state NOT IN (
          'stopped','deleted','archived','unloaded','lost',
          'draining','failed','unhealthy'
      );

    -- Allow up to desired + max_surge rows during rolling replacement.
    v_limit := p_desired + COALESCE(p_max_surge, 1);

    RETURN v_non_terminal < v_limit;
END;
$$;

COMMENT ON FUNCTION claim_replica_slot(UUID, INTEGER, INTEGER) IS
'Atomically checks whether a new replica slot is available.
During rolling replacement the limit is desired + max_surge.
Terminal states (stopped, deleted, failed, unhealthy, draining, etc.) are
excluded from the count so they never block new container starts.';
