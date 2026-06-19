package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/models"
	"github.com/nexusllm/nexusllm/internal/policy"
	"github.com/redis/go-redis/v9"
)

// TeamHandler manages team CRUD, policies, and model permissions.
type TeamHandler struct {
	db     *sqlx.DB
	rdb    *redis.Client
	engine *policy.Engine
}

// NewTeamHandler constructs a TeamHandler.
func NewTeamHandler(db *sqlx.DB, rdb *redis.Client, engine *policy.Engine) *TeamHandler {
	return &TeamHandler{db: db, rdb: rdb, engine: engine}
}

// CreateTeam handles POST /admin/v1/orgs/:org_id/teams
func (h *TeamHandler) CreateTeam(c *gin.Context) {
	orgID := c.Param("org_id")
	var input struct {
		Name     string `json:"name"     binding:"required"`
		Slug     string `json:"slug"     binding:"required"`
		Priority int    `json:"priority"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.Priority == 0 {
		input.Priority = 5
	}

	team := models.Team{
		ID:        uuid.New().String(),
		OrgID:     orgID,
		Name:      input.Name,
		Slug:      input.Slug,
		Priority:  input.Priority,
		Active:    true,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	_, err := h.db.ExecContext(c.Request.Context(),
		`INSERT INTO teams (id, org_id, name, slug, priority, active, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		team.ID, team.OrgID, team.Name, team.Slug, team.Priority, team.Active, team.CreatedAt, team.UpdatedAt,
	)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "team already exists or slug conflict"})
		return
	}

	// Seed a default policy row
	_, _ = h.db.ExecContext(c.Request.Context(),
		`INSERT INTO policies (id, team_id, rpm, tpd, max_concurrent, max_context_tokens, created_at, updated_at)
		 VALUES ($1,$2,100,1000000,10,8192,NOW(),NOW())`,
		uuid.New().String(), team.ID,
	)

	c.JSON(http.StatusCreated, team)
}

// GetTeam handles GET /admin/v1/teams/:id
func (h *TeamHandler) GetTeam(c *gin.Context) {
	id := c.Param("id")
	var team models.Team
	if err := h.db.GetContext(c.Request.Context(), &team,
		`SELECT id, org_id, name, slug, priority, active, created_at, updated_at FROM teams WHERE id = $1`, id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "team not found"})
		return
	}
	c.JSON(http.StatusOK, team)
}

// ListTeams handles GET /admin/v1/orgs/:org_id/teams
func (h *TeamHandler) ListTeams(c *gin.Context) {
	orgID := c.Param("org_id")
	var teams []models.Team
	if err := h.db.SelectContext(c.Request.Context(), &teams,
		`SELECT id, org_id, name, slug, priority, active, created_at, updated_at
		 FROM teams WHERE org_id = $1 ORDER BY priority DESC`, orgID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": teams, "total": len(teams)})
}

// UpdateTeamPolicy handles PUT /admin/v1/teams/:id/policy
func (h *TeamHandler) UpdateTeamPolicy(c *gin.Context) {
	teamID := c.Param("id")
	var input struct {
		RPM              *int `json:"rpm"`
		TPD              *int `json:"tpd"`
		MaxConcurrent    *int `json:"max_concurrent"`
		MaxContextTokens *int `json:"max_context_tokens"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Build update query dynamically
	q := `UPDATE policies SET updated_at = NOW()`
	args := []interface{}{}
	idx := 1

	if input.RPM != nil {
		q += `, rpm = $` + itoa(idx)
		args = append(args, *input.RPM)
		idx++
	}
	if input.TPD != nil {
		q += `, tpd = $` + itoa(idx)
		args = append(args, *input.TPD)
		idx++
	}
	if input.MaxConcurrent != nil {
		q += `, max_concurrent = $` + itoa(idx)
		args = append(args, *input.MaxConcurrent)
		idx++
	}
	if input.MaxContextTokens != nil {
		q += `, max_context_tokens = $` + itoa(idx)
		args = append(args, *input.MaxContextTokens)
		idx++
	}
	q += ` WHERE team_id = $` + itoa(idx)
	args = append(args, teamID)

	if _, err := h.db.ExecContext(c.Request.Context(), q, args...); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update policy"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "policy updated"})
}

// GetTeamPolicy handles GET /admin/v1/teams/:id/policy
func (h *TeamHandler) GetTeamPolicy(c *gin.Context) {
	teamID := c.Param("id")
	var p models.Policy
	if err := h.db.GetContext(c.Request.Context(), &p,
		`SELECT id, team_id, rpm, tpd, max_concurrent, max_context_tokens, created_at, updated_at
		 FROM policies WHERE team_id = $1`, teamID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "policy not found"})
		return
	}
	c.JSON(http.StatusOK, p)
}

// AddModelPermission handles POST /admin/v1/teams/:id/models
func (h *TeamHandler) AddModelPermission(c *gin.Context) {
	teamID := c.Param("id")
	var input struct {
		ModelName string `json:"model_name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Look up model ID
	var modelID string
	if err := h.db.GetContext(c.Request.Context(), &modelID,
		`SELECT id FROM models WHERE name = $1 AND active = TRUE`, input.ModelName); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
		return
	}

	_, err := h.db.ExecContext(c.Request.Context(),
		`INSERT INTO team_model_permissions (team_id, model_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		teamID, modelID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}

	// Sync to Redis
	if err := h.engine.SetModelAllowed(c.Request.Context(), teamID, input.ModelName); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "redis sync failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "model permission granted"})
}

// RemoveModelPermission handles DELETE /admin/v1/teams/:id/models/:model
func (h *TeamHandler) RemoveModelPermission(c *gin.Context) {
	teamID := c.Param("id")
	modelName := c.Param("model")

	var modelID string
	if err := h.db.GetContext(c.Request.Context(), &modelID,
		`SELECT id FROM models WHERE name = $1`, modelName); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
		return
	}

	_, _ = h.db.ExecContext(c.Request.Context(),
		`DELETE FROM team_model_permissions WHERE team_id = $1 AND model_id = $2`, teamID, modelID)

	_ = h.engine.RemoveModelAllowed(c.Request.Context(), teamID, modelName)

	c.JSON(http.StatusOK, gin.H{"message": "model permission removed"})
}

// DeactivateTeam handles DELETE /admin/v1/teams/:id
func (h *TeamHandler) DeactivateTeam(c *gin.Context) {
	id := c.Param("id")
	res, err := h.db.ExecContext(c.Request.Context(),
		`UPDATE teams SET active = FALSE, updated_at = NOW() WHERE id = $1`, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "team not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "team deactivated"})
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
