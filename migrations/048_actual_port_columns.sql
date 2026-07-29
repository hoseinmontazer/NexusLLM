-- NexusLLM Migration 048 — Runtime Port Auditability Columns
--
-- Goal: make the distinction between the requested (pre-start) port and the
--       actual (post-inspect) port explicit and queryable in the DB.
--
-- Background:
--   After the FIX-8 audit finding, agent_runtimes.bind_port is the operational
--   routing column and is always overwritten with the real container port once
--   discoverPortForBackend() confirms it.  But before this migration there was
--   no way to tell from the DB whether bind_port == 8200 because the container
--   was requested on 8200 *and* bound there, or because discovery had not yet
--   run and the column still holds the pre-start allocation.
--
--   Two new columns solve this:
--
--     requested_port  — the port sent to the agent at task dispatch time.
--                       Set once, never overwritten by agent reports.
--                       Populated here from the current bind_port value
--                       (best-effort for existing rows).
--
--     actual_port     — the port confirmed by docker inspect after container
--                       start, written by CompleteTask / UpdateRuntime handlers.
--                       Equals bind_port once confirmed; 0 until first report.
--
--   bind_port remains the canonical operational column used by loadEndpoints().
--   requested_port and actual_port are audit / observability columns only.
--
-- All statements are idempotent (safe to re-run).

BEGIN;

-- ── New columns ────────────────────────────────────────────────────────────

ALTER TABLE agent_runtimes
    ADD COLUMN IF NOT EXISTS requested_port INTEGER NOT NULL DEFAULT 0;

ALTER TABLE agent_runtimes
    ADD COLUMN IF NOT EXISTS actual_port INTEGER NOT NULL DEFAULT 0;

-- ── Back-fill existing rows ────────────────────────────────────────────────
-- For rows already in the DB we have only bind_port, which may be either the
-- requested value or the actual value depending on when it was last written.
-- Treat the current bind_port as both for existing rows — it is the best
-- available approximation and allows the columns to be non-null immediately.

UPDATE agent_runtimes
SET    requested_port = bind_port,
       actual_port    = bind_port
WHERE  requested_port = 0
  AND  bind_port     > 0;

-- ── Comments ───────────────────────────────────────────────────────────────

COMMENT ON COLUMN agent_runtimes.requested_port IS
    'Port sent to the node agent at task dispatch time (pre-start allocation). '
    'Set once by enqueueStartModel(); never overwritten by agent reports. '
    'Use this to audit whether the container honored the requested port.';

COMMENT ON COLUMN agent_runtimes.actual_port IS
    'Port confirmed by docker inspect after container start. '
    'Written by CompleteTask and UpdateRuntime handlers once discoverPortForBackend() '
    'returns a result. Equals 0 until the agent reports back. '
    'Equals bind_port once confirmed. bind_port is the operational routing column.';

COMMENT ON COLUMN agent_runtimes.bind_port IS
    'Operational routing port. Always equals actual_port once confirmed by the agent. '
    'This is the column read by registry.loadEndpoints() for all routing decisions. '
    'See also: requested_port (pre-start), actual_port (post-inspect audit trail).';

-- ── Index for mismatch queries ─────────────────────────────────────────────
-- Used by portMismatchSweep() and operator diagnostics to quickly find rows
-- where the agent bound to a different port than requested.

CREATE INDEX IF NOT EXISTS idx_agent_runtimes_port_mismatch
    ON agent_runtimes (model_id, requested_port, actual_port)
    WHERE actual_port > 0
      AND requested_port != actual_port;

COMMIT;
