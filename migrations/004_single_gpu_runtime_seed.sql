-- NexusLLM — Single GPU server runtime seed
-- Registers the three vLLM instances running on one physical GPU server.
--
-- Service names resolve via Docker Compose DNS:
--   vllm-gemma → port 8000
--   vllm-llama → port 8000
--   vllm-qwen  → port 8000
--
-- Run after: 001, 002, 003

BEGIN;

-- ── Register models in the new schema ────────────────────────────────────────
INSERT INTO models (id, name, display_name, provider, backend_type, max_context, max_output, enabled, tags)
VALUES
  ('m0000000-0000-0000-0000-000000000001', 'gemma-3-27b',    'Gemma 3 27B',     'google',  'vllm', 32768, 8192, TRUE, '["chat","instruct"]'),
  ('m0000000-0000-0000-0000-000000000002', 'llama-3.3-70b',  'LLaMA 3.3 70B',   'meta',    'vllm', 65536, 8192, TRUE, '["chat","instruct","tools"]'),
  ('m0000000-0000-0000-0000-000000000003', 'qwen-3-32b',     'Qwen 3 32B',      'alibaba', 'vllm', 32768, 8192, TRUE, '["chat","instruct"]')
ON CONFLICT (name) DO NOTHING;

-- ── Default versions ──────────────────────────────────────────────────────────
INSERT INTO model_versions (id, model_id, version, is_default)
VALUES
  (gen_random_uuid(), 'm0000000-0000-0000-0000-000000000001', 'v1', TRUE),
  (gen_random_uuid(), 'm0000000-0000-0000-0000-000000000002', 'v1', TRUE),
  (gen_random_uuid(), 'm0000000-0000-0000-0000-000000000003', 'v1', TRUE)
ON CONFLICT DO NOTHING;

-- ── Endpoint pool — one endpoint per model (single GPU server) ───────────────
-- host = Docker Compose service name (resolved by bridge DNS)
-- port = container-internal port (8000 for all vLLM instances)
INSERT INTO model_endpoints (id, model_id, host, port, base_path, weight, priority, health_status, is_enabled)
VALUES
  (gen_random_uuid(), 'm0000000-0000-0000-0000-000000000001', 'vllm-gemma', 8000, '/v1', 100, 1, 'unknown', TRUE),
  (gen_random_uuid(), 'm0000000-0000-0000-0000-000000000002', 'vllm-llama', 8000, '/v1', 100, 1, 'unknown', TRUE),
  (gen_random_uuid(), 'm0000000-0000-0000-0000-000000000003', 'vllm-qwen',  8000, '/v1', 100, 1, 'unknown', TRUE);

-- ── Runtime configs ───────────────────────────────────────────────────────────
INSERT INTO model_runtime_configs (id, model_id, gpu_memory_util, tensor_parallel, dtype)
VALUES
  (gen_random_uuid(), 'm0000000-0000-0000-0000-000000000001', 0.90, 2, 'bfloat16'),
  (gen_random_uuid(), 'm0000000-0000-0000-0000-000000000002', 0.90, 4, 'bfloat16'),
  (gen_random_uuid(), 'm0000000-0000-0000-0000-000000000003', 0.90, 2, 'bfloat16')
ON CONFLICT DO NOTHING;

COMMIT;
