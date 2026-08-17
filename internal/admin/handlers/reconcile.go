// Package handlers — reconcile.go
//
// Permission reconciliation (forensic audit, production-readiness round).
//
// PostgreSQL (team_model_permissions / project_model_permissions) is always
// the source of truth; Redis (nexus:team:<id>:models /
// nexus:project:<id>:models) is a derived, best-effort cache that the
// gateway actually enforces against. Every write path (AddModelPermission,
// AddProjectModelPermission, RemoveModelPermission,
// RemoveProjectModelPermission, restorePermissionsFromSnapshot) now commits
// Postgres first and syncs Redis second, checking the sync error — but a
// checked error still means the two stores can drift apart (a transient
// Redis failure after a successful Postgres write). This file provides a
// pull-based reconciliation sweep that detects and repairs that drift in
// both directions:
//   - Postgres has a grant Redis doesn't (under-permissive drift — e.g. a
//     restore's post-commit sync failed) → add it to Redis.
//   - Redis has a grant Postgres doesn't (over-permissive drift — e.g. a
//     revoke's DB delete committed but the Redis removal failed) → remove
//     it from Redis. This direction matters more from a security standpoint:
//     it means the gateway is enforcing an ACL that no longer reflects any
//     admin decision.
//
// Scope: reconciles every team/project that has at least one row in
// team_model_permissions/project_model_permissions, PLUS every team/project
// recorded in model_permission_scopes (migration 059) — which durably marks,
// in Postgres, every scope that has ever had an explicit grant or revoke
// performed on it, even after being revoked back down to zero permission
// rows. This closes a real gap (production security re-audit, Critical
// Finding #2): without the scopes table, a team/project revoked to zero rows
// disappears from reconciliation's view entirely, so a stray Redis ALLOW
// left behind by a revoke whose Redis call failed could never be found or
// repaired again. A scope that is in model_permission_scopes but has zero
// current permission rows is diffed against an EMPTY wanted set — i.e. any
// live Redis member for it is unconditionally stray and removed.
//
// A scope with NO row in either table (never configured at all) is still
// intentionally left untouched — touching its Redis state would risk turning
// a legitimate "never configured" passthrough project into an accidental
// "configured, empty" deny-all project.
package handlers

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/policy"
	"go.uber.org/zap"
)

// ReconcileReport summarizes one reconciliation sweep.
type ReconcileReport struct {
	TeamsChecked    int      `json:"teams_checked"`
	ProjectsChecked int      `json:"projects_checked"`
	TeamRepairs     int      `json:"team_repairs"`
	ProjectRepairs  int      `json:"project_repairs"`
	Errors          []string `json:"errors,omitempty"`
}

// addConfiguredScopes ensures every scope_id recorded in
// model_permission_scopes for the given scopeType appears in `byScope`, even
// if it currently has zero permission rows (an empty map entry, so it's still
// diffed against Redis and any stray live member is removed). Scopes that
// already have rows are left untouched. Best-effort: a query failure here is
// recorded but does not abort reconciliation of the scopes already found via
// the primary permissions-table query.
func addConfiguredScopes(ctx context.Context, db *sqlx.DB, scopeType string, byScope map[string]map[string]bool, errs *[]string) {
	var scopeIDs []string
	if err := db.SelectContext(ctx, &scopeIDs,
		`SELECT scope_id::text FROM model_permission_scopes WHERE scope_type = $1`, scopeType); err != nil {
		*errs = append(*errs, "query model_permission_scopes ("+scopeType+"): "+err.Error())
		return
	}
	for _, id := range scopeIDs {
		if _, ok := byScope[id]; !ok {
			byScope[id] = make(map[string]bool)
		}
	}
}

// ReconcilePermissions diffs Postgres's team_model_permissions and
// project_model_permissions against their corresponding Redis sets and
// repairs any drift found, in both directions (missing entries added,
// stray entries removed). Safe to call repeatedly and concurrently — every
// operation it performs (SAdd/SRem via the same functions the ordinary
// grant/revoke handlers use) is idempotent.
func ReconcilePermissions(ctx context.Context, db *sqlx.DB, engine *policy.Engine, log *zap.Logger) ReconcileReport {
	if log == nil {
		log = zap.NewNop()
	}
	report := ReconcileReport{}

	// ── Teams ──────────────────────────────────────────────────────────────
	type teamModels struct {
		TeamID string `db:"team_id"`
		Name   string `db:"name"`
	}
	var teamRows []teamModels
	if err := db.SelectContext(ctx, &teamRows, `
		SELECT tmp.team_id::text AS team_id, m.name
		FROM team_model_permissions tmp
		JOIN models m ON m.id = tmp.model_id
		WHERE COALESCE(m.lifecycle, 'active') != 'deleted'`); err != nil {
		report.Errors = append(report.Errors, "query team_model_permissions: "+err.Error())
	} else {
		byTeam := make(map[string]map[string]bool)
		for _, r := range teamRows {
			if byTeam[r.TeamID] == nil {
				byTeam[r.TeamID] = make(map[string]bool)
			}
			byTeam[r.TeamID][r.Name] = true
		}
		addConfiguredScopes(ctx, db, "team", byTeam, &report.Errors)
		report.TeamsChecked = len(byTeam)
		for teamID, wanted := range byTeam {
			live, err := engine.TeamModelSet(ctx, teamID)
			if err != nil {
				report.Errors = append(report.Errors, "read team "+teamID+" Redis set: "+err.Error())
				continue
			}
			liveSet := make(map[string]bool, len(live))
			for _, m := range live {
				liveSet[m] = true
			}
			for model := range wanted {
				if !liveSet[model] {
					if err := engine.SetModelAllowed(ctx, teamID, model); err != nil {
						report.Errors = append(report.Errors, "repair team "+teamID+" model "+model+": "+err.Error())
						continue
					}
					report.TeamRepairs++
					log.Warn("reconciliation: added missing team model grant to Redis",
						zap.String("team_id", teamID), zap.String("model", model))
				}
			}
			for model := range liveSet {
				if !wanted[model] {
					if err := engine.RemoveModelAllowed(ctx, teamID, model); err != nil {
						report.Errors = append(report.Errors, "repair team "+teamID+" stray model "+model+": "+err.Error())
						continue
					}
					report.TeamRepairs++
					log.Warn("reconciliation: removed stray team model grant from Redis — Postgres no longer grants it",
						zap.String("team_id", teamID), zap.String("model", model))
				}
			}
		}
	}

	// ── Projects ───────────────────────────────────────────────────────────
	type projectModels struct {
		ProjectID string `db:"project_id"`
		Name      string `db:"name"`
	}
	var projectRows []projectModels
	if err := db.SelectContext(ctx, &projectRows, `
		SELECT pmp.project_id::text AS project_id, m.name
		FROM project_model_permissions pmp
		JOIN models m ON m.id = pmp.model_id
		WHERE COALESCE(m.lifecycle, 'active') != 'deleted'`); err != nil {
		report.Errors = append(report.Errors, "query project_model_permissions: "+err.Error())
	} else {
		byProject := make(map[string]map[string]bool)
		for _, r := range projectRows {
			if byProject[r.ProjectID] == nil {
				byProject[r.ProjectID] = make(map[string]bool)
			}
			byProject[r.ProjectID][r.Name] = true
		}
		addConfiguredScopes(ctx, db, "project", byProject, &report.Errors)
		report.ProjectsChecked = len(byProject)
		for projectID, wanted := range byProject {
			live, err := engine.ProjectModelSet(ctx, projectID)
			if err != nil {
				report.Errors = append(report.Errors, "read project "+projectID+" Redis set: "+err.Error())
				continue
			}
			liveSet := make(map[string]bool, len(live))
			for _, m := range live {
				liveSet[m] = true
			}
			for model := range wanted {
				if !liveSet[model] {
					if err := engine.SetProjectModelAllowed(ctx, projectID, model); err != nil {
						report.Errors = append(report.Errors, "repair project "+projectID+" model "+model+": "+err.Error())
						continue
					}
					report.ProjectRepairs++
					log.Warn("reconciliation: added missing project model grant to Redis",
						zap.String("project_id", projectID), zap.String("model", model))
				}
			}
			for model := range liveSet {
				if !wanted[model] {
					if err := engine.RemoveProjectModelAllowed(ctx, projectID, model); err != nil {
						report.Errors = append(report.Errors, "repair project "+projectID+" stray model "+model+": "+err.Error())
						continue
					}
					report.ProjectRepairs++
					log.Warn("reconciliation: removed stray project model grant from Redis — Postgres no longer grants it",
						zap.String("project_id", projectID), zap.String("model", model))
				}
			}
		}
	}

	return report
}

// ReconciliationHandler exposes ReconcilePermissions as an on-demand admin
// endpoint, in addition to the periodic background sweep (see
// cmd/admin/main.go). Useful to force an immediate repair after a known
// Redis outage instead of waiting for the next scheduled tick.
type ReconciliationHandler struct {
	db     *sqlx.DB
	engine *policy.Engine
	log    *zap.Logger
}

// NewReconciliationHandler constructs a ReconciliationHandler.
func NewReconciliationHandler(db *sqlx.DB, engine *policy.Engine, log *zap.Logger) *ReconciliationHandler {
	if log == nil {
		log, _ = zap.NewProduction()
	}
	return &ReconciliationHandler{db: db, engine: engine, log: log}
}

// ReconcileNow handles POST /admin/v1/system/reconcile-permissions
func (h *ReconciliationHandler) ReconcileNow(c *gin.Context) {
	report := ReconcilePermissions(c.Request.Context(), h.db, h.engine, h.log)
	status := http.StatusOK
	if len(report.Errors) > 0 {
		status = http.StatusPartialContent
	}
	c.JSON(status, report)
}
