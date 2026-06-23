package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/models"
)

// OrgHandler manages organization CRUD.
type OrgHandler struct {
	db *sqlx.DB
}

// NewOrgHandler constructs an OrgHandler.
func NewOrgHandler(db *sqlx.DB) *OrgHandler {
	return &OrgHandler{db: db}
}

// CreateOrg handles POST /admin/v1/orgs
func (h *OrgHandler) CreateOrg(c *gin.Context) {
	var input struct {
		Name string `json:"name" binding:"required"`
		Slug string `json:"slug" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	org := models.Organization{
		ID:        uuid.New().String(),
		Name:      input.Name,
		Slug:      input.Slug,
		Active:    true,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	_, err := h.db.ExecContext(c.Request.Context(),
		`INSERT INTO organizations (id, name, slug, active, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		org.ID, org.Name, org.Slug, org.Active, org.CreatedAt, org.UpdatedAt,
	)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "organization already exists or slug conflict"})
		return
	}
	c.JSON(http.StatusCreated, org)
}

// GetOrg handles GET /admin/v1/orgs/:id
func (h *OrgHandler) GetOrg(c *gin.Context) {
	id := c.Param("id")
	var org models.Organization
	err := h.db.GetContext(c.Request.Context(), &org,
		`SELECT id, name, slug, active, created_at, updated_at FROM organizations WHERE id = $1`, id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
		return
	}
	c.JSON(http.StatusOK, org)
}

// ListOrgs handles GET /admin/v1/orgs
func (h *OrgHandler) ListOrgs(c *gin.Context) {
	orgs := []models.Organization{}
	err := h.db.SelectContext(c.Request.Context(), &orgs,
		`SELECT id, name, slug, active, created_at, updated_at FROM organizations ORDER BY created_at DESC`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	if orgs == nil {
		orgs = []models.Organization{}
	}
	c.JSON(http.StatusOK, gin.H{"data": orgs, "total": len(orgs)})
}

// DeactivateOrg handles DELETE /admin/v1/orgs/:id
// Performs a hard delete of the organization and all its cascading data.
func (h *OrgHandler) DeactivateOrg(c *gin.Context) {
	id := c.Param("id")

	// Hard delete — cascades to teams, policies, team_model_permissions via FK ON DELETE CASCADE
	res, err := h.db.ExecContext(c.Request.Context(),
		`DELETE FROM organizations WHERE id = $1`, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error: " + err.Error()})
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "organization deleted", "id": id})
}
