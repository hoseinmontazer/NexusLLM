package handlers

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// GetPricing handles GET /admin/v1/models/:id/pricing
// Returns the currently active (effective_to IS NULL) pricing row for the model.
func (h *RuntimeHandler) GetPricing(c *gin.Context) {
	modelID := c.Param("id")

	var row struct {
		ID             string `db:"id"               json:"id"`
		ModelID        string `db:"model_id"         json:"model_id"`
		InputPerToken  string `db:"input_per_token"  json:"input_per_token"`
		OutputPerToken string `db:"output_per_token" json:"output_per_token"`
		CachedPerToken string `db:"cached_per_token" json:"cached_per_token"`
		Currency       string `db:"currency"         json:"currency"`
		EffectiveFrom  string `db:"effective_from"   json:"effective_from"`
		CreatedBy      string `db:"created_by"       json:"created_by"`
	}
	err := h.db.GetContext(c.Request.Context(), &row, `
		SELECT id::text, model_id::text, input_per_token::text, output_per_token::text,
		       cached_per_token::text, currency, effective_from::text, created_by
		FROM model_pricing
		WHERE model_id = $1::uuid AND effective_to IS NULL`, modelID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "no pricing configured for model " + modelID})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, row)
}

// SetPricing handles PUT /admin/v1/models/:id/pricing
//
// model_pricing is append-only and versioned: the previously active row (if
// any) is closed out with effective_to=NOW() and a new row is inserted, so
// past usage keeps referencing the rate that was actually in effect when it
// ran. This mirrors what OpenRouter/cloud providers report as usage.cost —
// once a rate is set here, the gateway starts including a computed
// usage.cost/usage.cost_details on every response for this model (see
// Handler.applyEstimatedCost in internal/proxy).
func (h *RuntimeHandler) SetPricing(c *gin.Context) {
	modelID := c.Param("id")

	var req struct {
		InputPerToken  float64 `json:"input_per_token"`
		OutputPerToken float64 `json:"output_per_token"`
		CachedPerToken float64 `json:"cached_per_token"`
		Currency       string  `json:"currency"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.InputPerToken < 0 || req.OutputPerToken < 0 || req.CachedPerToken < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "per-token rates must be >= 0"})
		return
	}
	if req.Currency == "" {
		req.Currency = "USD"
	}

	tx, err := h.db.BeginTxx(c.Request.Context(), nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(c.Request.Context(), `
		UPDATE model_pricing SET effective_to = NOW()
		WHERE model_id = $1::uuid AND effective_to IS NULL`, modelID,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var newID string
	err = tx.QueryRowContext(c.Request.Context(), `
		INSERT INTO model_pricing (model_id, input_per_token, output_per_token, cached_per_token, currency, created_by)
		VALUES ($1::uuid, $2, $3, $4, $5, 'admin')
		RETURNING id::text`,
		modelID, req.InputPerToken, req.OutputPerToken, req.CachedPerToken, req.Currency,
	).Scan(&newID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to set pricing (is " + modelID + " a valid model id?): " + err.Error()})
		return
	}

	if err := tx.Commit(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "pricing updated", "id": newID, "model_id": modelID})
}
