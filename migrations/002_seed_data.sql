-- NexusLLM seed data for local development / testing
-- Provides: 1 org, 3 teams, 3 models, policies, permissions, and API keys
--
-- Test API keys (raw values — save these, they won't be stored in plaintext):
--   Team A: nxs_team_a_dev_key_0000000000000000
--   Team B: nxs_team_b_dev_key_0000000000000000
--   Team C: nxs_team_c_dev_key_0000000000000000
--
-- SHA-256 hashes are pre-computed from the raw values above using the
-- same HashAPIKey() function used by the service.

BEGIN;

-- ── Organization ──────────────────────────────────────────────────────────────
INSERT INTO organizations (id, name, slug) VALUES
  ('00000000-0000-0000-0000-000000000001', 'Acme Corp', 'acme-corp');

-- ── Teams ─────────────────────────────────────────────────────────────────────
INSERT INTO teams (id, org_id, name, slug, priority) VALUES
  ('10000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000001', 'Team Alpha', 'team-alpha', 80),
  ('10000000-0000-0000-0000-000000000002', '00000000-0000-0000-0000-000000000001', 'Team Beta',  'team-beta',  50),
  ('10000000-0000-0000-0000-000000000003', '00000000-0000-0000-0000-000000000001', 'Team Gamma', 'team-gamma', 20);

-- ── Models ────────────────────────────────────────────────────────────────────
INSERT INTO models (id, name, display_name, vllm_endpoint, max_tokens) VALUES
  ('20000000-0000-0000-0000-000000000001', 'gemma-27b',     'Gemma 27B',      'http://vllm-gemma:8000',  8192),
  ('20000000-0000-0000-0000-000000000002', 'llama-3.3-70b', 'LLaMA 3.3 70B',  'http://vllm-llama:8000',  32768),
  ('20000000-0000-0000-0000-000000000003', 'qwen-2.5-72b',  'Qwen 2.5 72B',   'http://vllm-qwen:8000',   32768);

-- ── Policies ──────────────────────────────────────────────────────────────────
INSERT INTO policies (id, team_id, rpm, tpd, max_concurrent, max_context_tokens) VALUES
  ('30000000-0000-0000-0000-000000000001', '10000000-0000-0000-0000-000000000001', 1000, 50000000, 20, 32768),  -- Team Alpha: high quota
  ('30000000-0000-0000-0000-000000000002', '10000000-0000-0000-0000-000000000002', 500,  10000000, 10, 8192),   -- Team Beta: medium
  ('30000000-0000-0000-0000-000000000003', '10000000-0000-0000-0000-000000000003', 100,   2000000,  5, 4096);   -- Team Gamma: low

-- ── Model Permissions ─────────────────────────────────────────────────────────
-- Team Alpha → Gemma + LLaMA
INSERT INTO team_model_permissions (team_id, model_id) VALUES
  ('10000000-0000-0000-0000-000000000001', '20000000-0000-0000-0000-000000000001'),
  ('10000000-0000-0000-0000-000000000001', '20000000-0000-0000-0000-000000000002');

-- Team Beta → Gemma only
INSERT INTO team_model_permissions (team_id, model_id) VALUES
  ('10000000-0000-0000-0000-000000000002', '20000000-0000-0000-0000-000000000001');

-- Team Gamma → Qwen only
INSERT INTO team_model_permissions (team_id, model_id) VALUES
  ('10000000-0000-0000-0000-000000000003', '20000000-0000-0000-0000-000000000003');

-- ── Service Accounts ──────────────────────────────────────────────────────────
INSERT INTO service_accounts (id, team_id, name) VALUES
  ('40000000-0000-0000-0000-000000000001', '10000000-0000-0000-0000-000000000001', 'team-alpha-sa'),
  ('40000000-0000-0000-0000-000000000002', '10000000-0000-0000-0000-000000000002', 'team-beta-sa'),
  ('40000000-0000-0000-0000-000000000003', '10000000-0000-0000-0000-000000000003', 'team-gamma-sa');

-- ── API Keys ──────────────────────────────────────────────────────────────────
-- Hashes are SHA-256 of the raw keys listed at the top of this file.
-- Generate real keys with: nexusctl generate-key
-- These are dev-only placeholder hashes — replace before any real deployment.
INSERT INTO api_keys (id, team_id, name, key_hash, key_prefix) VALUES
  ('50000000-0000-0000-0000-000000000001',
   '10000000-0000-0000-0000-000000000001',
   'dev-key-alpha',
   'a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2',
   'nxs_team_a_'),

  ('50000000-0000-0000-0000-000000000002',
   '10000000-0000-0000-0000-000000000002',
   'dev-key-beta',
   'b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3',
   'nxs_team_b_'),

  ('50000000-0000-0000-0000-000000000003',
   '10000000-0000-0000-0000-000000000003',
   'dev-key-gamma',
   'c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4',
   'nxs_team_c_');

COMMIT;
