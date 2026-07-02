-- NexusLLM Migration 028 — Atomic Replica Slot Guard
--
-- Problem: multiple concurrent callers (reconciler, activator, admin handler,
-- idle manager) each perform a read-then-insert pattern to create a new
-- agent_runtimes row.  Between the count-read and the INSERT there is a window
-- where another concurrent process can also decide "there is capacity" and
-- insert a second row, exceeding desired_replicas.
--
-- Fix: a single PostgreSQL function that acquires a per-model advisory lock,
-- counts non-terminal rows, and either inserts the new row or rejects the
-- request — atomically, with no TOCTOU gap.
--
-- All statements are idempotent.
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- claim_replica_slot(model_id, desired_replicas)
--
-- Returns TRUE  → slot was available; the caller MUST proceed with INSERT.
-- Returns FALSE → no slot available; caller must abort and not insert.
--
-- Uses pg_advisory_xact_lock(hashtext(model_id::text)) so only one transaction
-- at a time can claim a slot for a given model.  The lock is released
-- automatically when the calling transaction commits or rolls back.
--
-- "Non-terminal" = any state except the terminal set:
--   stopped, deleted, archived, unloaded, lost
--
-- This matches exactly the definition used by reconciler plan() so both callers
-- see the same universe of "live" rows.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION claim_replica_slot(
    p_model_id       UUID,
    p_desired        INTEGER
) RETURNS BOOLEAN
LANGUAGE plpgsql AS $$
DECLARE
    v_non_terminal INTEGER;
BEGIN
    -- Acquire an exclusive advisory lock scoped to this model's UUID.
    -- hashtext() maps the UUID string to a 32-bit integer that fits the
    -- single-argument advisory lock API.
    PERFORM pg_advisory_xact_lock(hashtext(p_model_id::text));

    SELECT COUNT(*) INTO v_non_terminal
    FROM agent_runtimes
    WHERE model_id = p_model_id
      AND state NOT IN ('stopped','deleted','archived','unloaded','lost');

    RETURN v_non_terminal < p_desired;
END;
$$;

COMMENT ON FUNCTION claim_replica_slot(UUID, INTEGER) IS
'Atomically checks whether a new replica slot is available for p_model_id.
Returns TRUE (slot available) or FALSE (already at capacity).
Uses a per-model advisory lock to prevent concurrent over-provisioning.
Caller MUST call this inside a transaction and proceed with the INSERT only
when TRUE is returned; commit atomically releases the lock.';

-- ─────────────────────────────────────────────────────────────────────────────
-- desired_replicas(model_id) helper
--
-- Returns the configured desired_replicas for a model, defaulting to 1.
-- Used by the Go guard wrappers to avoid a separate query round-trip.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION desired_replicas(p_model_id UUID)
RETURNS INTEGER
LANGUAGE sql STABLE AS $$
    SELECT COALESCE(
        (SELECT desired_replicas FROM model_replica_specs WHERE model_id = p_model_id),
        1
    );
$$;

COMMENT ON FUNCTION desired_replicas(UUID) IS
'Returns the desired replica count for a model from model_replica_specs, defaulting to 1.';

COMMIT;
