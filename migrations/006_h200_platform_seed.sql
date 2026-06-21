-- NexusLLM Migration 006 — H200 Platform Seed
-- Seeds the default single-server node for a 2x H200 NVL deployment.
-- Safe to re-run (idempotent).
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- Default single-server node
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO nodes (
    hostname, display_name,
    total_cpu, total_ram_mb, total_vram_mb,
    status, labels
)
SELECT
    'nexus-h200-01', 'Primary AI Server',
    384,              -- 384 vCPUs
    1048576,          -- 1 TB RAM = 1,048,576 MB
    294912,           -- 2x H200 NVL 144GB each = 288GB = 294,912 MB
    'online',
    '{"tier":"primary","gpu":"h200","generation":"hopper","form_factor":"single_server"}'::jsonb
WHERE NOT EXISTS (SELECT 1 FROM nodes WHERE hostname = 'nexus-h200-01');

-- ─────────────────────────────────────────────────────────────────────────────
-- Register the GPU node under the cluster node
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO gpu_nodes (name, host, driver_type, total_vram_mb, is_available, node_id, created_at, updated_at)
SELECT 'h200-node-0', 'localhost', 'docker',
       294912, TRUE,
       (SELECT id FROM nodes WHERE hostname = 'nexus-h200-01'),
       NOW(), NOW()
WHERE NOT EXISTS (SELECT 1 FROM gpu_nodes WHERE name = 'h200-node-0');

-- ─────────────────────────────────────────────────────────────────────────────
-- Register both H200 NVL GPUs
-- H200 NVL 144GB each = 147,456 MB per GPU
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO gpu_devices (
    node_id, device_index, name, vram_mb,
    status, utilization_pct, temperature_c, power_draw_w,
    numa_node, compute_cap,
    last_seen_at, created_at, updated_at
)
SELECT
    gn.id, 0, 'NVIDIA H200 NVL', 147456,
    'available', 0, 30, 0,
    0, '9.0',
    NOW(), NOW(), NOW()
FROM gpu_nodes gn WHERE gn.name = 'h200-node-0'
  AND NOT EXISTS (
      SELECT 1 FROM gpu_devices d2 WHERE d2.node_id = gn.id AND d2.device_index = 0
  );

INSERT INTO gpu_devices (
    node_id, device_index, name, vram_mb,
    status, utilization_pct, temperature_c, power_draw_w,
    numa_node, compute_cap,
    last_seen_at, created_at, updated_at
)
SELECT
    gn.id, 1, 'NVIDIA H200 NVL', 147456,
    'available', 0, 30, 0,
    1, '9.0',
    NOW(), NOW(), NOW()
FROM gpu_nodes gn WHERE gn.name = 'h200-node-0'
  AND NOT EXISTS (
      SELECT 1 FROM gpu_devices d2 WHERE d2.node_id = gn.id AND d2.device_index = 1
  );

COMMIT;
