// Package handlers — project_models.go
//
// Project-level model authorization for Public (regular/managed) models.
// Mirrors team.go's AddModelPermission/RemoveModelPermission/ListTeamModels
// exactly, keyed by project_id instead of team_id, and writing to
// project_model_permissions + the nexus:project:<id>:models Redis set
// (internal/policy.Engine.SetProjectModelAllowed) instead of the team
// equivalents.
//
// Enforcement (Option A — a project can only narrow its team's access, never
// widen it) lives in internal/policy/engine.go's Evaluate. This file is only
// the admin-facing grant/revoke/list surface.
package handlers

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
)

// ProjectModelGrant is one row in ListProjectModels' response — a granted
// model plus whether it's actually confirmed present in the live Redis ACL
// set. Synced=false means Postgres shows the grant but Redis doesn't
// (usually a transient sync failure — see restorePermissionsFromSnapshot
// and ReconcilePermissions in reconcile.go) — the UI surfaces this instead
// of silently showing a green "granted" chip for a permission that isn't
// actually enforced yet.
type ProjectModelGrant struct {
	Name   string `json:"name"`
	Synced bool   `json:"synced"`
}

// ListProjectModels handles GET /admin/v1/projects/:id/models
func (h *ProjectHandler) ListProjectModels(c *gin.Context) {
	projectID := c.Param("id")
	type modelRow struct {
		Name string `db:"name" json:"name"`
	}
	var rows []modelRow
	// DISTINCT + excluding lifecycle='deleted' for the same reason as
	// team.go's ListTeamModels: a model redeploy under the same name leaves
	// the old (now-deleted) model_id's project grant row in place so
	// model_permission_snapshots can restore it later.
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT DISTINCT m.name
		FROM project_model_permissions pmp
		JOIN models m ON m.id = pmp.model_id
		WHERE pmp.project_id = $1
		  AND COALESCE(m.lifecycle, 'active') != 'deleted'
		ORDER BY m.name`, projectID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Cross-check each grant against the live Redis set so the UI can show
	// sync state rather than blindly trusting the DB row exists.
	var liveSet map[string]bool
	if live, err := h.engine.ProjectModelSet(c.Request.Context(), projectID); err == nil {
		liveSet = make(map[string]bool, len(live))
		for _, m := range live {
			liveSet[m] = true
		}
	}

	grants := make([]ProjectModelGrant, len(rows))
	for i, r := range rows {
		grants[i] = ProjectModelGrant{Name: r.Name, Synced: liveSet != nil && liveSet[r.Name]}
	}
	c.JSON(http.StatusOK, gin.H{"models": grants})
}

// AddProjectModelPermission handles POST /admin/v1/projects/:id/models
func (h *ProjectHandler) AddProjectModelPermission(c *gin.Context) {
	projectID := c.Param("id")
	var input struct {
		ModelName string `json:"model_name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var modelID string
	err := h.db.GetContext(c.Request.Context(), &modelID, `
		SELECT id FROM models
		WHERE name = $1 AND enabled = TRUE AND COALESCE(lifecycle,'active') != 'deleted'
		ORDER BY created_at DESC LIMIT 1`, input.ModelName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "model not found: " + input.ModelName})
		return
	}

	_, err = h.db.ExecContext(c.Request.Context(),
		`INSERT INTO project_model_permissions (project_id, model_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		projectID, modelID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	markModelPermissionScopeConfigured(c.Request.Context(), h.db, "project", projectID)

	// The Redis write (and the "configured" marker it sets — see
	// projectModelsConfiguredKey) is what the gateway actually enforces
	// against. A silent failure here must not be reported as success.
	if err := h.engine.SetProjectModelAllowed(c.Request.Context(), projectID, input.ModelName); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "permission recorded in database but ACL sync failed — retry the grant: " + err.Error(),
		})
		return
	}
	h.syncProjectPermissionChange(c.Request.Context(), projectID)
	c.JSON(http.StatusOK, gin.H{"message": "project model permission granted"})
}

// RemoveProjectModelPermission handles DELETE /admin/v1/projects/:id/models/:model
func (h *ProjectHandler) RemoveProjectModelPermission(c *gin.Context) {
	projectID := c.Param("id")
	modelName := c.Param("model")

	var modelID string
	if err := h.db.GetContext(c.Request.Context(), &modelID,
		`SELECT id FROM models WHERE name = $1 ORDER BY created_at DESC LIMIT 1`, modelName); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
		return
	}

	_, err := h.db.ExecContext(c.Request.Context(),
		`DELETE FROM project_model_permissions WHERE project_id = $1 AND model_id = $2`, projectID, modelID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	markModelPermissionScopeConfigured(c.Request.Context(), h.db, "project", projectID)

	if err := h.engine.RemoveProjectModelAllowed(c.Request.Context(), projectID, modelName); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "permission removed in database but ACL sync failed — retry the revoke: " + err.Error(),
		})
		return
	}
	h.syncProjectPermissionChange(c.Request.Context(), projectID)
	c.JSON(http.StatusOK, gin.H{"message": "project model permission removed"})
}

// syncProjectPermissionChange busts the claims cache for every active API key
// scoped to this project, mirroring team.go's syncPermissionChanges. The
// model ACL check itself is live (no cache in front of it — see Evaluate),
// so this is purely for claims.Permissions-style fields that DO cache.
func (h *ProjectHandler) syncProjectPermissionChange(ctx context.Context, projectID string) {
	var hashes []string
	_ = h.db.SelectContext(ctx, &hashes, `SELECT key_hash FROM api_keys WHERE project_id = $1 AND active = TRUE`, projectID)
	for _, hash := range hashes {
		_ = h.rdb.Del(ctx, "nexus:apikey:"+hash).Err()
	}
}
