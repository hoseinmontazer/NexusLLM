-- Migration 060: bounded recovery chain support
--
-- Production forensic audit (runtime churn + reconciliation follow-up) found
-- that recovery_attempt was being tracked per-ROW instead of per-LOGICAL-
-- REPLICA: every replacement row was inserted with a hardcoded
-- recovery_attempt=1 and the pre-existing `recovered_from` column was never
-- populated, so nothing could walk the replacement chain to compute a
-- cumulative attempt count. Combined with claim_replica_slot() correctly
-- excluding 'unhealthy'/'failed' rows from its capacity count (by design, so
-- an in-progress replacement never blocks itself), this meant a persistently
-- failing replica could be replaced indefinitely — production data showed
-- one row used as the recovery-chain root across roughly 639 replacement
-- attempts over ~35 hours, and 360 replacement rows/hour sustained for 8
-- consecutive hours for a single model.
--
-- This migration adds one nullable column needed to make the recovery
-- cooldown durable and independent of `updated_at` (which gets touched by
-- routine bookkeeping — e.g. linking/clearing `replaced_by` — on every
-- reconciler tick, silently resetting any cooldown computed from it).
-- `recovered_from` and `replaced_by` already exist (migration 029) and are
-- reused as the logical-replica chain identity — no new abstraction/table is
-- introduced.

BEGIN;

ALTER TABLE agent_runtimes
    ADD COLUMN IF NOT EXISTS next_retry_at TIMESTAMPTZ NULL;

COMMENT ON COLUMN agent_runtimes.next_retry_at IS
    'Earliest time this row (while unhealthy, awaiting a bounded-recovery replacement) may have another replacement attempt spawned for it. Set explicitly by internal/ha.Reconciler when a replacement fails and the row becomes eligible again; deliberately independent of updated_at, which changes on every reconciler tick for unrelated bookkeeping and must not silently reset the cooldown.';

-- Deliberately no index, no NOT NULL, no default: this column starts NULL for
-- every existing row (meaning "eligible immediately" for the first check),
-- matching the pre-migration behavior of a first attempt not waiting on a
-- persisted cooldown.

COMMIT;
