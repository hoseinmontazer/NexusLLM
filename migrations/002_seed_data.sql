-- NexusLLM dev seed data — fully idempotent, no hardcoded UUIDs after org creation
BEGIN;

-- Ensure the Acme Corp org exists (insert only if no org with this slug)
INSERT INTO organizations (name, slug)
SELECT 'Acme Corp', 'acme-corp'
WHERE NOT EXISTS (SELECT 1 FROM organizations WHERE slug = 'acme-corp');

-- Ensure teams exist under acme-corp
INSERT INTO teams (org_id, name, slug, priority)
SELECT o.id, 'Team Alpha', 'team-alpha', 80
FROM organizations o WHERE o.slug = 'acme-corp'
  AND NOT EXISTS (SELECT 1 FROM teams WHERE slug = 'team-alpha' AND org_id = o.id);

INSERT INTO teams (org_id, name, slug, priority)
SELECT o.id, 'Team Beta', 'team-beta', 50
FROM organizations o WHERE o.slug = 'acme-corp'
  AND NOT EXISTS (SELECT 1 FROM teams WHERE slug = 'team-beta' AND org_id = o.id);

INSERT INTO teams (org_id, name, slug, priority)
SELECT o.id, 'Team Gamma', 'team-gamma', 20
FROM organizations o WHERE o.slug = 'acme-corp'
  AND NOT EXISTS (SELECT 1 FROM teams WHERE slug = 'team-gamma' AND org_id = o.id);

-- Base model rows
INSERT INTO models (name, display_name, vllm_endpoint, max_tokens) VALUES
  ('gemma-3-27b',   'Gemma 3 27B',   'http://vllm-gemma:8000', 8192),
  ('llama-3.3-70b', 'LLaMA 3.3 70B', 'http://vllm-llama:8000', 8192),
  ('qwen-3-32b',    'Qwen 3 32B',    'http://vllm-qwen:8000',  8192)
ON CONFLICT (name) DO NOTHING;

-- Policies for each team (one row per team)
INSERT INTO policies (team_id, rpm, tpd, max_concurrent, max_context_tokens)
SELECT t.id, 1000, 50000000, 20, 32768 FROM teams t WHERE t.slug = 'team-alpha'
  AND NOT EXISTS (SELECT 1 FROM policies WHERE team_id = t.id);

INSERT INTO policies (team_id, rpm, tpd, max_concurrent, max_context_tokens)
SELECT t.id, 500, 10000000, 10, 8192 FROM teams t WHERE t.slug = 'team-beta'
  AND NOT EXISTS (SELECT 1 FROM policies WHERE team_id = t.id);

INSERT INTO policies (team_id, rpm, tpd, max_concurrent, max_context_tokens)
SELECT t.id, 100, 2000000, 5, 4096 FROM teams t WHERE t.slug = 'team-gamma'
  AND NOT EXISTS (SELECT 1 FROM policies WHERE team_id = t.id);

-- Model permissions
INSERT INTO team_model_permissions (team_id, model_id)
SELECT t.id, m.id FROM teams t, models m
WHERE t.slug = 'team-alpha' AND m.name IN ('gemma-3-27b','llama-3.3-70b')
ON CONFLICT DO NOTHING;

INSERT INTO team_model_permissions (team_id, model_id)
SELECT t.id, m.id FROM teams t, models m
WHERE t.slug = 'team-beta' AND m.name = 'gemma-3-27b'
ON CONFLICT DO NOTHING;

INSERT INTO team_model_permissions (team_id, model_id)
SELECT t.id, m.id FROM teams t, models m
WHERE t.slug = 'team-gamma' AND m.name = 'qwen-3-32b'
ON CONFLICT DO NOTHING;

-- Service accounts
INSERT INTO service_accounts (team_id, name)
SELECT t.id, 'team-alpha-sa' FROM teams t WHERE t.slug = 'team-alpha'
  AND NOT EXISTS (SELECT 1 FROM service_accounts WHERE name = 'team-alpha-sa' AND team_id = t.id);

INSERT INTO service_accounts (team_id, name)
SELECT t.id, 'team-beta-sa' FROM teams t WHERE t.slug = 'team-beta'
  AND NOT EXISTS (SELECT 1 FROM service_accounts WHERE name = 'team-beta-sa' AND team_id = t.id);

INSERT INTO service_accounts (team_id, name)
SELECT t.id, 'team-gamma-sa' FROM teams t WHERE t.slug = 'team-gamma'
  AND NOT EXISTS (SELECT 1 FROM service_accounts WHERE name = 'team-gamma-sa' AND team_id = t.id);

-- Dev API keys (placeholder hashes — use: make generate-key for real keys)
INSERT INTO api_keys (team_id, name, key_hash, key_prefix)
SELECT t.id, 'dev-key-alpha',
  'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'nxs_alpha_'
FROM teams t WHERE t.slug = 'team-alpha'
  AND NOT EXISTS (SELECT 1 FROM api_keys WHERE key_hash = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa');

INSERT INTO api_keys (team_id, name, key_hash, key_prefix)
SELECT t.id, 'dev-key-beta',
  'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 'nxs_beta__'
FROM teams t WHERE t.slug = 'team-beta'
  AND NOT EXISTS (SELECT 1 FROM api_keys WHERE key_hash = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb');

INSERT INTO api_keys (team_id, name, key_hash, key_prefix)
SELECT t.id, 'dev-key-gamma',
  'cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc', 'nxs_gamma_'
FROM teams t WHERE t.slug = 'team-gamma'
  AND NOT EXISTS (SELECT 1 FROM api_keys WHERE key_hash = 'cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc');

COMMIT;
