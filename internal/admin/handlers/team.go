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

// CreateTeam handles POST /admin/v1/teams
// org_id is provided in the request body.
func (h *TeamHandler) CreateTeam(c *gin.Context) {
	var input struct {
		OrgID    string `json:"org_id"   binding:"required"`
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
		OrgID:     input.OrgID,
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
		team.ID, team.OrgID, team.Name, team.Slug, team.Priority,
		team.Active, team.CreatedAt, team.UpdatedAt,
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

// ListTeams handles GET /admin/v1/teams?org_id=<uuid>
func (h *TeamHandler) ListTeams(c *gin.Context) {
	orgID := c.Query("org_id")

	teams := []models.Team{}
	var err error
	if orgID != "" {
		err = h.db.SelectContext(c.Request.Context(), &teams,
			`SELECT id, org_id, name, slug, priority, active, created_at, updated_at
			 FROM teams WHERE org_id = $1 ORDER BY priority DESC`, orgID)
	} else {
		err = h.db.SelectContext(c.Request.Context(), &teams,
			`SELECT id, org_id, name, slug, priority, active, created_at, updated_at
			 FROM teams ORDER BY priority DESC`)
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	if teams == nil {
		teams = []models.Team{}
	}
	c.JSON(http.StatusOK, gin.H{"data": teams, "total": len(teams)})
}

// GetTeam handles GET /admin/v1/teams/:id
func (h *TeamHandler) GetTeam(c *gin.Context) {
	id := c.Param("id")
	var team models.Team
	if err := h.db.GetContext(c.Request.Context(), &team,
		`SELECT id, org_id, name, slug, priority, active, created_at, updated_at
		 FROM teams WHERE id = $1`, id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "team not found"})
		return
	}
	c.JSON(http.StatusOK, team)
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

	q := `UPDATE policies SET updated_at = NOW()`
	args := []interface{}{}
	idx := 1

	if input.RPM != nil {
		q += fmt.Sprintf(`, rpm = $%d`, idx)
		args = append(args, *input.RPM)
		idx++
	}
	if input.TPD != nil {
		q += fmt.Sprintf(`, tpd = $%d`, idx)
		args = append(args, *input.TPD)
		idx++
	}
	if input.MaxConcurrent != nil {
		q += fmt.Sprintf(`, max_concurrent = $%d`, idx)
		args = append(args, *input.MaxConcurrent)
		idx++
	}
	if input.MaxContextTokens != nil {
		q += fmt.Sprintf(`, max_context_tokens = $%d`, idx)
		args = append(args, *input.MaxContextTokens)
		idx++
	}
	q += fmt.Sprintf(` WHERE team_id = $%d`, idx)
	args = append(args, teamID)

	if _, err := h.db.ExecContext(c.Request.Context(), q, args...); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update policy"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "policy updated"})
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

	// Look up model ID — use enabled column (migration 003)
	var modelID string
	err := h.db.GetContext(c.Request.Context(), &modelID,
		`SELECT id FROM models WHERE name = $1 AND enabled = TRUE`, input.ModelName)
	if err != nil {
		// Fallback: model may not have enabled column yet (pre-003 schema)
		err = h.db.GetContext(c.Request.Context(), &modelID,
			`SELECT id FROM models WHERE name = $1`, input.ModelName)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "model not found: " + input.ModelName})
			return
		}
	}

	_, err = h.db.ExecContext(c.Request.Context(),
		`INSERT INTO team_model_permissions (team_id, model_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		teamID, modelID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}

	_ = h.engine.SetModelAllowed(c.Request.Context(), teamID, input.ModelName)
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
