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
		//
		// Pointer-typed so an omitted key preserves the stored value. This used
		// to be a plain []int written straight to gpu_devices = EXCLUDED.*, so
		// any PUT that didn't mention GPUs (e.g. saving only extra_args from the
		// model detail page) silently blanked the assignment. On the next
		// container start buildDockerArgs then saw len(GPUDevices)==0, omitted
		// --gpus, and the GPU model crash-looped with "Failed to infer device
		// type" / "No CUDA runtime is found".
		GPUDevices *[]int `json:"gpu_devices"`

		// Node assignment — which node this model should run on.
		// If set, overrides the node_id inferred from model_endpoints.
		NodeID string `json:"node_id"`

		// ExecutionMode controls GPU vs CPU deployment.
		//   "cpu"  — CPU-only; no --gpus; n_gpu_layers forced to 0
		//   "gpu"  — always use GPUs
		//   "auto" — detect node GPU capability at startup (default)
		ExecutionMode string `json:"execution_mode"` // cpu | gpu | auto

		// ExtraArgs are appended verbatim to the backend command after all
		// structured flags. Useful for model-specific tuning:
		//   ["-thk","0"]           — disable Qwen3 thinking mode
		//   ["--rope-scale","2"]   — extend context via RoPE scaling
		//   ["--no-warmup"]        — skip warmup inference on startup
		// Pointer-typed for the same preserve-on-omit reason as GPUDevices.
		ExtraArgs *[]string `json:"extra_args"`

		// Env vars passed to the container at startup via -e KEY=VALUE.
		// Useful for CPU-native services like faster-whisper:
		//   {"WHISPER__MODEL":"Systran/faster-whisper-large-v3","UVICORN_PORT":"8100"}
		// The agent always overrides PORT after port scanning, so use the
		// service-specific var (e.g. UVICORN_PORT) to request a preferred port.
		Env *map[string]string `json:"env"`

		// Idle behaviour (0 = use cluster default)
		IdleTimeoutSecs *int `json:"idle_timeout_secs"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Encode the JSONB fields. nil (→ SQL NULL) means "caller omitted this key,
	// keep whatever is stored"; the statement below COALESCEs NULL back to the
	// existing row on conflict, and to the column default on insert. An
	// explicitly-sent empty array/object still round-trips as [] / {}.
	jsonbOrNil := func(v interface{}) interface{} {
		if v == nil {
			return nil
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return string(b)
	}
	var gpuDevicesJSON, extraArgsJSON, envJSON interface{}
	if input.GPUDevices != nil {
		gpuDevicesJSON = jsonbOrNil(*input.GPUDevices)
	}
	if input.ExtraArgs != nil {
		extraArgsJSON = jsonbOrNil(*input.ExtraArgs)
	}
	if input.Env != nil {
		envJSON = jsonbOrNil(*input.Env)
	}

	// Upsert into model_runtime_configs.
	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO model_runtime_configs
		  (id, model_id,
		   gguf_path, hf_repo, hf_file, hf_token,
		   ctx_size, n_gpu_layers, cpu_threads, memory_limit, models_volume,
		   gpu_devices, node_id,
		   idle_timeout_secs, execution_mode, extra_args, env, updated_at)
		VALUES (gen_random_uuid(), $1,
		        $2, $3, $4, $5,
		        $6, $7, COALESCE($8, 0), $9, $10,
		        COALESCE($11::jsonb, '[]'::jsonb), $12::uuid,
		        $13, COALESCE(NULLIF($14::text,''), 'auto'),
		        COALESCE($15::jsonb, '[]'::jsonb), COALESCE($16::jsonb, '{}'::jsonb), NOW())
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
		  gpu_devices       = COALESCE($11::jsonb, model_runtime_configs.gpu_devices),
		  node_id           = COALESCE(EXCLUDED.node_id,           model_runtime_configs.node_id),
		  idle_timeout_secs = EXCLUDED.idle_timeout_secs,
		  execution_mode    = COALESCE(NULLIF($14::text,''), model_runtime_configs.execution_mode, 'auto'),
		  extra_args        = COALESCE($15::jsonb, model_runtime_configs.extra_args),
		  env               = COALESCE($16::jsonb, model_runtime_configs.env),
		  updated_at        = NOW()`,
		modelID,
		nilableStr(input.GGUFPath), nilableStr(input.HFRepo), nilableStr(input.HFFile), nilableStr(input.HFToken),
		input.CtxSize, input.NGPULayers, input.CPUThreads, nilableStr(input.MemLimit), nilableStr(input.Volume),
		gpuDevicesJSON, nilableStr(input.NodeID),
		input.IdleTimeoutSecs,
		nilableStr(input.ExecutionMode),
		extraArgsJSON,
		envJSON,
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
		ExtraArgs     json.RawMessage `db:"extra_args"         json:"extra_args"`
		Env           json.RawMessage `db:"env"                json:"env"`
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
		       COALESCE(gpu_devices, '[]'::jsonb)    AS gpu_devices,
		       node_id::text                  AS node_id,
		       idle_timeout_secs,
		       COALESCE(execution_mode,'auto') AS execution_mode,
		       CASE WHEN jsonb_typeof(COALESCE(extra_args, '[]'::jsonb)) = 'array'
		            THEN COALESCE(extra_args, '[]'::jsonb)
		            ELSE '[]'::jsonb END AS extra_args,
		       CASE WHEN jsonb_typeof(COALESCE(env, '{}'::jsonb)) = 'object'
		            THEN COALESCE(env, '{}'::jsonb)
		            ELSE '{}'::jsonb END AS env,
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
