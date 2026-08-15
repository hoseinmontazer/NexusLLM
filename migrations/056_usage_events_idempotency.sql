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

BEGIN;

CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_events_request_id
    ON usage_events(request_id)
    WHERE request_id IS NOT NULL AND request_id != '';

COMMIT;
