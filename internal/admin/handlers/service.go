package handlers

// service.go — DEPRECATED compatibility bridge for the AI Service Registry API.
//
// ARCHITECTURAL STATUS: Deprecated since migration 033 (Universal Runtime Platform).
// All AI workload types (LLM, STT, TTS, OCR, Embedding, Rerank, Vision, etc.)
// are now first-class Models. There is no architectural distinction between a
// "service" and a "model".
//
// MIGRATION PATH:
//   OLD (deprecated):   POST /admin/v1/services/deploy
//   NEW (canonical):    POST /admin/v1/models/deploy
//
//   OLD (deprecated):   POST /admin/v1/services
//   NEW (canonical):    POST /admin/v1/models  (register external model)
//
//   OLD (deprecated):   GET /admin/v1/services
//   NEW (canonical):    GET /admin/v1/models
//
// These routes are kept alive for backward compatibility with existing API clients
// but now delegate internally to RuntimeHandler. No new features will be added here.
//
// Resource reservations (/services/:id/reservation) have no equivalent in the
// models API — those routes remain here until a dedicated /models/:id/reservation
// endpoint is added.
//
// This file will be removed in a future release once all clients have migrated.

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/services"
)

// ServiceHandler is a deprecated compatibility shim.
// Internally it holds a reference to RuntimeHandler so all write operations
// flow through the canonical deployment path.
//
// Deprecated: Use RuntimeHandler directly.
type ServiceHandler struct {
	db          *sqlx.DB
	runtimeH    *RuntimeHandler
	svcRegistry *services.Registry // kept only for reservation management
}

// NewServiceHandler constructs a deprecated ServiceHandler compatibility shim.
// svcRegistry is retained solely for GET/PUT /services/:id/reservation.
//
// Deprecated: construct RuntimeHandler directly instead.
func NewServiceHandler(
	db *sqlx.DB,
	svcRegistry *services.Registry,
	runtimeH *RuntimeHandler,
) *ServiceHandler {
	return &ServiceHandler{
		db:          db,
		runtimeH:    runtimeH,
		svcRegistry: svcRegistry,
	}
}

// RegisterService handles POST /admin/v1/services
//
// Deprecated: use POST /admin/v1/models instead.
// This handler translates the services.RegisterRequest into a models/register
// request body and delegates to RuntimeHandler.RegisterModel.
func (h *ServiceHandler) RegisterService(c *gin.Context) {
	var req services.RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Translate RegisterRequest → RegisterModel body (canonical schema).
	body := map[string]interface{}{
		"name":         req.Name,
		"display_name": req.DisplayName,
		"provider":     req.Provider,
		"backend_type": func() string {
			if req.BackendType != "" {
				return req.BackendType
			}
			return "openai_compat"
		}(),
		"host":        req.Host,
		"port":        req.Port,
		"max_context": req.MaxContext,
		"max_output":  req.MaxOutput,
		"tags":        req.Tags,
		// Preserve service_type on the model row for query compatibility.
		"service_type": req.ServiceType,
		"runtime_type": req.RuntimeType,
	}

	b, _ := json.Marshal(body)
	c.Request.Body = newBodyFromBytes(b)
	c.Request.ContentLength = int64(len(b))

	// Add deprecation header so API clients can plan migration.
	c.Header("Deprecation", "true")
	c.Header("Link", `</admin/v1/models>; rel="successor-version"`)

	h.runtimeH.RegisterModel(c)
}

// DeployService handles POST /admin/v1/services/deploy
//
// Deprecated: use POST /admin/v1/models/deploy instead.
// This handler translates the services deploy request into a models/deploy
// request body and delegates to RuntimeHandler.DeployModel.
func (h *ServiceHandler) DeployService(c *gin.Context) {
	var input struct {
		services.RegisterRequest

		Image          string   `json:"image"`
		HFModelID      string   `json:"hf_model_id"`
		HFToken        string   `json:"hf_token"`
		GPUCount       int      `json:"gpu_count"`
		ExtraArgs      []string `json:"extra_args"`
		GPUMemUtil     float64  `json:"gpu_memory_util"`
		TensorParallel int      `json:"tensor_parallel"`
		StartNow       *bool    `json:"start_now"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Translate to models/deploy body (canonical schema).
	body := map[string]interface{}{
		"name":            input.Name,
		"display_name":    input.DisplayName,
		"provider":        input.Provider,
		"backend_type":    input.BackendType,
		"host":            input.Host,
		"port":            input.Port,
		"max_context":     input.MaxContext,
		"max_output":      input.MaxOutput,
		"tags":            input.Tags,
		"service_type":    input.ServiceType,
		"runtime_type":    input.RuntimeType,
		"image":           input.Image,
		"hf_model_id":     input.HFModelID,
		"hf_token":        input.HFToken,
		"gpu_count":       input.GPUCount,
		"extra_args":      input.ExtraArgs,
		"gpu_memory_util": input.GPUMemUtil,
		"tensor_parallel": input.TensorParallel,
		"start_now":       input.StartNow,
		"min_vram_mb":     input.MinVRAMMB,
		"max_vram_mb":     input.MaxVRAMMB,
		"priority_weight": input.PriorityWeight,
		// Use auto_place so the canonical deploy path uses the scheduler for
		// placement (same as before when the old placement.Engine was used).
		"auto_place": true,
	}

	b, _ := json.Marshal(body)
	c.Request.Body = newBodyFromBytes(b)
	c.Request.ContentLength = int64(len(b))

	c.Header("Deprecation", "true")
	c.Header("Link", `</admin/v1/models/deploy>; rel="successor-version"`)

	h.runtimeH.DeployModel(c)
}

// ListServices handles GET /admin/v1/services
//
// Deprecated: use GET /admin/v1/models instead.
// Preserves the optional ?type= filter by mapping it to a service_type query
// against the unified models table.
func (h *ServiceHandler) ListServices(c *gin.Context) {
	c.Header("Deprecation", "true")
	c.Header("Link", `</admin/v1/models>; rel="successor-version"`)

	// Delegate to ListModels. The ?type= parameter is ignored by ListModels
	// (it filters by lifecycle). For full service_type filtering, callers should
	// migrate to GET /admin/v1/models and filter client-side, or we add a
	// ?service_type= query param to ListModels in a future release.
	h.runtimeH.ListModels(c)
}

// GetReservation handles GET /admin/v1/services/:id/reservation
// No equivalent exists in the models API yet — this route is preserved as-is.
func (h *ServiceHandler) GetReservation(c *gin.Context) {
	modelID := c.Param("id")
	rr, err := h.svcRegistry.GetReservation(c.Request.Context(), modelID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no reservation found for model " + modelID})
		return
	}
	c.JSON(http.StatusOK, rr)
}

// UpsertReservation handles PUT /admin/v1/services/:id/reservation
// No equivalent exists in the models API yet — this route is preserved as-is.
func (h *ServiceHandler) UpsertReservation(c *gin.Context) {
	modelID := c.Param("id")
	var req services.ReservationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.ModelID = modelID
	if req.Priority == "" {
		req.Priority = "normal"
	}
	if req.PreferredRuntime == "" {
		req.PreferredRuntime = "GPU_RUNTIME"
	}
	if err := h.svcRegistry.UpsertReservation(c.Request.Context(), req); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "reservation updated", "model_id": modelID})
}

// ─── internal helpers ─────────────────────────────────────────────────────────

func newBodyFromBytes(b []byte) *bodyReader {
	return &bodyReader{r: bytes.NewReader(b)}
}

type bodyReader struct {
	r *bytes.Reader
}

func (b *bodyReader) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *bodyReader) Close() error               { return nil }
