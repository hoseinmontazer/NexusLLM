-- NexusLLM Migration 058 — Project-level model authorization for Public
-- (regular/managed) models.
--
-- Forensic audit confirmed this was never built: internal/policy/engine.go's
-- Evaluate() only ever checks team_model_permissions (via the
-- nexus:team:<id>:models Redis set) for Public models. Project-scoped ACL
-- infrastructure (project_provider_access, nexus:project:<id>:vproviders)
-- exists but is explicitly scoped to virtual/catalog models only — see that
-- table's own migration 050 comment. A project-scoped API token therefore
-- currently has exactly the same Public-model access as a plain team token
-- for the same team; project_id has zero effect on regular-model
-- authorization. This migration adds the missing table.
--
-- Semantics (Option A — project narrows team, never widens it):
--   effective_access = team_model_access AND project_model_access
-- enforced in Go (internal/policy/engine.go), not here.
--
-- Backward compatibility: this table starts empty for every existing
-- project. The Go-side enforcement treats "this project has never been
-- explicitly configured" (no grants ever made) as full passthrough to the
-- team's access, so every existing project-scoped token keeps working
-- exactly as before until an admin explicitly grants (or revokes) at least
-- one model for that project — at which point that project moves into
-- restricted (Option A) mode. See internal/policy/engine.go's
-- projectModelsConfiguredKey.
BEGIN;

CREATE TABLE IF NOT EXISTS project_model_permissions (
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- ON DELETE RESTRICT (not CASCADE) mirrors migration 053's fix for
    -- team_model_permissions: a model soft-delete must not silently wipe out
    -- project grants that a later redeploy under the same name should
    -- restore (see model_permission_snapshots below).
    model_id   UUID NOT NULL REFERENCES models(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, model_id)
);

CREATE INDEX IF NOT EXISTS idx_project_model_permissions_project
    ON project_model_permissions(project_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- Extend the existing model soft-delete/redeploy permission-snapshot
-- mechanism (migration 053) to also cover project grants, so a model
-- redeployed under the same name restores BOTH team and project grants —
-- exactly the same guarantee migration 053 already gives team grants.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE model_permission_snapshots
    ADD COLUMN IF NOT EXISTS project_ids JSONB NOT NULL DEFAULT '[]';

CREATE OR REPLACE FUNCTION fn_snapshot_model_permissions()
RETURNS TRIGGER AS $$
DECLARE
    v_team_ids    JSONB;
    v_project_ids JSONB;
BEGIN
    IF NEW.lifecycle = 'deleted' AND OLD.lifecycle != 'deleted' THEN
        SELECT COALESCE(jsonb_agg(tmp.team_id::text), '[]'::jsonb)
        INTO   v_team_ids
        FROM   team_model_permissions tmp
        WHERE  tmp.model_id = OLD.id;

        SELECT COALESCE(jsonb_agg(pmp.project_id::text), '[]'::jsonb)
        INTO   v_project_ids
        FROM   project_model_permissions pmp
        WHERE  pmp.model_id = OLD.id;

        INSERT INTO model_permission_snapshots
            (model_name, deleted_model_id, team_ids, project_ids)
        VALUES
            (OLD.name, OLD.id, v_team_ids, v_project_ids);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

COMMIT;
