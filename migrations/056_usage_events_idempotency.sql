-- NexusLLM Migration 056 — usage_events idempotency
--
-- Forensic audit (Case File 002) found usage_events had no idempotency key at
-- all beyond its (id, created_at) primary key, where `id` is minted fresh by
-- the producer on every Record() call. That leaves nothing to stop a client-
-- side retry — or any future producer bug — from inserting two rows for what
-- is logically the same request_id.
--
-- Partial (not full) unique index: request_id is not always populated for
-- older/legacy event producers, and Postgres already treats NULL as distinct
-- from NULL in a unique index, but an empty string is not — hence the extra
-- filter excluding ''.
--
-- If this fails to apply because duplicate non-empty request_ids already
-- exist in the table, that is a real data-quality signal worth investigating
-- before forcing the constraint through, not something to silently work
-- around.
--
-- usage_events is PARTITION BY RANGE (created_at) (migration 005) — Postgres
-- requires every unique index on a partitioned table to include all
-- partitioning columns, so created_at must be part of this index. This means
-- the guarantee is "one row per request_id per created_at value", not a pure
-- cross-partition uniqueness on request_id alone: a retry whose two inserts
-- land with different created_at values (e.g. straddling a year boundary,
-- the current partition granularity) would not be caught. Real client
-- retries are expected within milliseconds of each other, so this is not a
-- practical gap today, but it is a real one — do not assume this closes
-- every duplicate-insert scenario.

BEGIN;

CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_events_request_id
    ON usage_events(request_id, created_at)
    WHERE request_id IS NOT NULL AND request_id != '';

COMMIT;
