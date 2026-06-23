-- NexusLLM Migration 006 — intentionally empty.
-- The original H200 seed data was removed.
-- Nodes, GPU nodes, and GPU devices are now registered dynamically:
--   • Nodes register via POST /agent/v1/register (node agent) or
--     POST /admin/v1/nodes (admin API).
--   • GPU devices are auto-populated by the node agent on first heartbeat
--     using nvidia-smi output (see internal/nodeagent/agent.go syncGPUDevices).
-- No hardcoded hardware assumptions remain in the schema.
BEGIN;
COMMIT;
