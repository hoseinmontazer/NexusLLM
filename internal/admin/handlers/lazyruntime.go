package handlers

// lazyruntime.go — Admin API handlers for lazy-load runtime configuration.
//
// Routes:
//   PUT  /admin/v1/models/:id/lazy-config   — set GGUF source + idle timeout
//   GET  /admin/v1/models/:id/lazy-config   — read current config
//   GET  /admin/v1/models/:id/runtime-status — live state of the model container

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

// LazyRuntimeHandler manages lazy-load configuration for models.
type LazyRuntimeHandler struct {
	db *sqlx.DB
}

// NewLazyRuntimeHandler constructs a LazyRuntimeHandler.
func NewLazyRuntimeHandler(db *sqlx.DB) *LazyRuntimeHandler {
	return &LazyRuntimeHandler{db: db}
}

// SetLazyConfig handles PUT /admin/v1/models/:id/lazy-config
//
// Stores the llama.cpp source (GGUF path or HF repo) and idle timeout in
// model_runtime_configs. The runtime manager reads these on every EnsureRunning call.
func (h *LazyRuntimeHandler) SetLazyConfig(c *gin.Context) {
	modelID := c.Param("id")
	var input struct {
		// Model source — first non-empty field wins at runtime start.
		// Option A: local GGUF already on the shared volume.
		GGUFPath string `json:"gguf_path"` // e.g. "/models/gemma-2-2b-it-Q4_K_M.gguf"
		// Option B: download directly from a pre-quantized HF GGUF repo.
		HFRepo  string `json:"hf_repo"`  // e.g. "bartowski/gemma-2-2b-it-GGUF"
		HFFile  string `json:"hf_file"`  // e.g. "gemma-2-2b-it-Q4_K_M.gguf"
		HFToken string `json:"hf_token"` // for gated repos (stored encrypted in prod)

		// llama-server flags
		CtxSize    int    `json:"ctx_size"`      // default: 4096
		NGPULayers int    `json:"n_gpu_layers"`  // -1=all GPU, 0=CPU-only
		CPUThreads *int   `json:"cpu_threads"`   // nil = auto-detect
		MemLimit   string `json:"memory_limit"`  // docker --memory e.g. "8g"
		Volume     string `json:"models_volume"` // named vol or host path

		// GPU placement — which GPU device indices to assign to this model.
		// e.g. [0] for first GPU, [0,1] for two GPUs, [] for CPU-only.
		GPUDevices []int `json:"gpu_devices"`

		// Node assignment — which node this model should run on.
		// If set, overrides the node_id inferred from model_endpoints.
		NodeID string `json:"node_id"`

		// ExecutionMode controls GPU vs CPU deployment.
		//   "cpu"  — CPU-only; no --gpus; n_gpu_layers forced to 0
		//   "gpu"  — always use GPUs
		//   "auto" — detect node GPU capability at startup (default)
		ExecutionMode string `json:"execution_mode"` // cpu | gpu | auto

		// Idle behaviour (0 = use cluster default)
		IdleTimeoutSecs *int `json:"idle_timeout_secs"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Encode gpu_devices as JSON array for storage.
	gpuDevicesJSON := "[]"
	if len(input.GPUDevices) > 0 {
		if b, err := json.Marshal(input.GPUDevices); err == nil {
			gpuDevicesJSON = string(b)
		}
	}

	// Upsert into model_runtime_configs.
	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO model_runtime_configs
		  (id, model_id,
		   gguf_path, hf_repo, hf_file, hf_token,
		   ctx_size, n_gpu_layers, cpu_threads, memory_limit, models_volume,
		   gpu_devices, node_id,
		   idle_timeout_secs, execution_mode, updated_at)
		VALUES (gen_random_uuid(), $1,
		        $2, $3, $4, $5,
		        $6, $7, COALESCE($8, 0), $9, $10,
		        $11::jsonb, $12::uuid,
		        $13, $14, NOW())
		ON CONFLICT (model_id) DO UPDATE SET
		  gguf_path         = COALESCE(EXCLUDED.gguf_path,         model_runtime_configs.gguf_path),
		  hf_repo           = COALESCE(EXCLUDED.hf_repo,           model_runtime_configs.hf_repo),
		  hf_file           = COALESCE(EXCLUDED.hf_file,           model_runtime_configs.hf_file),
		  hf_token          = COALESCE(EXCLUDED.hf_token,          model_runtime_configs.hf_token),
		  ctx_size          = EXCLUDED.ctx_size,
		  n_gpu_layers      = EXCLUDED.n_gpu_layers,
		  cpu_threads       = COALESCE(EXCLUDED.cpu_threads,        model_runtime_configs.cpu_threads, 0),
		  memory_limit      = EXCLUDED.memory_limit,
		  models_volume     = EXCLUDED.models_volume,
		  gpu_devices       = EXCLUDED.gpu_devices,
		  node_id           = COALESCE(EXCLUDED.node_id,           model_runtime_configs.node_id),
		  idle_timeout_secs = EXCLUDED.idle_timeout_secs,
		  execution_mode    = COALESCE(NULLIF(EXCLUDED.execution_mode,''), model_runtime_configs.execution_mode, 'auto'),
		  updated_at        = NOW()`,
		modelID,
		nilableStr(input.GGUFPath), nilableStr(input.HFRepo), nilableStr(input.HFFile), nilableStr(input.HFToken),
		input.CtxSize, input.NGPULayers, input.CPUThreads, nilableStr(input.MemLimit), nilableStr(input.Volume),
		gpuDevicesJSON, nilableStr(input.NodeID),
		input.IdleTimeoutSecs,
		orDefault(input.ExecutionMode, "auto"),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "lazy-load config updated", "model_id": modelID})
}

// GetLazyConfig handles GET /admin/v1/models/:id/lazy-config
func (h *LazyRuntimeHandler) GetLazyConfig(c *gin.Context) {
	modelID := c.Param("id")
	var row struct {
		GGUFPath      *string         `db:"gguf_path"          json:"gguf_path"`
		HFRepo        *string         `db:"hf_repo"            json:"hf_repo"`
		HFFile        *string         `db:"hf_file"            json:"hf_file"`
		CtxSize       int             `db:"ctx_size"           json:"ctx_size"`
		NGPULayers    int             `db:"n_gpu_layers"       json:"n_gpu_layers"`
		CPUThreads    *int            `db:"cpu_threads"        json:"cpu_threads"`
		MemLimit      *string         `db:"memory_limit"       json:"memory_limit"`
		Volume        *string         `db:"models_volume"      json:"models_volume"`
		GPUDevices    json.RawMessage `db:"gpu_devices"        json:"gpu_devices"`
		NodeID        *string         `db:"node_id"            json:"node_id"`
		IdleTimeout   *int            `db:"idle_timeout_secs"  json:"idle_timeout_secs"`
		ExecutionMode string          `db:"execution_mode"     json:"execution_mode"`
		UpdatedAt     time.Time       `db:"updated_at"         json:"updated_at"`
	}
	if err := h.db.GetContext(c.Request.Context(), &row, `
		SELECT COALESCE(gguf_path,'')         AS gguf_path,
		       COALESCE(hf_repo,'')           AS hf_repo,
		       COALESCE(hf_file,'')           AS hf_file,
		       COALESCE(ctx_size, 4096)       AS ctx_size,
		       COALESCE(n_gpu_layers, 0)      AS n_gpu_layers,
		       cpu_threads,
		       memory_limit,
		       models_volume,
		       COALESCE(gpu_devices, '[]'::jsonb)   AS gpu_devices,
		       node_id::text                  AS node_id,
		       idle_timeout_secs,
		       COALESCE(execution_mode,'auto') AS execution_mode,
		       updated_at
		FROM model_runtime_configs WHERE model_id = $1`, modelID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no lazy config found for this model"})
		return
	}
	c.JSON(http.StatusOK, row)
}

// GetRuntimeStatus handles GET /admin/v1/models/:id/runtime-status
//
// Returns the live container state for every node this model is deployed on.
func (h *LazyRuntimeHandler) GetRuntimeStatus(c *gin.Context) {
	modelID := c.Param("id")
	type statusRow struct {
		RuntimeID     string     `db:"id"              json:"runtime_id"`
		NodeID        string     `db:"node_id"         json:"node_id"`
		Hostname      string     `db:"hostname"        json:"hostname"`
		State         string     `db:"state"           json:"state"`
		ContainerID   string     `db:"container_id"    json:"container_id"`
		BindHost      string     `db:"bind_host"       json:"bind_host"`
		BindPort      int        `db:"bind_port"       json:"bind_port"`
		RequestedMode string     `db:"requested_mode"  json:"requested_mode"`
		EffectiveMode string     `db:"effective_mode"  json:"effective_mode"`
		LastUsedAt    *time.Time `db:"last_used_at"    json:"last_used_at"`
		UpdatedAt     time.Time  `db:"updated_at"      json:"updated_at"`
	}
	var rows []statusRow
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT ar.id, ar.node_id, n.hostname, ar.state,
		       COALESCE(ar.container_id,'')       AS container_id,
		       ar.bind_host, ar.bind_port,
		       COALESCE(ar.requested_mode,'auto') AS requested_mode,
		       COALESCE(ar.effective_mode,'cpu')  AS effective_mode,
		       ar.last_used_at, ar.updated_at
		FROM agent_runtimes ar
		JOIN nodes n ON n.id = ar.node_id
		WHERE ar.model_id = $1
		  AND ar.state NOT IN ('deleted')
		ORDER BY ar.updated_at DESC`, modelID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if rows == nil {
		rows = []statusRow{}
	}
	c.JSON(http.StatusOK, gin.H{
		"model_id": modelID,
		"runtimes": rows,
		"count":    len(rows),
	})
}
