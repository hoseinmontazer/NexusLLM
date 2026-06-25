-- NexusLLM Migration 020 — Node Port Allocator
-- Introduces a per-node port lease table so multiple runtimes on the same
-- node get unique ports. Required for HA (N replicas × M nodes).
--
-- Port range: 8100–8999 (900 ports per node).
-- Key design: UNIQUE constraint is PARTIAL (WHERE released_at IS NULL) so
-- released ports can be reused. Uses pg_advisory_lock (session-level) inside
-- the allocation function so multiple calls within the same connection/transaction
-- serialize correctly and each call sees the ports inserted by previous calls.
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. NODE PORT LEASES
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS node_port_leases (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id      UUID        NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    port         INTEGER     NOT NULL,
    runtime_id   UUID        REFERENCES agent_runtimes(id) ON DELETE SET NULL,
    model_id     UUID        REFERENCES models(id) ON DELETE SET NULL,
    allocated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    released_at  TIMESTAMPTZ
);

-- Partial unique index: only ACTIVE (unreleased) leases must be unique.
-- This allows the same port to be reused after release.
DROP INDEX IF EXISTS node_port_leases_node_id_port_key;
CREATE UNIQUE INDEX IF NOT EXISTS idx_port_leases_active_unique
    ON node_port_leases(node_id, port)
    WHERE released_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_port_leases_node
    ON node_port_leases(node_id)
    WHERE released_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_port_leases_runtime
    ON node_port_leases(runtime_id)
    WHERE released_at IS NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. ALLOCATE PORT FUNCTION
-- Uses a session-level advisory lock (pg_advisory_lock, NOT xact) so that
-- multiple calls from the same connection see each other's inserts immediately,
-- even within the same transaction.
-- Returns 0 if the port range is exhausted.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION allocate_node_port(
    p_node_id  UUID,
    p_model_id UUID DEFAULT NULL
)
RETURNS INTEGER AS $$
DECLARE
    v_port     INTEGER;
    v_lock_key BIGINT;
BEGIN
    v_lock_key := abs(hashtext(p_node_id::text));

    -- Session-level lock: released explicitly after INSERT so subsequent
    -- calls in the same session/transaction see the new row.
    PERFORM pg_advisory_lock(v_lock_key);

    SELECT s.port INTO v_port
    FROM generate_series(8100, 8999) AS s(port)
    WHERE NOT EXISTS (
        SELECT 1 FROM node_port_leases l
        WHERE l.node_id = p_node_id
          AND l.port    = s.port
          AND l.released_at IS NULL
    )
    ORDER BY s.port
    LIMIT 1;

    IF v_port IS NOT NULL THEN
        INSERT INTO node_port_leases (node_id, port, model_id)
        VALUES (p_node_id, v_port, p_model_id);
    END IF;

    PERFORM pg_advisory_unlock(v_lock_key);
    RETURN COALESCE(v_port, 0);
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. RELEASE PORT FUNCTION
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION release_node_port(
    p_node_id UUID,
    p_port    INTEGER
)
RETURNS VOID AS $$
BEGIN
    UPDATE node_port_leases
    SET released_at = NOW()
    WHERE node_id    = p_node_id
      AND port       = p_port
      AND released_at IS NULL;
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. BACK-FILL LEASES FOR CURRENTLY ACTIVE RUNTIMES
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO node_port_leases (node_id, port, runtime_id, model_id)
SELECT ar.node_id, ar.bind_port, ar.id, ar.model_id
FROM agent_runtimes ar
WHERE ar.bind_port BETWEEN 8100 AND 8999
  AND ar.state NOT IN ('stopped','deleted','archived','unloaded','failed','lost')
ON CONFLICT DO NOTHING;

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. AUTO-RELEASE TRIGGER
-- Releases the port when a runtime enters a terminal state.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION trg_release_port_on_terminal()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.state IN ('stopped','deleted','archived','unloaded','failed')
       AND OLD.state NOT IN ('stopped','deleted','archived','unloaded','failed')
    THEN
        PERFORM release_node_port(NEW.node_id, NEW.bind_port);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS auto_release_port ON agent_runtimes;
CREATE TRIGGER auto_release_port
    AFTER UPDATE ON agent_runtimes
    FOR EACH ROW EXECUTE FUNCTION trg_release_port_on_terminal();

COMMIT;
