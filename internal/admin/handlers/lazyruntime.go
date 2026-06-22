package handlers

// lazyruntime.go — Admin API handlers for lazy-load runtime configuration.
//
// Routes:
//   PUT  /admin/v1/models/:id/lazy-config   — set GGUF source + idle timeout
//   GET  /admin/v1/models/:id/lazy-config   — read current config
//   GET  /admin/v1/models/:id/runtime-status — live state of the model container

import (
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
		HFRepo string `json:"hf_repo"` // e.g. "bartowski/gemma-2-2b-it-GGUF"
		HFFile string `json:"hf_file"` // e.g. "gemma-2-2b-it-Q4_K_M.gguf"
		HFToken string `json:"hf_token"` // for gated repos (stored encrypted in prod)

		// llama-server flags
		CtxSize    int    `json:"ctx_size"`     // default: 4096
		NGPULayers int    `json:"n_gpu_layers"` // -1=all GPU, 0=CPU-only
		CPUThreads *int   `json:"cpu_threads"`  // nil = auto-detect
		MemLimit   string `json:"memory_limit"` // docker --memory e.g. "8g"
		Volume     string `json:"models_volume"` // named vol or host path

		// Idle behaviour (0 = use cluster default)
		IdleTimeoutSecs *int `json:"idle_timeout_secs"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Upsert into model_runtime_configs.
	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO model_runtime_configs
		  (id, model_id,
		   gguf_path, hf_repo, hf_file, hf_token,
		   ctx_size, n_gpu_layers, cpu_threads, memory_limit, models_volume,
		   idle_timeout_secs, updated_at)
		VALUES (gen_random_uuid(), $1,
		        $2, $3, $4, $5,
		        $6, $7, $8, $9, $10,
		        $11, NOW())
		ON CONFLICT (model_id) DO UPDATE SET
		  gguf_path         = EXCLUDED.gguf_path,
		  hf_repo           = EXCLUDED.hf_repo,
		  hf_file           = EXCLUDED.hf_file,
		  hf_token          = EXCLUDED.hf_token,
		  ctx_size          = EXCLUDED.ctx_size,
		  n_gpu_layers      = EXCLUDED.n_gpu_layers,
		  cpu_threads       = EXCLUDED.cpu_threads,
		  memory_limit      = EXCLUDED.memory_limit,
		  models_volume     = EXCLUDED.models_volume,
		  idle_timeout_secs = EXCLUDED.idle_timeout_secs,
		  updated_at        = NOW()`,
		modelID,
		nilableStr(input.GGUFPath), nilableStr(input.HFRepo), nilableStr(input.HFFile), nilableStr(input.HFToken),
		input.CtxSize, input.NGPULayers, input.CPUThreads, nilableStr(input.MemLimit), nilableStr(input.Volume),
		input.IdleTimeoutSecs,
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
		GGUFPath     *string `db:"gguf_path"          json:"gguf_path"`
		HFRepo       *string `db:"hf_repo"            json:"hf_repo"`
		HFFile       *string `db:"hf_file"            json:"hf_file"`
		CtxSize      int     `db:"ctx_size"           json:"ctx_size"`
		NGPULayers   int     `db:"n_gpu_layers"       json:"n_gpu_layers"`
		CPUThreads   *int    `db:"cpu_threads"        json:"cpu_threads"`
		MemLimit     *string `db:"memory_limit"       json:"memory_limit"`
		Volume       *string `db:"models_volume"      json:"models_volume"`
		IdleTimeout  *int    `db:"idle_timeout_secs"  json:"idle_timeout_secs"`
		UpdatedAt    time.Time `db:"updated_at"       json:"updated_at"`
	}
	if err := h.db.GetContext(c.Request.Context(), &row, `
		SELECT COALESCE(gguf_path,'')    AS gguf_path,
		       COALESCE(hf_repo,'')      AS hf_repo,
		       COALESCE(hf_file,'')      AS hf_file,
		       COALESCE(ctx_size, 4096)  AS ctx_size,
		       COALESCE(n_gpu_layers, 0) AS n_gpu_layers,
		       cpu_threads,
		       memory_limit,
		       models_volume,
		       idle_timeout_secs,
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
		RuntimeID   string     `db:"id"           json:"runtime_id"`
		NodeID      string     `db:"node_id"       json:"node_id"`
		Hostname    string     `db:"hostname"      json:"hostname"`
		State       string     `db:"state"         json:"state"`
		ContainerID string     `db:"container_id"  json:"container_id"`
		BindHost    string     `db:"bind_host"     json:"bind_host"`
		BindPort    int        `db:"bind_port"     json:"bind_port"`
		LastUsedAt  *time.Time `db:"last_used_at"  json:"last_used_at"`
		UpdatedAt   time.Time  `db:"updated_at"    json:"updated_at"`
	}
	var rows []statusRow
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT ar.id, ar.node_id, n.hostname, ar.state,
		       COALESCE(ar.container_id,'') AS container_id,
		       ar.bind_host, ar.bind_port,
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
