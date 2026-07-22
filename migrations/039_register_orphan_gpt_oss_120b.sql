-- NexusLLM Migration 039 — Register orphaned gpt-oss-120b container
--
-- The model row exists but has no model_endpoints or agent_runtimes row.
-- The container (nexus-gpt-oss-120b) is running on aigpu-server:41635.
-- This migration registers it so the gateway can route requests to it.
--
-- After running this, the gateway's 10-second periodic reload will pick up
-- the endpoint and start routing within ~10 seconds.
--
-- Idempotent: uses INSERT ... ON CONFLICT DO NOTHING.

BEGIN;

-- ── 1. Insert model_endpoint row ─────────────────────────────────────────────
INSERT INTO model_endpoints
  (id, model_id, host, port, base_path,
   weight, priority, health_status, is_enabled, lifecycle_state)
SELECT
    gen_random_uuid(),
    m.id,
    -- IP of aigpu-server — resolve from nodes table
    COALESCE(
        host((SELECT ip_address FROM nodes WHERE hostname = 'aigpu-server' LIMIT 1)),
        (SELECT hostname FROM nodes WHERE hostname = 'aigpu-server' LIMIT 1),
        'localhost'
    ),
    41635,
    '/v1',
    100, 1,
    'healthy',
    TRUE,
    'active'
FROM models m
WHERE m.name = 'gpt-oss-120b'
  AND NOT EXISTS (
      SELECT 1 FROM model_endpoints WHERE model_id = m.id
  );

-- ── 2. Insert agent_runtimes row ──────────────────────────────────────────────
INSERT INTO agent_runtimes
  (id, node_id, endpoint_id, model_id, runtime_name, backend,
   state, gpu_ids, bind_host, bind_port,
   cpu_affinity, numa_node,
   requested_mode, effective_mode, workload_policy,
   last_used_at, updated_at)
SELECT
    gen_random_uuid(),
    (SELECT id FROM nodes WHERE hostname = 'aigpu-server' LIMIT 1),
    me.id,
    m.id,
    'nexus-gpt-oss-120b',
    'llamacpp',
    'ready',
    '[]'::jsonb,
    COALESCE(
        host((SELECT ip_address FROM nodes WHERE hostname = 'aigpu-server' LIMIT 1)),
        (SELECT hostname FROM nodes WHERE hostname = 'aigpu-server' LIMIT 1),
        'localhost'
    ),
    41635,
    '', -1,
    'gpu', 'gpu', 'lazy_load',
    NOW(), NOW()
FROM models m
JOIN model_endpoints me ON me.model_id = m.id
WHERE m.name = 'gpt-oss-120b'
  AND NOT EXISTS (
      SELECT 1 FROM agent_runtimes ar
      WHERE ar.model_id = m.id
        AND ar.state = 'ready'
  );

-- ── 3. Verify ─────────────────────────────────────────────────────────────────
DO $$
DECLARE
    ep_count INT;
    rt_count INT;
BEGIN
    SELECT COUNT(*) INTO ep_count
    FROM model_endpoints me JOIN models m ON m.id = me.model_id
    WHERE m.name = 'gpt-oss-120b';

    SELECT COUNT(*) INTO rt_count
    FROM agent_runtimes ar JOIN models m ON m.id = ar.model_id
    WHERE m.name = 'gpt-oss-120b' AND ar.state = 'ready';

    RAISE NOTICE 'gpt-oss-120b: % endpoint(s), % ready runtime(s)', ep_count, rt_count;
END $$;

COMMIT;
