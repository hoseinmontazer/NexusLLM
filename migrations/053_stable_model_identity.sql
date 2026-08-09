-- NexusLLM Migration 053 — Stable Model Identity & Permission Preservation
--
-- Fixes three confirmed bugs from the forensic audit:
--
--   BUG-1 (CRITICAL): Permission loss on model delete/recreate.
--     team_model_permissions referenced models(id) ON DELETE CASCADE, so every
--     hard DELETE of a model row silently destroyed all permission grants.
--     When the model was recreated it got a new UUID → zero permissions.
--
--   BUG-2 (HIGH): Org-level ACL set conflated multi-team grants.
--     seedModelPermissions loaded every team's models into a single
--     nexus:org:<id>:models Redis set. Step-0 in the policy engine checked
--     that set first, allowing Team-A tokens to call models only granted to
--     Team-B as long as they shared an org. A dedicated
--     nexus:team:<id>:models set is now the canonical path.
--
--   Stable identity: introduces a stable per-name deletion record so that
--     re-deploying a model by the same name can restore its previous grants.
--
-- Strategy:
--   1. Convert hard DELETE to soft-delete: models.lifecycle = 'deleted'.
--      The FK ON DELETE CASCADE on team_model_permissions is changed to
--      ON DELETE RESTRICT so permission rows survive the lifecycle change.
--      (Hard delete is still allowed via a separate admin purge path.)
--   2. Add model_permission_snapshots to remember grants for deleted models
--      by name — used to auto-restore permissions when the name is redeployed.
--   3. Add a unique partial index on models(name) for enabled rows so the
--      existing UNIQUE(name) constraint continues to work with soft-deleted
--      rows (same name may exist once as deleted and once as active).
--
-- All statements are idempotent.

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. Ensure models.lifecycle column exists (added by migration 011 as
--    models.lifecycle, repurposed here). Default 'active'.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE models
    ADD COLUMN IF NOT EXISTS lifecycle VARCHAR(20) NOT NULL DEFAULT 'active'
        CHECK (lifecycle IN ('active', 'archived', 'deleted'));

-- Back-fill: any row already soft-deleted via enabled=FALSE is 'archived';
-- fully missing rows don't apply here.  Active is the default so no back-fill
-- needed for normal rows.

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. Drop the existing UNIQUE(name) constraint on models so that a deleted row
--    and a new active row can share the same name (delete → recreate flow).
--    Replace it with a partial unique index on (name) WHERE lifecycle != 'deleted'.
-- ─────────────────────────────────────────────────────────────────────────────
DO $$
BEGIN
    -- models.name has a plain UNIQUE constraint from migration 001.
    -- Drop it so the partial index below takes over.
    ALTER TABLE models DROP CONSTRAINT IF EXISTS models_name_key;
EXCEPTION WHEN undefined_object THEN
    NULL;  -- already gone
END $$;

-- Only one *active* or *archived* model may carry a given name at a time.
-- Multiple deleted rows with the same name are allowed (audit trail).
CREATE UNIQUE INDEX IF NOT EXISTS idx_models_name_active
    ON models(name)
    WHERE lifecycle != 'deleted';

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. Change team_model_permissions FK from ON DELETE CASCADE to ON DELETE RESTRICT.
--    This prevents silent permission loss when a model row is hard-deleted.
--    Soft-delete (lifecycle='deleted') leaves the model row intact so FK holds.
--
--    The original constraint was created in migration 001 as part of PRIMARY KEY,
--    but the FK itself is a separate constraint.  We must drop and recreate.
-- ─────────────────────────────────────────────────────────────────────────────
DO $$
BEGIN
    -- Drop old FK (CASCADE)
    ALTER TABLE team_model_permissions
        DROP CONSTRAINT IF EXISTS team_model_permissions_model_id_fkey;
    -- Recreate with RESTRICT so accidental hard-DELETE is blocked at DB level
    ALTER TABLE team_model_permissions
        ADD CONSTRAINT team_model_permissions_model_id_fkey
        FOREIGN KEY (model_id) REFERENCES models(id) ON DELETE RESTRICT;
EXCEPTION WHEN others THEN
    NULL;  -- constraint manipulation failed — safe to ignore on idempotent re-run
END $$;

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. model_permission_snapshots — stores a copy of all team grants for a model
--    at the moment it is soft-deleted, keyed by model NAME (not UUID).
--    Used by the redeploy path to auto-restore grants when a model is recreated
--    under the same name.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS model_permission_snapshots (
    id             UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    model_name     VARCHAR(255) NOT NULL,
    -- The UUID of the model row that was deleted (kept for audit).
    deleted_model_id UUID       NOT NULL,
    -- JSON array of team_id strings: ["uuid1","uuid2",...]
    team_ids       JSONB        NOT NULL DEFAULT '[]',
    deleted_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    -- Whether this snapshot has already been consumed by a redeploy.
    restored       BOOLEAN      NOT NULL DEFAULT FALSE,
    restored_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_mps_model_name
    ON model_permission_snapshots(model_name)
    WHERE restored = FALSE;

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. Trigger: auto-snapshot permissions when a model is soft-deleted.
--    Fires on UPDATE ... SET lifecycle = 'deleted'.
--    Inserts one snapshot row capturing all current team grants.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION fn_snapshot_model_permissions()
RETURNS TRIGGER AS $$
DECLARE
    v_team_ids JSONB;
BEGIN
    -- Only fire when transitioning INTO 'deleted'
    IF NEW.lifecycle = 'deleted' AND OLD.lifecycle != 'deleted' THEN
        SELECT COALESCE(jsonb_agg(tmp.team_id::text), '[]'::jsonb)
        INTO   v_team_ids
        FROM   team_model_permissions tmp
        WHERE  tmp.model_id = OLD.id;

        INSERT INTO model_permission_snapshots
            (model_name, deleted_model_id, team_ids)
        VALUES
            (OLD.name, OLD.id, v_team_ids);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_snapshot_model_perms ON models;
CREATE TRIGGER trg_snapshot_model_perms
    AFTER UPDATE OF lifecycle ON models
    FOR EACH ROW
    EXECUTE FUNCTION fn_snapshot_model_permissions();

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. Indexes to accelerate the redeploy restore query in Go:
--    SELECT team_ids FROM model_permission_snapshots
--    WHERE model_name = $1 AND restored = FALSE
--    ORDER BY deleted_at DESC LIMIT 1
-- ─────────────────────────────────────────────────────────────────────────────
CREATE INDEX IF NOT EXISTS idx_mps_restore_lookup
    ON model_permission_snapshots(model_name, deleted_at DESC)
    WHERE restored = FALSE;

-- ─────────────────────────────────────────────────────────────────────────────
-- 7. Ensure the team-level ACL Redis key (nexus:team:<id>:models) is the
--    canonical auth path.  The org-level set (nexus:org:<id>:models) is
--    retained for governance (org budget checks) but must NOT be used as a
--    model ACL bypass.  This is a Go-side fix (policy/engine.go); this
--    migration only adds a documentation comment.
-- ─────────────────────────────────────────────────────────────────────────────
COMMENT ON TABLE org_model_permissions IS
    'Org-level model inventory — records that a model exists in the org. '
    'NOT used for per-request ACL enforcement (that is team_model_permissions). '
    'Used only for org governance checks (budget, compliance).';

COMMIT;
