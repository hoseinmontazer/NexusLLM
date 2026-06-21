package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/controller"
	"github.com/nexusllm/nexusllm/internal/placement"
	"github.com/nexusllm/nexusllm/internal/runtime"
	"github.com/redis/go-redis/v9"
)

// RuntimeHandler manages the runtime lifecycle API.
//
// Correct flow for deploying a model:
//
//	POST /admin/v1/models/deploy
//	  1. Insert model row into DB
//	  2. Insert endpoint row into DB
//	  3. Start vLLM Docker container (controller)
//	  4. Reload registry → gateway starts routing
//
// All in one atomic API call. No seed data needed.
type RuntimeHandler struct {
	db        *sqlx.DB
	rdb       *redis.Client
	registry  *runtime.Registry
	ctrl      *controller.ModelController
	placement placement.Placer // optional; nil = manual GPU assignment only
}

// NewRuntimeHandler constructs a RuntimeHandler.
func NewRuntimeHandler(db *sqlx.DB, rdb *redis.Client, registry *runtime.Registry, ctrl *controller.ModelController) *RuntimeHandler {
	return &RuntimeHandler{db: db, rdb: rdb, registry: registry, ctrl: ctrl}
}

// WithPlacement attaches a placement engine to the handler, enabling the
// auto_place option on DeployModel.
func (h *RuntimeHandler) WithPlacement(p placement.Placer) *RuntimeHandler {
	h.placement = p
	return h
}

// ─── Deploy (register + start) ────────────────────────────────────────────────

// DeployModel handles POST /admin/v1/models/deploy
//
// This is the single entry point for adding a new model to the platform.
// It registers the model in the DB, allocates an endpoint, starts the
// vLLM container via Docker, and notifies the registry — all in one call.
func (h *RuntimeHandler) DeployModel(c *gin.Context) {
	var input struct {
		// Model identity
		Name        string   `json:"name"         binding:"required"` // "gemma-3-27b"
		DisplayName string   `json:"display_name" binding:"required"` // "Gemma 3 27B"
		Provider    string   `json:"provider"`                        // "google"
		BackendType string   `json:"backend_type"`                    // vllm | ollama | tgi
		MaxContext  int      `json:"max_context"`
		MaxOutput   int      `json:"max_output"`
		Tags        []string `json:"tags"`

		// Container / runtime
		Image          string  `json:"image"           binding:"required"` // "vllm/vllm-openai:v0.4.3"
		HFModelID      string  `json:"hf_model_id"`                        // "google/gemma-3-27b-it"
		Host           string  `json:"host"`                               // "localhost" (default)
		Port           int     `json:"port"            binding:"required"` // 8000
		GPUDevices     []int   `json:"gpu_devices"`                        // [0,1]
		TensorParallel int     `json:"tensor_parallel"`
		GPUMemoryUtil  float64 `json:"gpu_memory_util"`
		MaxModelLen    int     `json:"max_model_len"`
		Dtype          string  `json:"dtype"`
		Quantization   string  `json:"quantization"`
		ExtraArgs      []string `json:"extra_args"`
		HFToken        string  `json:"hf_token"`  // passed as HUGGING_FACE_HUB_TOKEN env var

		// Whether to actually start the container now (default: true)
		// Set false to register only and start manually later.
		StartNow *bool `json:"start_now"`

		// AutoPlace: if true, skip gpu_devices and let the placement engine
		// choose GPU(s), CPU affinity, and NUMA node automatically.
		AutoPlace  bool  `json:"auto_place"`
		MinVRAMMB  int64 `json:"min_vram_mb"` // used when auto_place=true
		MaxVRAMMB  int64 `json:"max_vram_mb"` // used when auto_place=true
		Priority   string `json:"priority"`   // used when auto_place=true
	}

	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Defaults
	if input.BackendType == "" {
		input.BackendType = "vllm"
	}
	if input.Provider == "" {
		input.Provider = "local"
	}
	if input.MaxContext == 0 {
		input.MaxContext = 8192
	}
	if input.MaxOutput == 0 {
		input.MaxOutput = 4096
	}
	if input.Host == "" {
		input.Host = "localhost"
	}
	if input.TensorParallel == 0 {
		input.TensorParallel = 1
	}
	if input.GPUMemoryUtil == 0 {
		input.GPUMemoryUtil = 0.90
	}
	startNow := true
	if input.StartNow != nil {
		startNow = *input.StartNow
	}

	// Use HF model ID as the served model name if provided, else use name
	modelID := input.HFModelID
	if modelID == "" {
		modelID = input.Name
	}

	// ── 1. Insert model row ────────────────────────────────────────────────
	mID := uuid.New().String()
	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO models
		  (id, name, display_name, provider, backend_type,
		   max_context, max_output, enabled, tags, vllm_endpoint)
		VALUES ($1,$2,$3,$4,$5,$6,$7,TRUE,$8,$9)`,
		mID, input.Name, input.DisplayName, input.Provider, input.BackendType,
		input.MaxContext, input.MaxOutput,
		tagsJSON(input.Tags),
		fmt.Sprintf("http://%s:%d", input.Host, input.Port),
	)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "model name already exists: " + err.Error()})
		return
	}

	// Default version
	_, _ = h.db.ExecContext(c.Request.Context(),
		`INSERT INTO model_versions (id, model_id, version, is_default) VALUES ($1,$2,'v1',TRUE)`,
		uuid.New().String(), mID)

	// Runtime config
	_, _ = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO model_runtime_configs
		  (id, model_id, gpu_memory_util, tensor_parallel, dtype, quantization)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		uuid.New().String(), mID,
		input.GPUMemoryUtil, input.TensorParallel,
		orDefault(input.Dtype, "auto"),
		nilableStr(input.Quantization),
	)

	// ── 2. Insert endpoint row ─────────────────────────────────────────────
	epID := uuid.New().String()
	runtimeImage := input.Image
	_, err = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO model_endpoints
		  (id, model_id, host, port, base_path, weight, priority,
		   health_status, is_enabled, lifecycle_state, runtime_image)
		VALUES ($1,$2,$3,$4,'/v1',100,1,'unknown',TRUE,'registered',$5)`,
		epID, mID, input.Host, input.Port, runtimeImage,
	)
	if err != nil {
		// Rollback model row
		_, _ = h.db.ExecContext(c.Request.Context(), `DELETE FROM models WHERE id = $1`, mID)
		c.JSON(http.StatusConflict, gin.H{"error": "endpoint conflict (host:port already used): " + err.Error()})
		return
	}

	// ── 3. Start container for all Docker-managed backends ──────────────────
	var containerID string
	// vllm, ollama, and tgi all support Docker deployment
	canDeploy := input.BackendType == "vllm" || input.BackendType == "ollama" || input.BackendType == "tgi"
	shouldStart := startNow && canDeploy && input.Image != ""

	// ── Auto-placement ────────────────────────────────────────────────────
	// If auto_place=true and a placement engine is wired, ask it to choose
	// GPUs for this model. The result overwrites input.GPUDevices.
	if input.AutoPlace && h.placement != nil {
		gpuCount := len(input.GPUDevices)
		if gpuCount == 0 {
			gpuCount = input.TensorParallel
			if gpuCount == 0 {
				gpuCount = 1
			}
		}
		pReq := placement.Request{
			ModelID:     mID,
			ModelName:   input.Name,
			ServiceType: "CHAT",
			RuntimeType: placement.RuntimeGPU,
			Priority:    placement.Priority(orDefault(input.Priority, "normal")),
			MinVRAMMB:   input.MinVRAMMB,
			MaxVRAMMB:   input.MaxVRAMMB,
			GPUCount:    gpuCount,
		}
		dec, placErr := h.placement.Decide(c.Request.Context(), pReq)
		if placErr != nil {
			// Non-fatal: proceed without auto-placement, log the issue
			_ = placErr
		} else {
			input.GPUDevices = dec.GPUDeviceIndices
			input.TensorParallel = len(dec.GPUDeviceIndices)
			_ = h.placement.Apply(c.Request.Context(), dec, pReq, epID)
		}
	}

	if shouldStart {
		env := map[string]string{}
		if input.HFToken != "" {
			env["HUGGING_FACE_HUB_TOKEN"] = input.HFToken
		}

		spec := controller.RuntimeSpec{
			ModelName:       modelID,       // HF model ID (e.g. "google/gemma-3-27b-it")
			ServedModelName: input.Name,    // short name for routing (e.g. "gemma-3-27b")
			Version:         "v1",
			EndpointID:      epID,
			BackendType:     input.BackendType,
			Image:           runtimeImage,
			BindHost:        input.Host,
			BindPort:        input.Port,
			GPUDevices:      input.GPUDevices,
			TensorParallel:  input.TensorParallel,
			GPUMemoryUtil:   input.GPUMemoryUtil,
			MaxModelLen:     input.MaxModelLen,
			Dtype:           input.Dtype,
			Quantization:    input.Quantization,
			ExtraArgs:       input.ExtraArgs,
			Env:             env,
		}

		containerID, err = h.ctrl.StartRaw(c.Request.Context(), epID, mID, spec, "admin")
		if err != nil {
			// Container failed to start — mark endpoint as failed but keep DB rows
			_, _ = h.db.ExecContext(c.Request.Context(),
				`UPDATE model_endpoints SET lifecycle_state = 'failed', updated_at = NOW() WHERE id = $1`, epID)
			c.JSON(http.StatusAccepted, gin.H{
				"model_id":    mID,
				"endpoint_id": epID,
				"warning":     "model registered but container failed to start: " + err.Error(),
				"next_step":   fmt.Sprintf("POST /admin/v1/models/%s/start?endpoint_id=%s", mID, epID),
			})
			return
		}
	}

	// ── 4. Reload registry ─────────────────────────────────────────────────
	_ = h.registry.Reload(c.Request.Context())

	resp := gin.H{
		"model_id":    mID,
		"model_name":  input.Name,
		"endpoint_id": epID,
		"host":        input.Host,
		"port":        input.Port,
		"started":     shouldStart && containerID != "",
		"status":      func() string {
			if shouldStart && containerID != "" { return "loading" }
			if input.BackendType == "ollama" || input.BackendType == "openai_compat" { return "active" }
			return "registered"
		}(),
		"note": func() string {
			if shouldStart && containerID != "" { return "" }
			if input.BackendType == "ollama" && !shouldStart {
				return fmt.Sprintf("Ollama backend registered as external. Make sure Ollama is running on %s:%d", input.Host, input.Port)
			}
			if !shouldStart {
				return "Model registered. Use POST /admin/v1/models/:id/start?endpoint_id=" + epID + " to start the container."
			}
			return ""
		}(),
	}
	if containerID != "" {
		resp["container_id"] = containerID
	}
	c.JSON(http.StatusCreated, resp)
}

// ─── Simple model registration (no container) ─────────────────────────────────

// RegisterModel handles POST /admin/v1/models
// Registers an already-running external model (e.g. Ollama, TGI, remote API).
// Use DeployModel for vLLM containers managed by NexusLLM.
func (h *RuntimeHandler) RegisterModel(c *gin.Context) {
	var input struct {
		Name        string   `json:"name"         binding:"required"`
		DisplayName string   `json:"display_name" binding:"required"`
		Provider    string   `json:"provider"`
		BackendType string   `json:"backend_type" binding:"required"`
		Host        string   `json:"host"         binding:"required"`
		Port        int      `json:"port"         binding:"required"`
		MaxContext  int      `json:"max_context"`
		MaxOutput   int      `json:"max_output"`
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

	mID := uuid.New().String()
	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO models
		  (id, name, display_name, provider, backend_type,
		   max_context, max_output, enabled, tags, vllm_endpoint)
		VALUES ($1,$2,$3,$4,$5,$6,$7,TRUE,$8,$9)`,
		mID, input.Name, input.DisplayName, input.Provider, input.BackendType,
		input.MaxContext, input.MaxOutput,
		tagsJSON(input.Tags),
		fmt.Sprintf("http://%s:%d", input.Host, input.Port),
	)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "model name already exists: " + err.Error()})
		return
	}

	_, _ = h.db.ExecContext(c.Request.Context(),
		`INSERT INTO model_versions (id, model_id, version, is_default) VALUES ($1,$2,'v1',TRUE)`,
		uuid.New().String(), mID)

	epID := uuid.New().String()
	_, _ = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO model_endpoints
		  (id, model_id, host, port, base_path, weight, priority,
		   health_status, is_enabled, lifecycle_state)
		VALUES ($1,$2,$3,$4,'/v1',100,1,'unknown',TRUE,'active')`,
		epID, mID, input.Host, input.Port,
	)

	_ = h.registry.Reload(c.Request.Context())
	c.JSON(http.StatusCreated, gin.H{
		"model_id":    mID,
		"model_name":  input.Name,
		"endpoint_id": epID,
		"note":        "registered as external model — NexusLLM will not manage its container lifecycle",
	})
}

// ─── Endpoint management ──────────────────────────────────────────────────────

// AddEndpoint handles POST /admin/v1/models/:id/endpoints
func (h *RuntimeHandler) AddEndpoint(c *gin.Context) {
	modelID := c.Param("id")
	var input struct {
		Host     string `json:"host"  binding:"required"`
		Port     int    `json:"port"  binding:"required"`
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

// ─── Lifecycle operations ─────────────────────────────────────────────────────

func (h *RuntimeHandler) DrainModel(c *gin.Context) {
	modelID := c.Param("id")
	_, _ = h.db.ExecContext(c.Request.Context(), `
		UPDATE model_endpoints SET health_status = 'draining', updated_at = NOW()
		WHERE model_id = $1 AND is_enabled = TRUE`, modelID)
	_ = h.registry.Reload(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"message": "model draining"})
}

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

func (h *RuntimeHandler) DisableModel(c *gin.Context) {
	modelID := c.Param("id")
	_, _ = h.db.ExecContext(c.Request.Context(),
		`UPDATE models SET enabled = FALSE, updated_at = NOW() WHERE id = $1`, modelID)
	_ = h.registry.Reload(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"message": "model disabled"})
}

func (h *RuntimeHandler) UpdateRuntimeConfig(c *gin.Context) {
	modelID := c.Param("id")
	var input struct {
		GPUMemoryUtil  *float64 `json:"gpu_memory_util"`
		TensorParallel *int     `json:"tensor_parallel"`
		DType          *string  `json:"dtype"`
		Quantization   *string  `json:"quantization"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.GPUMemoryUtil != nil {
		_, _ = h.db.ExecContext(c.Request.Context(),
			`UPDATE model_runtime_configs SET gpu_memory_util=$1, updated_at=NOW() WHERE model_id=$2`,
			*input.GPUMemoryUtil, modelID)
	}
	if input.TensorParallel != nil {
		_, _ = h.db.ExecContext(c.Request.Context(),
			`UPDATE model_runtime_configs SET tensor_parallel=$1, updated_at=NOW() WHERE model_id=$2`,
			*input.TensorParallel, modelID)
	}
	if input.DType != nil {
		_, _ = h.db.ExecContext(c.Request.Context(),
			`UPDATE model_runtime_configs SET dtype=$1, updated_at=NOW() WHERE model_id=$2`,
			*input.DType, modelID)
	}
	c.JSON(http.StatusOK, gin.H{"message": "runtime config updated"})
}

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
	c.JSON(http.StatusOK, gin.H{"message": "pool strategy updated"})
}

func (h *RuntimeHandler) GetModelHealth(c *gin.Context) {
	modelID := c.Param("id")
	type epRow struct {
		ID                  string     `db:"id"                   json:"id"`
		Host                string     `db:"host"                 json:"host"`
		Port                int        `db:"port"                 json:"port"`
		HealthStatus        string     `db:"health_status"        json:"health_status"`
		LifecycleState      string     `db:"lifecycle_state"      json:"lifecycle_state"`
		ContainerID         string     `db:"container_id"         json:"container_id"`
		ConsecutiveFailures int        `db:"consecutive_failures" json:"consecutive_failures"`
		ResponseTimeMs      *int       `db:"response_time_ms"     json:"response_time_ms"`
		LastCheckedAt       *time.Time `db:"last_checked_at"      json:"last_checked_at"`
	}
	rows := make([]epRow, 0)
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT id, host, port, health_status, lifecycle_state, container_id,
		       consecutive_failures, response_time_ms, last_checked_at
		FROM model_endpoints WHERE model_id = $1 ORDER BY priority`, modelID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if rows == nil {
		rows = make([]epRow, 0)
	}
	// Return 200 with empty endpoints list instead of 404
	c.JSON(http.StatusOK, gin.H{"model_id": modelID, "endpoints": rows, "count": len(rows)})
}

func (h *RuntimeHandler) ListModels(c *gin.Context) {
	type mRow struct {
		ID          string `db:"id"           json:"id"`
		Name        string `db:"name"         json:"name"`
		DisplayName string `db:"display_name" json:"display_name"`
		Provider    string `db:"provider"     json:"provider"`
		BackendType string `db:"backend_type" json:"backend_type"`
		MaxContext  int    `db:"max_context"  json:"max_context"`
		MaxOutput   int    `db:"max_output"   json:"max_output"`
		Enabled     bool   `db:"enabled"      json:"enabled"`
		EndpointCnt int    `db:"endpoint_cnt" json:"endpoint_count"`
		HealthyCnt  int    `db:"healthy_cnt"  json:"healthy_count"`
	}
	rows := make([]mRow, 0)
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT m.id, m.name, m.display_name, m.provider, m.backend_type,
		       m.max_context, m.max_output, m.enabled,
		       COUNT(me.id) AS endpoint_cnt,
		       COUNT(me.id) FILTER (WHERE me.health_status='healthy') AS healthy_cnt
		FROM models m
		LEFT JOIN model_endpoints me ON me.model_id = m.id AND me.is_enabled = TRUE
		GROUP BY m.id ORDER BY m.name`); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if rows == nil {
		rows = make([]mRow, 0)
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

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func nilableStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// ─── DeleteModel ──────────────────────────────────────────────────────────────

// DeleteModel handles DELETE /admin/v1/models/:id
// Removes the model and all associated endpoints from the DB.
// Does NOT stop running containers — call stop/drain first.
func (h *RuntimeHandler) DeleteModel(c *gin.Context) {
	modelID := c.Param("id")
	res, err := h.db.ExecContext(c.Request.Context(),
		`DELETE FROM models WHERE id = $1`, modelID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
		return
	}
	_ = h.registry.Reload(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"message": "model deleted", "model_id": modelID})
}

// GetDeployStatus handles GET /admin/v1/models/:id/deploy-status
// Returns the current lifecycle state of all endpoints for a model.
func (h *RuntimeHandler) GetDeployStatus(c *gin.Context) {
	modelID := c.Param("id")
	type statusRow struct {
		EndpointID     string     `db:"id"              json:"endpoint_id"`
		Host           string     `db:"host"            json:"host"`
		Port           int        `db:"port"            json:"port"`
		LifecycleState string     `db:"lifecycle_state" json:"lifecycle_state"`
		HealthStatus   string     `db:"health_status"   json:"health_status"`
		ContainerID    string     `db:"container_id"    json:"container_id"`
		RuntimeImage   string     `db:"runtime_image"   json:"runtime_image"`
		UpdatedAt      time.Time  `db:"updated_at"      json:"updated_at"`
	}
	var rows []statusRow
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT id, host, port, lifecycle_state, health_status,
		       COALESCE(container_id,'') AS container_id,
		       COALESCE(runtime_image,'') AS runtime_image,
		       updated_at
		FROM model_endpoints
		WHERE model_id = $1
		ORDER BY priority`, modelID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if rows == nil {
		rows = []statusRow{}
	}
	c.JSON(http.StatusOK, gin.H{
		"model_id":  modelID,
		"endpoints": rows,
		"count":     len(rows),
	})
}

// ─── Health reset ─────────────────────────────────────────────────────────────

// ResetHealth handles POST /admin/v1/models/:id/reset-health
// Clears failed/unknown health state so the watcher can re-evaluate.
// This is a recovery tool — it doesn't actually contact the backend.
func (h *RuntimeHandler) ResetHealth(c *gin.Context) {
	modelID := c.Param("id")
	epID := c.Query("endpoint_id") // optional — reset specific endpoint or all

	query := `UPDATE model_endpoints
	          SET health_status = 'unknown',
	              lifecycle_state = CASE
	                WHEN lifecycle_state = 'failed' THEN 'active'
	                ELSE lifecycle_state
	              END,
	              consecutive_failures = 0,
	              updated_at = NOW()
	          WHERE model_id = $1`
	args := []interface{}{modelID}

	if epID != "" {
		query += " AND id = $2"
		args = append(args, epID)
	}

	res, err := h.db.ExecContext(c.Request.Context(), query, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	rows, _ := res.RowsAffected()
	_ = h.registry.Reload(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{
		"message":           "health state reset — watcher will re-check on next tick",
		"endpoints_updated": rows,
	})
}

// ImportOllamaModels handles POST /admin/v1/models/import-ollama
// Queries a running Ollama instance and bulk-registers all models it has loaded.
// Already-registered models (by name) are skipped — safe to call repeatedly.
func (h *RuntimeHandler) ImportOllamaModels(c *gin.Context) {
	var input struct {
		Host string `json:"host"` // default: localhost
		Port int    `json:"port"` // default: 11434
	}
	_ = c.ShouldBindJSON(&input)
	if input.Host == "" {
		input.Host = "localhost"
	}
	if input.Port == 0 {
		input.Port = 11434
	}

	// Query /api/tags from the running Ollama instance
	ollamaURL := fmt.Sprintf("http://%s:%d/api/tags", input.Host, input.Port)
	resp, err := http.Get(ollamaURL) //nolint:gosec // internal admin call
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": fmt.Sprintf("could not reach Ollama at %s:%d — is it running? (%s)",
				input.Host, input.Port, err.Error()),
		})
		return
	}
	defer resp.Body.Close()

	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to parse Ollama response: " + err.Error()})
		return
	}

	type result struct {
		Name    string `json:"name"`
		Status  string `json:"status"`
		ModelID string `json:"model_id,omitempty"`
	}
	var results []result

	for _, m := range payload.Models {
		// Skip if already registered
		var existingID string
		err := h.db.QueryRowContext(c.Request.Context(),
			`SELECT id FROM models WHERE name = $1`, m.Name).Scan(&existingID)
		if err == nil {
			results = append(results, result{Name: m.Name, Status: "already_registered", ModelID: existingID})
			continue
		}

		// Register the model
		mID := uuid.New().String()
		_, err = h.db.ExecContext(c.Request.Context(), `
			INSERT INTO models
			  (id, name, display_name, provider, backend_type, service_type,
			   max_context, max_output, enabled, tags, vllm_endpoint)
			VALUES ($1,$2,$3,'local','ollama','CHAT',8192,4096,TRUE,'[]',$4)`,
			mID, m.Name, m.Name,
			fmt.Sprintf("http://%s:%d", input.Host, input.Port),
		)
		if err != nil {
			results = append(results, result{Name: m.Name, Status: "error: " + err.Error()})
			continue
		}

		_, _ = h.db.ExecContext(c.Request.Context(),
			`INSERT INTO model_versions (id, model_id, version, is_default) VALUES ($1,$2,'v1',TRUE)`,
			uuid.New().String(), mID)

		epID := uuid.New().String()
		_, _ = h.db.ExecContext(c.Request.Context(), `
			INSERT INTO model_endpoints
			  (id, model_id, host, port, base_path, weight, priority,
			   health_status, is_enabled, lifecycle_state, runtime_image, runtime_type)
			VALUES ($1,$2,$3,$4,'/v1',100,1,'unknown',TRUE,'active','ollama/ollama:latest','CPU_RUNTIME')`,
			epID, mID, input.Host, input.Port,
		)

		results = append(results, result{Name: m.Name, Status: "registered", ModelID: mID})
	}

	_ = h.registry.Reload(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{
		"host":    input.Host,
		"port":    input.Port,
		"results": results,
		"total":   len(results),
	})
}
