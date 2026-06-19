package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/models"
)

// ModelHandler manages model registry CRUD.
type ModelHandler struct {
	db *sqlx.DB
}

// NewModelHandler constructs a ModelHandler.
func NewModelHandler(db *sqlx.DB) *ModelHandler {
	return &ModelHandler{db: db}
}

// RegisterModel handles POST /admin/v1/models
func (h *ModelHandler) RegisterModel(c *gin.Context) {
	var input struct {
		Name         string `json:"name"          binding:"required"`
		DisplayName  string `json:"display_name"  binding:"required"`
		VLLMEndpoint string `json:"vllm_endpoint" binding:"required"`
		MaxTokens    int    `json:"max_tokens"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.MaxTokens == 0 {
		input.MaxTokens = 4096
	}

	m := models.Model{
		ID:           uuid.New().String(),
		Name:         input.Name,
		DisplayName:  input.DisplayName,
		VLLMEndpoint: input.VLLMEndpoint,
		MaxTokens:    input.MaxTokens,
		Active:       true,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	_, err := h.db.ExecContext(c.Request.Context(),
		`INSERT INTO models (id, name, display_name, vllm_endpoint, max_tokens, active, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		m.ID, m.Name, m.DisplayName, m.VLLMEndpoint, m.MaxTokens, m.Active, m.CreatedAt, m.UpdatedAt,
	)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "model name already exists"})
		return
	}
	c.JSON(http.StatusCreated, m)
}

// ListModels handles GET /admin/v1/models
func (h *ModelHandler) ListModels(c *gin.Context) {
	var ms []models.Model
	if err := h.db.SelectContext(c.Request.Context(), &ms,
		`SELECT id, name, display_name, vllm_endpoint, max_tokens, active, created_at, updated_at
		 FROM models ORDER BY name`); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": ms, "total": len(ms)})
}

// UpdateModel handles PUT /admin/v1/models/:id
func (h *ModelHandler) UpdateModel(c *gin.Context) {
	id := c.Param("id")
	var input struct {
		DisplayName  *string `json:"display_name"`
		VLLMEndpoint *string `json:"vllm_endpoint"`
		MaxTokens    *int    `json:"max_tokens"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if input.VLLMEndpoint != nil {
		_, _ = h.db.ExecContext(c.Request.Context(),
			`UPDATE models SET vllm_endpoint = $1, updated_at = NOW() WHERE id = $2`,
			*input.VLLMEndpoint, id)
	}
	if input.DisplayName != nil {
		_, _ = h.db.ExecContext(c.Request.Context(),
			`UPDATE models SET display_name = $1, updated_at = NOW() WHERE id = $2`,
			*input.DisplayName, id)
	}
	if input.MaxTokens != nil {
		_, _ = h.db.ExecContext(c.Request.Context(),
			`UPDATE models SET max_tokens = $1, updated_at = NOW() WHERE id = $2`,
			*input.MaxTokens, id)
	}

	c.JSON(http.StatusOK, gin.H{"message": "model updated"})
}

// DeactivateModel handles DELETE /admin/v1/models/:id
func (h *ModelHandler) DeactivateModel(c *gin.Context) {
	id := c.Param("id")
	res, err := h.db.ExecContext(c.Request.Context(),
		`UPDATE models SET active = FALSE, updated_at = NOW() WHERE id = $1`, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "model deactivated"})
}
