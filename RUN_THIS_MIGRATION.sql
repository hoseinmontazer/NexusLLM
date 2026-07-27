-- Run this to add the upstream_model_name column
-- Execute with: psql -U nexus -d nexusllm -f RUN_THIS_MIGRATION.sql
-- Or via docker: docker compose exec -T postgres psql -U nexus -d nexusllm < RUN_THIS_MIGRATION.sql

ALTER TABLE model_endpoints 
ADD COLUMN IF NOT EXISTS upstream_model_name VARCHAR(512) NOT NULL DEFAULT '';

-- Verify it worked
\d model_endpoints
