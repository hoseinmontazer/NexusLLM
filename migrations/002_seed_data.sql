-- NexusLLM Migration 002 — intentionally empty.
-- No seed data. All organizations, teams, API keys, and models are created
-- through the Admin API or Admin UI at runtime:
--
--   POST /admin/v1/orgs          → create an organization
--   POST /admin/v1/teams         → create a team under an org
--   POST /admin/v1/teams/:id/api-keys → generate API keys
--   POST /admin/v1/models/deploy → register and start a model
--
-- See docs/ for full examples.
BEGIN;
COMMIT;
