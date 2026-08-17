-- Migration 059: model_permission_scopes
--
-- Security re-audit follow-up (Critical Finding #2): ReconcilePermissions
-- previously discovered which teams/projects to check purely by scanning
-- team_model_permissions/project_model_permissions for existing rows. That
-- means a team or project revoked down to ZERO grants disappears from
-- reconciliation's scope entirely — if the Redis-side removal for that last
-- revoke failed (e.g. a transient outage), the stray Redis ALLOW entry can
-- never be found or repaired again, by any future sweep.
--
-- This table gives reconciliation a durable, Postgres-backed (source of
-- truth) way to distinguish:
--   1. Never configured  -> no row here -> full passthrough/inherited access
--      (must NOT be reconciled against an empty set, or a legitimate
--      passthrough project would be turned into an accidental deny-all one).
--   2. Configured, currently zero grants -> row exists here -> deny-all is
--      the correct, intentional state, and any stray Redis entries for this
--      scope ARE a bug that reconciliation must remove.
--
-- Populated by every explicit grant/revoke call (team.go's
-- AddModelPermission/RemoveModelPermission, project_models.go's
-- AddProjectModelPermission/RemoveProjectModelPermission) via
-- INSERT ... ON CONFLICT DO NOTHING — set on first touch, never cleared,
-- mirroring the existing Redis-side "configured" marker semantics
-- (nexus:project:<id>:models:configured) that Evaluate already relies on
-- for the identical distinction on the read path.

BEGIN;

CREATE TABLE IF NOT EXISTS model_permission_scopes (
    scope_type    TEXT NOT NULL CHECK (scope_type IN ('team', 'project')),
    scope_id      UUID NOT NULL,
    configured_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (scope_type, scope_id)
);

COMMIT;
