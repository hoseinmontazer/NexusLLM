-- NexusLLM — bind_host / model_endpoints.host repair
--
-- Forensic audit (Case File 003) found three code paths that could persist a
-- stale or wrong runtime endpoint address (activator.go's lazy cold-start,
-- cmd/admin/main.go's stuck-runtime sweeper, and internal/admin/handlers/
-- controller.go's shared Start/Restart/Upgrade/Rollback path). All three are
-- now fixed so this cannot happen going forward — this script repairs rows
-- that were written wrong BEFORE that fix.
--
-- This is deliberately NOT a numbered migration under migrations/ — it is a
-- one-time, manually-reviewed data repair, not schema DDL that should run
-- unconditionally on every `make migrate`. Run it by hand, once, after
-- reviewing the preview.
--
-- Topology-aware, not a blacklist: this does NOT assume "localhost" is
-- always wrong. It compares each row's stored host against its OWN node's
-- registered canonical address (nodes.ip_address, falling back to
-- nodes.hostname) and only touches rows that DISAGREE with that. A node that
-- genuinely registered itself with a loopback address (a real colocated
-- single-node deployment) is left completely untouched, because its stored
-- bind_host already matches its own canonical address.
--
-- Excluded by construction (never touched by either query below):
--   - Rows with no node_id at all (model_endpoints.node_id is nullable —
--     these are the external/provider-backed endpoints that intentionally
--     use a 0.0.0.0/admin-supplied host and route via upstream_base_url
--     instead of node resolution; agent_runtimes.node_id is NOT NULL so this
--     case cannot occur there).
--   - Rows whose stored host already matches their node's canonical address
--     (includes legitimate colocated-loopback deployments).
--
-- ── Step 1 — PREVIEW ─────────────────────────────────────────────────────
-- Run this first. Review every row before running Step 2. Nothing here
-- mutates data.

-- Preview: agent_runtimes violations (the column the registry actually
-- routes live traffic against for any active replica).
SELECT
    'agent_runtimes' AS table_name,
    ar.id             AS row_id,
    m.name            AS model_name,
    ar.node_id,
    ar.bind_host      AS current_host,
    COALESCE(host(n.ip_address), n.hostname) AS canonical_host,
    ar.state
FROM agent_runtimes ar
JOIN nodes n ON n.id = ar.node_id
LEFT JOIN models m ON m.id = ar.model_id
WHERE ar.bind_host IS DISTINCT FROM COALESCE(host(n.ip_address), n.hostname)
  AND ar.state IN ('ready','active','warm','idle','loading_model',
                    'waiting_ready','starting','validating','downloading',
                    'loading','pending')
ORDER BY ar.state, model_name;

-- Preview: model_endpoints violations (display/admin-panel and the
-- bootstrap fallback used before a runtime's port is confirmed).
SELECT
    'model_endpoints' AS table_name,
    me.id             AS row_id,
    m.name            AS model_name,
    me.node_id,
    me.host           AS current_host,
    COALESCE(host(n.ip_address), n.hostname) AS canonical_host
FROM model_endpoints me
JOIN nodes n ON n.id = me.node_id
LEFT JOIN models m ON m.id = me.model_id
WHERE me.node_id IS NOT NULL
  AND me.host IS DISTINCT FROM COALESCE(host(n.ip_address), n.hostname)
ORDER BY model_name;

-- ── Step 2 — REPAIR ──────────────────────────────────────────────────────
-- Only run after reviewing Step 1's output. Safe to run more than once —
-- each UPDATE's WHERE clause only ever matches rows that still disagree with
-- their node's current canonical address, so a second run is always a no-op
-- once converged.

BEGIN;

UPDATE agent_runtimes ar
SET bind_host = canonical.addr
FROM (
    SELECT n.id AS node_id, COALESCE(host(n.ip_address), n.hostname) AS addr
    FROM nodes n
) canonical
WHERE ar.node_id = canonical.node_id
  AND ar.bind_host IS DISTINCT FROM canonical.addr
  AND ar.state IN ('ready','active','warm','idle','loading_model',
                    'waiting_ready','starting','validating','downloading',
                    'loading','pending');

UPDATE model_endpoints me
SET host = canonical.addr
FROM (
    SELECT n.id AS node_id, COALESCE(host(n.ip_address), n.hostname) AS addr
    FROM nodes n
) canonical
WHERE me.node_id = canonical.node_id
  AND me.host IS DISTINCT FROM canonical.addr;

COMMIT;
