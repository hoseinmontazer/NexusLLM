package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/runtime"
)

// RuntimeHandler manages the runtime lifecycle API:
//
//	POST   /admin/v1/models                         register a model
//	POST   /admin/v1/models/:id/endpoints           add an endpoint to a model pool
//	DELETE /admin/v1/models/:id/endpoints/:ep_id    remove an endpoint
//	POST   /admin/v1/models/:id/drain               gracefully stop new traffic
//	POST   /admin/v1/models/:id/enable              re-enable after drain/disable
//	POST   /admin/v1/models/:id/disable             hard-disable (no traffic)
//	PUT    /admin/v1/models/:id/pool-strategy       change routing strategy
//	GET    /admin/v1/models/:id/health              live health snapshot
type RuntimeHandler struct {
	db       *sqlx.DB
	registry *runtime.Registry
}

// NewRuntimeHandler constructs a RuntimeHandler.
func NewRuntimeHandler(db *sqlx.DB, registry *runtime.Registry) *RuntimeHandler {
	return &RuntimeHandler{db: db, registry: registry}
}

// ─── Model registration ───────────────────────────────────────────────────────

// RegisterModel handles POST /admin/v1/models
func (h *RuntimeHandler) RegisterModel(c *gin.Context) {
	var input struct {
		Name        string `json:"name"         binding:"required"`
		DisplayName string `json:"display_name" binding:"required"`
		Provider    string `json:"provider"`
		BackendType string `json:"backend_type" binding:"required"`
		MaxContext  int    `json:"max_context"`
		MaxOutput   int    `json:"max_output"`
		Tags        []string `json:"tags"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.MaxContext == 0 {
		input.MaxContext = 8192
	}
	if input.MaxOutput == 0 {
		input.MaxOutput = 4096
	}
	if input.Provider == "" {
		input.Provider = "local"
	}

	id := uuid.New().String()
	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO models (id, name, display_name, provider, backend_type, max_context, max_output, enabled, tags)
		VALUES ($1,$2,$3,$4,$5,$6,$7,TRUE,$8)`,
		id, input.Name, input.DisplayName, input.Provider,
		input.BackendType, input.MaxContext, input.MaxOutput,
		tagsJSON(input.Tags),
	)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "model name already exists or DB error: " + err.Error()})
		return
	}

	// Create a default version.
	versionID := uuid.New().String()
	_, _ = h.db.ExecContext(c.Request.Context(),
		`INSERT INTO model_versions (id, model_id, version, is_default) VALUES ($1,$2,'v1',TRUE)`,
		versionID, id,
	)

	// Seed a default runtime config.
	_, _ = h.db.ExecContext(c.Request.Context(),
		`INSERT INTO model_runtime_configs (id, model_id) VALUES ($1,$2)`,
		uuid.New().String(), id,
	)

	_ = h.registry.Reload(c.Request.Context())
	c.JSON(http.StatusCreated, gin.H{"id": id, "name": input.Name})
}

// ─── Endpoint management ──────────────────────────────────────────────────────

// AddEndpoint handles POST /admin/v1/models/:id/endpoints
func (h *RuntimeHandler) AddEndpoint(c *gin.Context) {
	modelID := c.Param("id")
	var input struct {
		Host     string `json:"host"      binding:"required"`
		Port     int    `json:"port"      binding:"required"`
		BasePath string `json:"base_path"`
		Weight   int    `json:"weight"`
		Priority int    `json:"priority"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.BasePath == "" {
		input.BasePath = "/v1"
	}
	if input.Weight == 0 {
		input.Weight = 100
	}
	if input.Priority == 0 {
		input.Priority = 1
	}

	epID := uuid.New().String()
	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO model_endpoints
		  (id, model_id, host, port, base_path, weight, priority, health_status, is_enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'unknown',TRUE)`,
		epID, modelID, input.Host, input.Port, input.BasePath, input.Weight, input.Priority,
	)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "endpoint already exists: " + err.Error()})
		return
	}

	_ = h.registry.Reload(c.Request.Context())
	c.JSON(http.StatusCreated, gin.H{"id": epID, "model_id": modelID, "host": input.Host, "port": input.Port})
}

// RemoveEndpoint handles DELETE /admin/v1/models/:id/endpoints/:ep_id
func (h *RuntimeHandler) RemoveEndpoint(c *gin.Context) {
	modelID := c.Param("id")
	epID := c.Param("ep_id")

	res, err := h.db.ExecContext(c.Request.Context(),
		`DELETE FROM model_endpoints WHERE id = $1 AND model_id = $2`, epID, modelID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "endpoint not found"})
		return
	}
	_ = h.registry.Reload(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"message": "endpoint removed"})
}

// ─── Runtime lifecycle ────────────────────────────────────────────────────────

// DrainModel handles POST /admin/v1/models/:id/drain
// Marks all endpoints as "draining" — no new requests, in-flight finish.
func (h *RuntimeHandler) DrainModel(c *gin.Context) {
	modelID := c.Param("id")
	_, err := h.db.ExecContext(c.Request.Context(), `
		UPDATE model_endpoints SET health_status = 'draining', updated_at = NOW()
		WHERE model_id = $1 AND is_enabled = TRUE`, modelID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_ = h.registry.Reload(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"message": "model draining — no new requests accepted"})
}

// EnableModel handles POST /admin/v1/models/:id/enable
func (h *RuntimeHandler) EnableModel(c *gin.Context) {
	modelID := c.Param("id")
	_, _ = h.db.ExecContext(c.Request.Context(),
		`UPDATE models SET enabled = TRUE, updated_at = NOW() WHERE id = $1`, modelID)
	_, _ = h.db.ExecContext(c.Request.Context(), `
		UPDATE model_endpoints SET is_enabled = TRUE, health_status = 'unknown', updated_at = NOW()
		WHERE model_id = $1`, modelID)
	_ = h.registry.Reload(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"message": "model enabled"})
}

// DisableModel handles POST /admin/v1/models/:id/disable
func (h *RuntimeHandler) DisableModel(c *gin.Context) {
	modelID := c.Param("id")
	_, _ = h.db.ExecContext(c.Request.Context(),
		`UPDATE models SET enabled = FALSE, updated_at = NOW() WHERE id = $1`, modelID)
	_ = h.registry.Reload(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"message": "model disabled — removed from routing"})
}

// UpdateRuntimeConfig handles PUT /admin/v1/models/:id/runtime-config
func (h *RuntimeHandler) UpdateRuntimeConfig(c *gin.Context) {
	modelID := c.Param("id")
	var input struct {
		GPUMemoryUtil *float64 `json:"gpu_memory_util"`
		TensorParallel *int    `json:"tensor_parallel"`
		MaxBatchSize   *int    `json:"max_batch_size"`
		DType          *string `json:"dtype"`
		Quantization   *string `json:"quantization"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if input.GPUMemoryUtil != nil {
		_, _ = h.db.ExecContext(c.Request.Context(),
			`UPDATE model_runtime_configs SET gpu_memory_util = $1, updated_at = NOW() WHERE model_id = $2`,
			*input.GPUMemoryUtil, modelID)
	}
	if input.TensorParallel != nil {
		_, _ = h.db.ExecContext(c.Request.Context(),
			`UPDATE model_runtime_configs SET tensor_parallel = $1, updated_at = NOW() WHERE model_id = $2`,
			*input.TensorParallel, modelID)
	}
	if input.DType != nil {
		_, _ = h.db.ExecContext(c.Request.Context(),
			`UPDATE model_runtime_configs SET dtype = $1, updated_at = NOW() WHERE model_id = $2`,
			*input.DType, modelID)
	}

	c.JSON(http.StatusOK, gin.H{"message": "runtime config updated"})
}

// UpdatePoolStrategy handles PUT /admin/v1/models/:id/pool-strategy
func (h *RuntimeHandler) UpdatePoolStrategy(c *gin.Context) {
	var input struct {
		ModelName string `json:"model_name" binding:"required"`
		Strategy  string `json:"strategy"   binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.registry.SetPoolStrategy(input.ModelName, runtime.RoutingStrategy(input.Strategy)); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "pool strategy updated", "strategy": input.Strategy})
}

// GetModelHealth handles GET /admin/v1/models/:id/health
// Returns a live snapshot of all endpoints for this model.
func (h *RuntimeHandler) GetModelHealth(c *gin.Context) {
	modelID := c.Param("id")

	type endpointRow struct {
		ID                  string     `db:"id"                   json:"id"`
		Host                string     `db:"host"                 json:"host"`
		Port                int        `db:"port"                 json:"port"`
		HealthStatus        string     `db:"health_status"        json:"health_status"`
		ConsecutiveFailures int        `db:"consecutive_failures" json:"consecutive_failures"`
		ResponseTimeMs      *int       `db:"response_time_ms"     json:"response_time_ms"`
		LastCheckedAt       *time.Time `db:"last_checked_at"      json:"last_checked_at"`
		LastSuccessAt       *time.Time `db:"last_success_at"      json:"last_success_at"`
		Weight              int        `db:"weight"               json:"weight"`
		Priority            int        `db:"priority"             json:"priority"`
	}

	var rows []endpointRow
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT id, host, port, health_status, consecutive_failures,
		       response_time_ms, last_checked_at, last_success_at, weight, priority
		FROM model_endpoints
		WHERE model_id = $1
		ORDER BY priority, weight DESC`, modelID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if len(rows) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "model not found or has no endpoints"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"model_id": modelID, "endpoints": rows, "total": len(rows)})
}

// ListModels handles GET /admin/v1/models (enhanced)
func (h *RuntimeHandler) ListModels(c *gin.Context) {
	type modelRow struct {
		ID          string `db:"id"           json:"id"`
		Name        string `db:"name"         json:"name"`
		DisplayName string `db:"display_name" json:"display_name"`
		Provider    string `db:"provider"     json:"provider"`
		BackendType string `db:"backend_type" json:"backend_type"`
		MaxContext  int    `db:"max_context"  json:"max_context"`
		MaxOutput   int    `db:"max_output"   json:"max_output"`
		Enabled     bool   `db:"enabled"      json:"enabled"`
		Tags        []byte `db:"tags"         json:"-"`
		EndpointCnt int    `db:"endpoint_cnt" json:"endpoint_count"`
		HealthyCnt  int    `db:"healthy_cnt"  json:"healthy_count"`
	}
	var rows []modelRow
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT
			m.id, m.name, m.display_name, m.provider, m.backend_type,
			m.max_context, m.max_output, m.enabled, m.tags,
			COUNT(me.id)                                          AS endpoint_cnt,
			COUNT(me.id) FILTER (WHERE me.health_status='healthy') AS healthy_cnt
		FROM models m
		LEFT JOIN model_endpoints me ON me.model_id = m.id AND me.is_enabled = TRUE
		GROUP BY m.id
		ORDER BY m.name`); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": len(rows)})
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func tagsJSON(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	b := `[`
	for i, t := range tags {
		if i > 0 {
			b += ","
		}
		b += `"` + t + `"`
	}
	return b + `]`
}
