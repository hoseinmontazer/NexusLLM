package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	internalauth "github.com/nexusllm/nexusllm/internal/auth"
	"github.com/redis/go-redis/v9"
)

// APIKeyHandler manages API key lifecycle.
type APIKeyHandler struct {
	db  *sqlx.DB
	rdb *redis.Client
}

// NewAPIKeyHandler constructs an APIKeyHandler.
func NewAPIKeyHandler(db *sqlx.DB, rdb *redis.Client) *APIKeyHandler {
	return &APIKeyHandler{db: db, rdb: rdb}
}

// CreateAPIKey handles POST /admin/v1/teams/:id/api-keys
// Optionally accepts project_id to scope the key to a specific project.
// The raw key is returned exactly once — it is never stored in plaintext.
func (h *APIKeyHandler) CreateAPIKey(c *gin.Context) {
	teamID := c.Param("id")

	var input struct {
		Name      string     `json:"name"       binding:"required"`
		ExpiresAt *time.Time `json:"expires_at"`
		// Optional: scope this key to a specific project.
		// When set, every request using this key automatically inherits
		// the project's priority_weight for scheduling.
		ProjectID *string `json:"project_id"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Verify team exists
	var exists bool
	if err := h.db.GetContext(c.Request.Context(), &exists,
		`SELECT EXISTS(SELECT 1 FROM teams WHERE id = $1 AND active = TRUE)`, teamID); err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "team not found"})
		return
	}

	// If project_id provided, resolve project name + priority_weight for denormalisation
	var projectName string
	var projectPriorityWeight int
	if input.ProjectID != nil && *input.ProjectID != "" {
		var proj struct {
			Name           string `db:"name"`
			PriorityWeight int    `db:"priority_weight"`
			ProjTeamID     string `db:"team_id"`
		}
		if err := h.db.GetContext(c.Request.Context(), &proj,
			`SELECT name, priority_weight, team_id::text FROM projects WHERE id=$1 AND status='active'`,
			*input.ProjectID); err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "project not found or not active"})
			return
		}
		if proj.ProjTeamID != teamID {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "project does not belong to this team"})
			return
		}
		projectName = proj.Name
		projectPriorityWeight = proj.PriorityWeight
	}

	raw, hash, err := internalauth.GenerateAPIKey()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate key"})
		return
	}

	keyID := uuid.New().String()
	prefix := raw[:12] // "nxs_" + 8 chars

	if input.ProjectID != nil && *input.ProjectID != "" {
		// Insert with project scope (requires migration 022)
		_, err = h.db.ExecContext(c.Request.Context(), `
			INSERT INTO api_keys
			  (id, team_id, name, key_hash, key_prefix, active, expires_at, created_at, updated_at,
			   project_id, project_name, project_priority_weight)
			VALUES ($1,$2,$3,$4,$5,$6,$7,NOW(),NOW(),$8,$9,$10)`,
			keyID, teamID, input.Name, hash, prefix, true, input.ExpiresAt,
			*input.ProjectID, projectName, projectPriorityWeight,
		)
	} else {
		_, err = h.db.ExecContext(c.Request.Context(), `
			INSERT INTO api_keys (id, team_id, name, key_hash, key_prefix, active, expires_at, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,NOW(),NOW())`,
			keyID, teamID, input.Name, hash, prefix, true, input.ExpiresAt,
		)
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store key: " + err.Error()})
		return
	}

	resp := gin.H{
		"id":         keyID,
		"name":       input.Name,
		"key":        raw, // shown once only
		"key_prefix": prefix,
		"team_id":    teamID,
		"expires_at": input.ExpiresAt,
	}
	if input.ProjectID != nil && *input.ProjectID != "" {
		resp["project_id"] = *input.ProjectID
		resp["project_name"] = projectName
		resp["project_priority_weight"] = projectPriorityWeight
	}
	c.JSON(http.StatusCreated, resp)
}

// ListAPIKeys handles GET /admin/v1/teams/:id/api-keys
func (h *APIKeyHandler) ListAPIKeys(c *gin.Context) {
	teamID := c.Param("id")
	type keyRow struct {
		ID                    string     `db:"id"                      json:"id"`
		TeamID                string     `db:"team_id"                 json:"team_id"`
		Name                  string     `db:"name"                    json:"name"`
		KeyPrefix             string     `db:"key_prefix"              json:"key_prefix"`
		Active                bool       `db:"active"                  json:"active"`
		LastUsedAt            *time.Time `db:"last_used_at"            json:"last_used_at"`
		ExpiresAt             *time.Time `db:"expires_at"              json:"expires_at"`
		CreatedAt             time.Time  `db:"created_at"              json:"created_at"`
		UpdatedAt             time.Time  `db:"updated_at"              json:"updated_at"`
		ProjectID             *string    `db:"project_id"              json:"project_id,omitempty"`
		ProjectName           string     `db:"project_name"            json:"project_name,omitempty"`
		ProjectPriorityWeight int        `db:"project_priority_weight" json:"project_priority_weight,omitempty"`
	}
	var keys []keyRow
	// Try with project columns (migration 022); fall back if not applied yet.
	err := h.db.SelectContext(c.Request.Context(), &keys, `
		SELECT id, team_id, name, key_prefix, active, last_used_at, expires_at,
		       created_at, updated_at,
		       project_id::text AS project_id,
		       COALESCE(project_name,'')     AS project_name,
		       COALESCE(project_priority_weight,0) AS project_priority_weight
		FROM api_keys
		WHERE team_id = $1
		ORDER BY created_at DESC`, teamID)
	if err != nil {
		// Migration 022 not applied — fall back to base columns only.
		type legacyRow struct {
			ID         string     `db:"id"          json:"id"`
			TeamID     string     `db:"team_id"     json:"team_id"`
			Name       string     `db:"name"        json:"name"`
			KeyPrefix  string     `db:"key_prefix"  json:"key_prefix"`
			Active     bool       `db:"active"      json:"active"`
			LastUsedAt *time.Time `db:"last_used_at" json:"last_used_at"`
			ExpiresAt  *time.Time `db:"expires_at"  json:"expires_at"`
			CreatedAt  time.Time  `db:"created_at"  json:"created_at"`
			UpdatedAt  time.Time  `db:"updated_at"  json:"updated_at"`
		}
		var legacy []legacyRow
		if err2 := h.db.SelectContext(c.Request.Context(), &legacy,
			`SELECT id, team_id, name, key_prefix, active, last_used_at, expires_at, created_at, updated_at
			 FROM api_keys WHERE team_id = $1 ORDER BY created_at DESC`, teamID); err2 != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
			return
		}
		if legacy == nil {
			legacy = []legacyRow{}
		}
		c.JSON(http.StatusOK, gin.H{"data": legacy, "total": len(legacy)})
		return
	}
	if keys == nil {
		keys = []keyRow{}
	}
	c.JSON(http.StatusOK, gin.H{"data": keys, "total": len(keys)})
}

// SetKeyProject handles PUT /admin/v1/api-keys/:id/project
// Scopes an existing API key to a project so requests inherit project priority.
func (h *APIKeyHandler) SetKeyProject(c *gin.Context) {
	keyID := c.Param("id")
	var input struct {
		ProjectID *string `json:"project_id"` // null = remove project scope
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if input.ProjectID == nil || *input.ProjectID == "" {
		// Remove project scope
		_, err := h.db.ExecContext(c.Request.Context(), `
			UPDATE api_keys
			SET project_id = NULL, project_name = '', project_priority_weight = 500, updated_at = NOW()
			WHERE id = $1`, keyID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "project scope removed", "key_id": keyID})
		return
	}

	// Resolve project and verify it belongs to the same team as the key.
	var proj struct {
		Name           string `db:"name"`
		PriorityWeight int    `db:"priority_weight"`
		TeamID         string `db:"team_id"`
	}
	if err := h.db.GetContext(c.Request.Context(), &proj,
		`SELECT name, priority_weight, team_id::text FROM projects WHERE id=$1 AND status='active'`,
		*input.ProjectID); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "project not found or not active"})
		return
	}
	var keyTeamID string
	if err := h.db.GetContext(c.Request.Context(), &keyTeamID,
		`SELECT team_id::text FROM api_keys WHERE id=$1`, keyID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "api key not found"})
		return
	}
	if proj.TeamID != keyTeamID {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "project does not belong to the same team as this API key"})
		return
	}

	_, err := h.db.ExecContext(c.Request.Context(), `
		UPDATE api_keys
		SET project_id = $2, project_name = $3, project_priority_weight = $4, updated_at = NOW()
		WHERE id = $1`,
		keyID, *input.ProjectID, proj.Name, proj.PriorityWeight)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Invalidate Redis cache for this key so next request reloads from DB.
	var keyHash string
	_ = h.db.GetContext(c.Request.Context(), &keyHash, `SELECT key_hash FROM api_keys WHERE id=$1`, keyID)
	if keyHash != "" {
		_ = h.rdb.Del(c.Request.Context(), "nexus:apikey:"+keyHash)
	}

	c.JSON(http.StatusOK, gin.H{
		"message":                 "project scope updated",
		"key_id":                  keyID,
		"project_id":              *input.ProjectID,
		"project_name":            proj.Name,
		"project_priority_weight": proj.PriorityWeight,
	})
}

// RevokeAPIKey handles DELETE /admin/v1/api-keys/:id
func (h *APIKeyHandler) RevokeAPIKey(c *gin.Context) {
	keyID := c.Param("id")

	// Get the hash before deactivating (needed for cache invalidation)
	var keyHash string
	if err := h.db.GetContext(c.Request.Context(), &keyHash,
		`SELECT key_hash FROM api_keys WHERE id = $1`, keyID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "api key not found"})
		return
	}

	res, err := h.db.ExecContext(c.Request.Context(),
		`UPDATE api_keys SET active = FALSE, updated_at = NOW() WHERE id = $1`, keyID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "api key not found"})
		return
	}

	// Purge from Redis cache
	_ = h.rdb.Del(c.Request.Context(), "nexus:apikey:"+keyHash)

	c.JSON(http.StatusOK, gin.H{"message": "api key revoked"})
}
