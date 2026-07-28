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
	"github.com/nexusllm/nexusllm/internal/project"
	"github.com/nexusllm/nexusllm/internal/runtime"
	"github.com/nexusllm/nexusllm/internal/scheduler"
	"github.com/nexusllm/nexusllm/internal/taskmanager"
	"github.com/redis/go-redis/v9"
)

// RuntimeHandler manages the runtime lifecycle API.
type RuntimeHandler struct {
	db       *sqlx.DB
	rdb      *redis.Client
	registry *runtime.Registry
	ctrl     *controller.ModelController
	sched    *scheduler.Scheduler // optional; nil = manual GPU assignment only
	taskMgr  *taskmanager.Manager // optional; nil = local Docker deployment only
}

// NewRuntimeHandler constructs a RuntimeHandler.
func NewRuntimeHandler(db *sqlx.DB, rdb *redis.Client, registry *runtime.Registry, ctrl *controller.ModelController) *RuntimeHandler {
	return &RuntimeHandler{db: db, rdb: rdb, registry: registry, ctrl: ctrl}
}

// WithScheduler attaches the placement scheduler to the handler.
// When set, auto_place=true deployments use the scheduler to select a node.
func (h *RuntimeHandler) WithScheduler(s *scheduler.Scheduler) *RuntimeHandler {
	h.sched = s
	return h
}

// WithPlacement is a no-op kept for call-site compatibility while callers migrate.
// Deprecated: use WithScheduler instead.
func (h *RuntimeHandler) WithPlacement(_ interface{}) *RuntimeHandler {
	return h
}

// WithTaskManager attaches a task manager, enabling node-agent based deployment.
func (h *RuntimeHandler) WithTaskManager(tm *taskmanager.Manager) *RuntimeHandler {
	h.taskMgr = tm
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
		Name        string   `json:"name"         binding:"required"`
		DisplayName string   `json:"display_name" binding:"required"`
		Provider    string   `json:"provider"`
		BackendType string   `json:"backend_type"`
		ServiceType string   `json:"service_type"` // CHAT | STT | TTS | OCR | EMBEDDING | RERANK | VISION | IMAGE_GENERATION | CUSTOM
		MaxContext  int      `json:"max_context"`
		MaxOutput   int      `json:"max_output"`
		Tags        []string `json:"tags"`

		// Container / runtime
		Image          string   `json:"image"` // optional — agent can default
		HFModelID      string   `json:"hf_model_id"`
		Host           string   `json:"host"`
		Port           int      `json:"port"` // 0 = agent allocates
		GPUDevices     []int    `json:"gpu_devices"`
		TensorParallel int      `json:"tensor_parallel"`
		GPUMemoryUtil  float64  `json:"gpu_memory_util"`
		MaxModelLen    int      `json:"max_model_len"`
		Dtype          string   `json:"dtype"`
		Quantization   string   `json:"quantization"`
		ExtraArgs      []string `json:"extra_args"`
		HFToken        string   `json:"hf_token"`

		StartNow *bool `json:"start_now"`

		// Legacy auto-place fields (kept for backward compat)
		AutoPlace      bool  `json:"auto_place"`
		MinVRAMMB      int64 `json:"min_vram_mb"`
		MaxVRAMMB      int64 `json:"max_vram_mb"`
		PriorityWeight int   `json:"priority_weight"`

		// NodeID — legacy direct pin (still accepted)
		NodeID string `json:"node_id"`

		// ── Placement v2 ─────────────────────────────────────────────────────
		// PlacementMode: auto | specific_node | node_group | label_selector
		PlacementMode string `json:"placement_mode"`
		// SpecificNodeID for PlacementMode == "specific_node"
		SpecificNodeID string `json:"specific_node_id"`
		// NodeGroupID for PlacementMode == "node_group"
		NodeGroupID string `json:"node_group_id"`
		// NodeSelector: label key→value map for PlacementMode == "label_selector"
		// Example: {"accelerator":"h200","storage":"nvme"}
		NodeSelector map[string]string `json:"node_selector"`
		// PlacementStrategy for tie-breaking within matched nodes: spread | packed | auto
		PlacementStrategy string `json:"placement_strategy"`
		// AcceleratorType requirement: any | gpu | cpu
		AcceleratorType string `json:"accelerator_type"`
		// ReplicaDistribution for HA: spread | anti_affinity | pack
		ReplicaDistribution string `json:"replica_distribution"`

		// ── llamacpp-specific ─────────────────────────────────────────────────
		LlamaCppModelPath    string `json:"llamacpp_model_path"`
		LlamaCppHFRepo       string `json:"llamacpp_hf_repo"`
		LlamaCppHFFile       string `json:"llamacpp_hf_file"`
		LlamaCppCtxSize      int    `json:"llamacpp_ctx_size"`
		LlamaCppNGPULayers   int    `json:"llamacpp_n_gpu_layers"`
		LlamaCppModelsVolume string `json:"llamacpp_models_volume"`
		ExecutionMode        string `json:"execution_mode"`

		// ── Thinking/reasoning mode ──────────────────────────────────────────
		// SupportsThinking marks this model as a reasoning model.
		// ThinkingEnabled sets the deployment default (true = thinking on by default).
		// MinThinkingTokens auto-disables thinking when max_tokens < this value.
		SupportsThinking  bool `json:"supports_thinking"`
		ThinkingEnabled   bool `json:"thinking_enabled"`
		MinThinkingTokens int  `json:"min_thinking_tokens"`

		// Capabilities explicitly declares which API endpoints this model supports.
		// When omitted, capabilities are derived automatically from ServiceType.
		// Example: ["chat","completion"] for a chat model, ["transcription"] for Whisper.
		Capabilities []string `json:"capabilities"`

		// Env contains additional environment variables injected into the container.
		// Used for service-specific configuration (e.g. WHISPER__MODEL, UVICORN_PORT).
		Env map[string]string `json:"env"`

		// Cloud / external API credentials — for models not run by NexusLLM containers.
		// upstream_api_key is injected as Authorization: Bearer on every upstream request.
		// upstream_base_url overrides host:port (e.g. "https://api.openai.com").
		// upstream_proxy routes outbound calls through an HTTP/SOCKS5 proxy.
		// Leave blank for local self-hosted models.
		UpstreamAPIKey    string `json:"upstream_api_key"`
		UpstreamBaseURL   string `json:"upstream_base_url"`
		UpstreamProxy     string `json:"upstream_proxy"`
		UpstreamModelName string `json:"upstream_model_name"`
	}

	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Defaults
	if input.BackendType == "" {
		input.BackendType = "vllm"
	}
	if input.ServiceType == "" {
		input.ServiceType = "CHAT"
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

	// ── Resolve bind host from node IP ────────────────────────────────────
	// When deploying to a specific node, the endpoint host must be the node's
	// real IP address (not localhost) so the gateway can route to it over the network.
	bindHost := input.Host
	if input.NodeID != "" {
		var nodeIP string
		_ = h.db.QueryRowContext(c.Request.Context(),
			`SELECT COALESCE(host(ip_address), '') FROM nodes WHERE id = $1`, input.NodeID,
		).Scan(&nodeIP)
		if nodeIP != "" {
			bindHost = nodeIP
		} else {
			var hostname string
			_ = h.db.QueryRowContext(c.Request.Context(),
				`SELECT hostname FROM nodes WHERE id = $1`, input.NodeID,
			).Scan(&hostname)
			if hostname != "" {
				bindHost = hostname
			}
		}
	}

	// ── 1. Insert model row ────────────────────────────────────────────────
	mID := uuid.New().String()
	minThinkTok := input.MinThinkingTokens
	if minThinkTok == 0 {
		minThinkTok = 500
	}

	// Resolve capabilities: use explicit input if provided, otherwise derive
	// from service_type so the column is always populated on insert.
	capabilitiesJSON := capabilitiesFromInput(input.Capabilities, input.ServiceType)

	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO models
		  (id, name, display_name, provider, backend_type, service_type,
		   max_context, max_output, enabled, tags, capabilities,
		   supports_thinking, thinking_enabled, min_thinking_tokens)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,TRUE,$9,$10::jsonb,$11,$12,$13)`,
		mID, input.Name, input.DisplayName, input.Provider, input.BackendType, input.ServiceType,
		input.MaxContext, input.MaxOutput,
		tagsJSON(input.Tags),
		capabilitiesJSON,
		input.SupportsThinking, input.ThinkingEnabled, minThinkTok,
	)
	if err != nil {
		// May fail if migration 027 hasn't run — retry without thinking columns.
		_, err = h.db.ExecContext(c.Request.Context(), `
			INSERT INTO models
			  (id, name, display_name, provider, backend_type, service_type,
			   max_context, max_output, enabled, tags, capabilities)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,TRUE,$9,$10::jsonb)`,
			mID, input.Name, input.DisplayName, input.Provider, input.BackendType, input.ServiceType,
			input.MaxContext, input.MaxOutput,
			tagsJSON(input.Tags),
			capabilitiesJSON,
		)
		if err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "model name already exists: " + err.Error()})
			return
		}
	}

	// Default version
	_, _ = h.db.ExecContext(c.Request.Context(),
		`INSERT INTO model_versions (id, model_id, version, is_default) VALUES ($1,$2,'v1',TRUE)`,
		uuid.New().String(), mID)

	// Runtime config — save all fields including llamacpp source so lazy config modal can read them back
	_, _ = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO model_runtime_configs
		  (id, model_id, gpu_memory_util, tensor_parallel, dtype, quantization,
		   gguf_path, hf_repo, hf_file, hf_token, ctx_size, n_gpu_layers,
		   models_volume, execution_mode)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		uuid.New().String(), mID,
		input.GPUMemoryUtil, input.TensorParallel,
		orDefault(input.Dtype, "auto"),
		nilableStr(input.Quantization),
		nilableStr(input.LlamaCppModelPath),
		nilableStr(input.LlamaCppHFRepo),
		nilableStr(input.LlamaCppHFFile),
		nilableStr(input.HFToken),
		func() int {
			if input.LlamaCppCtxSize > 0 {
				return input.LlamaCppCtxSize
			}
			return 4096
		}(),
		input.LlamaCppNGPULayers,
		nilableStr(input.LlamaCppModelsVolume),
		orDefault(input.ExecutionMode, "auto"),
	)

	// ── 2. Insert endpoint row ─────────────────────────────────────────────
	epID := uuid.New().String()
	runtimeImage := input.Image
	_, err = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO model_endpoints
		  (id, model_id, host, port, base_path, weight, priority,
		   health_status, is_enabled, lifecycle_state, runtime_image,
		   upstream_api_key, upstream_base_url, upstream_proxy)
		VALUES ($1,$2,$3,$4,'/v1',100,1,'unknown',TRUE,'registered',$5,
		        NULLIF($6,''), NULLIF($7,''), COALESCE(NULLIF($8,''),''))`,
		epID, mID, bindHost, input.Port, runtimeImage,
		input.UpstreamAPIKey, input.UpstreamBaseURL, input.UpstreamProxy,
	)
	if err != nil {
		// Rollback model row
		_, _ = h.db.ExecContext(c.Request.Context(), `DELETE FROM models WHERE id = $1`, mID)
		c.JSON(http.StatusConflict, gin.H{"error": "endpoint conflict (host:port already used): " + err.Error()})
		return
	}

	// ── 3. Deploy the runtime ─────────────────────────────────────────────────
	canDeploy := input.BackendType == "vllm" ||
		input.BackendType == "tgi" || input.BackendType == "llamacpp" || input.BackendType == "cpu_native"
	shouldStart := startNow && canDeploy && input.Image != ""

	// ── Resolve effective NodeID from placement mode ───────────────────────
	// For specific_node: use SpecificNodeID directly (already validated by scheduler).
	// For legacy node_id field: honour it as-is.
	// For node_group / label_selector: the scheduler picks the node; NodeID stays empty
	// here and the caller should use the scheduler path instead.
	if input.PlacementMode == "specific_node" && input.SpecificNodeID != "" && input.NodeID == "" {
		input.NodeID = input.SpecificNodeID
	}

	// ── Auto-placement: scheduler picks the best node + GPU allocation ────────
	// Activated when auto_place=true and no explicit NodeID was given.
	// The scheduler considers VRAM, RAM, CPU, GPU count, NUMA locality,
	// node labels, and project priority — identical to HA replica placement.
	if (input.AutoPlace || input.NodeID != "") && h.sched != nil && input.NodeID == "" {
		gpuCount := len(input.GPUDevices)
		if gpuCount == 0 {
			gpuCount = input.TensorParallel
			if gpuCount == 0 {
				gpuCount = 1
			}
		}
		execMode := input.ExecutionMode
		if execMode == "" {
			execMode = "auto"
		}
		sReq := scheduler.PlacementRequest{
			ModelID:        mID,
			ModelName:      input.Name,
			RequiredVRAMMB: input.MinVRAMMB,
			RequiredGPUs:   gpuCount,
			ExecutionMode:  execMode,
			Mode:           scheduler.ModeAuto,
			AcceleratorReq: func() scheduler.AcceleratorType {
				switch input.AcceleratorType {
				case "gpu":
					return scheduler.AcceleratorGPU
				case "cpu":
					return scheduler.AcceleratorCPU
				default:
					return scheduler.AcceleratorAny
				}
			}(),
			PriorityWeight: project.PriorityWeight(func() int {
				if input.PriorityWeight > 0 {
					return input.PriorityWeight
				}
				return 500
			}()),
			EffectivePriority: func() int {
				if input.PriorityWeight > 0 {
					return input.PriorityWeight
				}
				return 500
			}(),
		}
		if dec, schedErr := h.sched.Decide(c.Request.Context(), sReq); schedErr == nil {
			input.GPUDevices = dec.GPUDeviceIndices
			if len(dec.GPUDeviceIndices) > 0 {
				input.TensorParallel = len(dec.GPUDeviceIndices)
			}
			// Apply: mark the scheduler decision as used.
			_, _ = h.sched.Apply(c.Request.Context(), dec, sReq)
		}
	}
	// ── Path A: Deploy via Node Agent ──────────────────────────────────────
	// Dispatch a START_MODEL task — the single unified startup pipeline.
	// The agent executes: VALIDATING → DOWNLOADING → STARTING → LOADING_MODEL.
	// The control plane (registry) polls /health to complete → READY.
	if shouldStart && input.NodeID != "" && h.taskMgr != nil {
		runtimeID := uuid.New().String()
		containerName := "nexus-" + input.Name

		gpuIDsJSON := "[]"
		if len(input.GPUDevices) > 0 {
			if b, jerr := json.Marshal(input.GPUDevices); jerr == nil {
				gpuIDsJSON = string(b)
			}
		}

		// Insert runtime row at state="pending" — the only value guaranteed
		// to be in the agent_runtimes_state_check constraint on all DB versions.
		// We check RowsAffected: if it's 0 the task enqueue must not proceed
		// because runtimeID won't exist and the FK on agent_tasks.runtime_id
		// will reject the insert.
		rtRes, rtErr := h.db.ExecContext(c.Request.Context(), `
			INSERT INTO agent_runtimes
			  (id, node_id, endpoint_id, model_id, runtime_name, backend,
			   state, gpu_ids, bind_host, bind_port, cpu_affinity, numa_node)
			VALUES ($1,$2,$3,$4,$5,$6,'pending',$7::jsonb,$8,$9,'',-1)`,
			runtimeID, input.NodeID, epID, mID,
			containerName, input.BackendType,
			gpuIDsJSON, bindHost, input.Port,
		)
		if rtErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"model_id": mID, "endpoint_id": epID,
				"error": "failed to create runtime record: " + rtErr.Error(),
			})
			return
		}
		if n, _ := rtRes.RowsAffected(); n == 0 {
			c.JSON(http.StatusInternalServerError, gin.H{
				"model_id": mID, "endpoint_id": epID,
				"error": "runtime record not created (possible duplicate) — cannot dispatch task",
			})
			return
		}

		// Build the unified START_MODEL payload.
		// All startup scenarios — initial deploy, cold start, re-deploy, recovery —
		// use this exact payload structure via TaskStartModel.
		//
		// Inject --reasoning off for llamacpp backends where thinking is supported
		// but disabled — this enforces the setting at the server level, which is
		// more reliable than chat_template_kwargs (which depends on the Jinja template).
		effectiveExtraArgs := input.ExtraArgs
		if input.BackendType == "llamacpp" && input.SupportsThinking && !input.ThinkingEnabled {
			alreadySet := false
			for _, a := range effectiveExtraArgs {
				if a == "--reasoning" || a == "-rea" {
					alreadySet = true
					break
				}
			}
			if !alreadySet {
				effectiveExtraArgs = append([]string{"--reasoning", "off"}, effectiveExtraArgs...)
			}
		}

		// Merge input.Env with any implicitly derived env vars (e.g. HF token).
		// Start with a fresh map so we never mutate the input.
		deployEnv := make(map[string]string, len(input.Env))
		for k, v := range input.Env {
			deployEnv[k] = v
		}
		// HFToken takes precedence over any HUGGING_FACE_HUB_TOKEN set in input.Env.
		if input.HFToken != "" {
			deployEnv["HUGGING_FACE_HUB_TOKEN"] = input.HFToken
		}

		payload := taskmanager.StartModelPayload{
			RuntimeID:      runtimeID,
			EndpointID:     epID,
			ModelID:        mID,
			RuntimeName:    containerName,
			Backend:        input.BackendType,
			Image:          runtimeImage,
			ModelName:      modelID,
			ServedAs:       input.Name,
			BindHost:       bindHost,
			BindPort:       input.Port,
			GPUDevices:     input.GPUDevices,
			TensorParallel: input.TensorParallel,
			GPUMemoryUtil:  input.GPUMemoryUtil,
			MaxModelLen:    input.MaxModelLen,
			Dtype:          input.Dtype,
			Quantization:   input.Quantization,
			ExtraArgs:      effectiveExtraArgs,
			HFToken:        input.HFToken,
			Env:            deployEnv,
			// llamacpp model source
			GGUFPath:      input.LlamaCppModelPath,
			HFRepo:        input.LlamaCppHFRepo,
			HFFile:        input.LlamaCppHFFile,
			CtxSize:       input.LlamaCppCtxSize,
			NGPULayers:    input.LlamaCppNGPULayers,
			ModelsVolume:  input.LlamaCppModelsVolume,
			ExecutionMode: orDefault(input.ExecutionMode, "auto"),
			// NUMANode: -1 means no affinity (don't add --cpuset-mems).
			// 0 would add --cpuset-mems 0 which restricts the container to NUMA
			// node 0 — undesirable for generic CPU services like STT/Embedding.
			NUMANode: -1,
		}

		// Task priority — derived from project priority_weight (0–1000 → 50–95 task scale)
		priority := 70
		if input.PriorityWeight >= 900 {
			priority = 95
		} else if input.PriorityWeight >= 700 {
			priority = 85
		} else if input.PriorityWeight >= 500 {
			priority = 70
		} else if input.PriorityWeight >= 300 {
			priority = 55
		}

		taskID, taskErr := h.taskMgr.Enqueue(
			c.Request.Context(),
			input.NodeID,
			taskmanager.TaskStartModel,
			payload,
			taskmanager.WithPriority(priority),
			taskmanager.WithActor("admin-deploy"),
			taskmanager.WithRuntimeID(runtimeID),
			taskmanager.WithIdempotencyKey("start:"+input.NodeID+":"+runtimeID),
		)

		// Mark endpoint as starting; link to node.
		_, _ = h.db.ExecContext(c.Request.Context(),
			`UPDATE model_endpoints SET lifecycle_state='loading', node_id=$1, updated_at=NOW() WHERE id=$2`,
			input.NodeID, epID)

		_ = h.registry.Reload(c.Request.Context())

		if taskErr != nil {
			c.JSON(http.StatusAccepted, gin.H{
				"model_id":    mID,
				"endpoint_id": epID,
				"runtime_id":  runtimeID,
				"warning":     "model registered but START_MODEL task dispatch failed: " + taskErr.Error(),
			})
			return
		}

		c.JSON(http.StatusCreated, gin.H{
			"model_id":    mID,
			"model_name":  input.Name,
			"endpoint_id": epID,
			"runtime_id":  runtimeID,
			"task_id":     taskID,
			"node_id":     input.NodeID,
			"status":      "created",
			"note":        "START_MODEL task dispatched — pipeline: VALIDATING → DOWNLOADING → STARTING → LOADING_MODEL → READY",
		})
		return
	}

	// ── Path B: Deploy locally via Docker (legacy / single-server) ──────────
	var containerID string
	if shouldStart {
		env := map[string]string{}
		if input.HFToken != "" {
			env["HUGGING_FACE_HUB_TOKEN"] = input.HFToken
		}

		spec := controller.RuntimeSpec{
			ModelName:       modelID,
			ServedModelName: input.Name,
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
			// Generic model source fields (formerly LlamaCpp-prefixed)
			GGUFPath:     input.LlamaCppModelPath,
			HFRepo:       input.LlamaCppHFRepo,
			HFFile:       input.LlamaCppHFFile,
			CtxSize:      input.LlamaCppCtxSize,
			NGPULayers:   input.LlamaCppNGPULayers,
			ModelsVolume: input.LlamaCppModelsVolume,
		}

		containerID, err = h.ctrl.StartRaw(c.Request.Context(), epID, mID, spec, "admin")
		if err != nil {
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
		"status": func() string {
			if shouldStart && containerID != "" {
				return "loading"
			}
			if input.BackendType == "openai_compat" {
				return "active"
			}
			return "registered"
		}(),
		"note": func() string {
			if shouldStart && containerID != "" {
				return ""
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
// Registers an already-running external model (e.g. TGI, remote API).
// Use DeployModel for containers managed by NexusLLM.
func (h *RuntimeHandler) RegisterModel(c *gin.Context) {
	var input struct {
		Name        string   `json:"name"         binding:"required"`
		DisplayName string   `json:"display_name" binding:"required"`
		Provider    string   `json:"provider"`
		BackendType string   `json:"backend_type" binding:"required"`
		ServiceType string   `json:"service_type"`
		Host        string   `json:"host"         binding:"required"`
		Port        int      `json:"port"         binding:"required"`
		MaxContext  int      `json:"max_context"`
		MaxOutput   int      `json:"max_output"`
		Tags        []string `json:"tags"`
		// Capabilities explicitly declares which API endpoints this model supports.
		// When omitted, derived automatically from service_type.
		Capabilities []string `json:"capabilities"`
		// Cloud / external API credentials.
		// upstream_api_key is injected as Authorization: Bearer on every upstream request.
		// upstream_base_url overrides host:port routing (e.g. "https://api.openai.com").
		// upstream_proxy routes outbound calls through an HTTP/SOCKS5 proxy.
		// Leave blank for local self-hosted models.
		UpstreamAPIKey  string `json:"upstream_api_key"`
		UpstreamBaseURL string `json:"upstream_base_url"`
		UpstreamProxy   string `json:"upstream_proxy"`
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
	if input.ServiceType == "" {
		input.ServiceType = "CHAT"
	}

	capabilitiesJSON := capabilitiesFromInput(input.Capabilities, input.ServiceType)

	mID := uuid.New().String()
	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO models
		  (id, name, display_name, provider, backend_type, service_type,
		   max_context, max_output, enabled, tags, capabilities)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,TRUE,$9,$10::jsonb)`,
		mID, input.Name, input.DisplayName, input.Provider, input.BackendType, input.ServiceType,
		input.MaxContext, input.MaxOutput,
		tagsJSON(input.Tags),
		capabilitiesJSON,
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
		   health_status, is_enabled, lifecycle_state,
		   upstream_api_key, upstream_base_url, upstream_proxy)
		VALUES ($1,$2,$3,$4,'/v1',100,1,'unknown',TRUE,'active',
		        NULLIF($5,''), NULLIF($6,''), COALESCE(NULLIF($7,''),''))`,
		epID, mID, input.Host, input.Port,
		input.UpstreamAPIKey, input.UpstreamBaseURL, input.UpstreamProxy,
	)

	_ = h.registry.Reload(c.Request.Context())
	c.JSON(http.StatusCreated, gin.H{
		"model_id":      mID,
		"model_name":    input.Name,
		"endpoint_id":   epID,
		"capabilities":  capabilitiesJSON,
		"upstream_auth": input.UpstreamAPIKey != "",
		"note":          "registered as external model — NexusLLM will not manage its container lifecycle",
	})
}

// ─── External / cloud provider model registration ────────────────────────────

// RegisterExternalModel handles POST /admin/v1/models/external
//
// Registers an external AI provider model as a first-class model.
// The model enters the single model registry and is immediately routable.
// No container is started; no scheduler or placement decisions are made.
// Everything else — policy, rate limits, quota, audit, usage — runs
// identically to a locally managed model.
//
// Architecture note: the backend_type field determines which Backend
// implementation handles requests (e.g. "anthropic_provider" → AnthropicBackend).
// The provider_name column mirrors backend_type for query convenience.
// upstream_base_url is pre-filled from provider_defaults when omitted.
//
// Security: upstream_api_key is stored in the DB column added by migration 040.
// In production, use a secrets manager reference (e.g. "vault://secret/openai-key")
// and resolve it at runtime. The gateway never returns the key in API responses.
func (h *RuntimeHandler) RegisterExternalModel(c *gin.Context) {
	var input struct {
		// Model identity
		Name        string   `json:"name"         binding:"required"`
		DisplayName string   `json:"display_name" binding:"required"`
		ServiceType string   `json:"service_type"`
		MaxContext  int      `json:"max_context"`
		MaxOutput   int      `json:"max_output"`
		Tags        []string `json:"tags"`
		Capabilities []string `json:"capabilities"`

		// Provider — must match a BackendType constant with IsProviderBackend()==true.
		// Examples: "openai_provider", "anthropic_provider", "google_provider",
		//           "azure_openai_provider", "openrouter_provider", "groq_provider",
		//           "together_provider", "mistral_provider", "cohere_provider",
		//           "deepseek_provider"
		ProviderBackendType string `json:"provider_backend_type" binding:"required"`

		// upstream_api_key — API key for the provider. Never returned in responses.
		UpstreamAPIKey string `json:"upstream_api_key"`

		// upstream_base_url — provider endpoint base URL.
		// When empty, the canonical default from provider_defaults is used.
		// Required for Azure OpenAI (resource-specific URL).
		UpstreamBaseURL string `json:"upstream_base_url"`

		// upstream_model_name — the model identifier sent to the provider.
		// When empty the NexusLLM model name is forwarded unchanged.
		// Example: NexusLLM name "gpt4" → upstream_model_name "gpt-4o"
		UpstreamModelName string `json:"upstream_model_name"`

		// upstream_proxy — optional HTTP/SOCKS5 proxy for outbound calls.
		UpstreamProxy string `json:"upstream_proxy"`

		// provider_api_version — used by Azure OpenAI (e.g. "2024-08-01-preview").
		ProviderAPIVersion string `json:"provider_api_version"`

		// provider_extra_config — arbitrary JSONB for provider-specific settings.
		// Examples: {"organization":"org-xxx"}, {"http_referer":"https://myapp.com"}
		ProviderExtraConfig map[string]interface{} `json:"provider_extra_config"`

		// provider_timeout_seconds — request timeout for this provider (default 120).
		ProviderTimeoutSeconds int `json:"provider_timeout_seconds"`

		// provider_max_retries — automatic retries on 429/5xx (default 2).
		ProviderMaxRetries int `json:"provider_max_retries"`

		// provider_extra_headers — additional HTTP headers injected on every request.
		// Example: {"HTTP-Referer":"https://nexusllm.example.com","X-Title":"NexusLLM"}
		ProviderExtraHeaders map[string]string `json:"provider_extra_headers"`

		// Allowed projects and teams for model ACL (optional).
		// When empty the model is accessible to all projects/teams in the org.
		AllowedProjectIDs []string `json:"allowed_project_ids"`
		AllowedTeamIDs    []string `json:"allowed_team_ids"`
	}

	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Validate provider backend type.
	providerType := runtime.BackendType(input.ProviderBackendType)
	if !runtime.IsProviderBackend(providerType) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("unknown provider backend type %q — valid values: "+
				"openai_provider, anthropic_provider, google_provider, azure_openai_provider, "+
				"openrouter_provider, groq_provider, together_provider, mistral_provider, "+
				"cohere_provider, deepseek_provider", input.ProviderBackendType),
		})
		return
	}

	// Apply defaults.
	if input.ServiceType == "" {
		input.ServiceType = "CHAT"
	}
	if input.MaxContext == 0 {
		input.MaxContext = 128000
	}
	if input.MaxOutput == 0 {
		input.MaxOutput = 16384
	}
	if input.ProviderTimeoutSeconds == 0 {
		input.ProviderTimeoutSeconds = 120
	}
	if input.ProviderMaxRetries == 0 {
		input.ProviderMaxRetries = 2
	}

	// Resolve upstream_base_url from provider_defaults when not supplied.
	if input.UpstreamBaseURL == "" {
		var defaultURL string
		_ = h.db.QueryRowContext(c.Request.Context(),
			`SELECT default_base_url FROM provider_defaults WHERE provider_name = $1`,
			input.ProviderBackendType,
		).Scan(&defaultURL)
		input.UpstreamBaseURL = defaultURL
	}

	// Azure OpenAI requires an explicit base URL (resource-specific).
	if providerType == runtime.BackendAzureOpenAI && input.UpstreamBaseURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "upstream_base_url is required for azure_openai_provider " +
				"(format: https://YOUR_RESOURCE.openai.azure.com)",
		})
		return
	}

	capabilitiesJSON := capabilitiesFromInput(input.Capabilities, input.ServiceType)

	extraConfigJSON := "{}"
	if len(input.ProviderExtraConfig) > 0 {
		if b, err := json.Marshal(input.ProviderExtraConfig); err == nil {
			extraConfigJSON = string(b)
		}
	}
	extraHeadersJSON := "{}"
	if len(input.ProviderExtraHeaders) > 0 {
		if b, err := json.Marshal(input.ProviderExtraHeaders); err == nil {
			extraHeadersJSON = string(b)
		}
	}

	// Insert model row — uses provider columns added by migration 044.
	mID := uuid.New().String()
	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO models
		  (id, name, display_name, provider, backend_type, service_type,
		   max_context, max_output, enabled, tags, capabilities,
		   provider_name, provider_is_external, provider_api_version, provider_extra_config)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,TRUE,$9,$10::jsonb,
		        $11,$12,$13,$14::jsonb)`,
		mID,
		input.Name,
		input.DisplayName,
		input.ProviderBackendType, // provider display column
		string(providerType),      // backend_type
		input.ServiceType,
		input.MaxContext,
		input.MaxOutput,
		tagsJSON(input.Tags),
		capabilitiesJSON,
		// provider columns
		input.ProviderBackendType,
		true,
		nilableStr(input.ProviderAPIVersion),
		extraConfigJSON,
	)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "model name already exists or DB error: " + err.Error()})
		return
	}

	_, _ = h.db.ExecContext(c.Request.Context(),
		`INSERT INTO model_versions (id, model_id, version, is_default) VALUES ($1,$2,'v1',TRUE)`,
		uuid.New().String(), mID)

	// Insert endpoint row — immediately active, no container lifecycle.
	// host/port are placeholders (0.0.0.0:0) since routing uses upstream_base_url.
	// health_status starts as 'unknown'; the watcher will probe on the next tick.
	epID := uuid.New().String()
	_, epErr := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO model_endpoints
		  (id, model_id, host, port, base_path, weight, priority,
		   health_status, is_enabled, lifecycle_state,
		   upstream_api_key, upstream_base_url, upstream_proxy, upstream_model_name,
		   provider_timeout_seconds, provider_max_retries, provider_extra_headers)
		VALUES ($1,$2,'0.0.0.0',0,'/v1',100,1,'unknown',TRUE,'active',
		        NULLIF($3,''), NULLIF($4,''), NULLIF($5,''), NULLIF($6,''),
		        $7,$8,$9::jsonb)`,
		epID, mID,
		input.UpstreamAPIKey,
		input.UpstreamBaseURL,
		input.UpstreamProxy,
		input.UpstreamModelName,
		input.ProviderTimeoutSeconds,
		input.ProviderMaxRetries,
		extraHeadersJSON,
	)
	if epErr != nil {
		// Roll back model row if endpoint insert failed.
		_, _ = h.db.ExecContext(c.Request.Context(), `DELETE FROM models WHERE id=$1`, mID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create endpoint: " + epErr.Error()})
		return
	}

	_ = h.registry.Reload(c.Request.Context())

	c.JSON(http.StatusCreated, gin.H{
		"model_id":              mID,
		"model_name":            input.Name,
		"endpoint_id":           epID,
		"provider_backend_type": input.ProviderBackendType,
		"upstream_base_url":     input.UpstreamBaseURL,
		"upstream_model_name":   input.UpstreamModelName,
		"upstream_api_key_set":  input.UpstreamAPIKey != "",
		"capabilities":          capabilitiesJSON,
		"status":                "active",
		"note": "provider model registered and immediately routable — " +
			"full policy pipeline applies (auth, rate limits, quota, audit, usage)",
	})
}

// ListProviderDefaults handles GET /admin/v1/providers
// Returns the canonical defaults for every supported external provider.
// Used by the admin UI to pre-fill the registration form.
func (h *RuntimeHandler) ListProviderDefaults(c *gin.Context) {
	type row struct {
		ProviderName      string `db:"provider_name"       json:"provider_name"`
		DisplayName       string `db:"display_name"        json:"display_name"`
		DefaultBaseURL    string `db:"default_base_url"    json:"default_base_url"`
		HealthPath        string `db:"health_path"         json:"health_path"`
		DocsURL           string `db:"docs_url"            json:"docs_url"`
		SupportsStreaming  bool   `db:"supports_streaming"  json:"supports_streaming"`
		SupportsFunctions bool   `db:"supports_functions"  json:"supports_functions"`
		SupportsVision    bool   `db:"supports_vision"     json:"supports_vision"`
		SupportsEmbedding bool   `db:"supports_embedding"  json:"supports_embedding"`
	}
	var rows []row
	if err := h.db.SelectContext(c.Request.Context(), &rows,
		`SELECT provider_name, display_name, default_base_url, health_path,
		        COALESCE(docs_url,'') AS docs_url,
		        supports_streaming, supports_functions, supports_vision, supports_embedding
		 FROM provider_defaults
		 ORDER BY display_name`); err != nil {
		// Table may not exist yet (migration 044 not run) — return hardcoded fallback.
		c.JSON(http.StatusOK, gin.H{"data": builtinProviderDefaults(), "source": "builtin"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": len(rows)})
}

// builtinProviderDefaults returns hardcoded provider info used when the
// provider_defaults table has not been created yet (pre-migration-044 installs).
func builtinProviderDefaults() []map[string]interface{} {
	return []map[string]interface{}{
		{"provider_name": "openai_provider",       "display_name": "OpenAI",          "default_base_url": "https://api.openai.com"},
		{"provider_name": "anthropic_provider",    "display_name": "Anthropic",        "default_base_url": "https://api.anthropic.com"},
		{"provider_name": "google_provider",       "display_name": "Google Gemini",    "default_base_url": "https://generativelanguage.googleapis.com"},
		{"provider_name": "azure_openai_provider", "display_name": "Azure OpenAI",     "default_base_url": ""},
		{"provider_name": "openrouter_provider",   "display_name": "OpenRouter",       "default_base_url": "https://openrouter.ai"},
		{"provider_name": "groq_provider",         "display_name": "Groq",             "default_base_url": "https://api.groq.com"},
		{"provider_name": "together_provider",     "display_name": "Together AI",      "default_base_url": "https://api.together.xyz"},
		{"provider_name": "mistral_provider",      "display_name": "Mistral AI",       "default_base_url": "https://api.mistral.ai"},
		{"provider_name": "cohere_provider",       "display_name": "Cohere",           "default_base_url": "https://api.cohere.com"},
		{"provider_name": "deepseek_provider",     "display_name": "DeepSeek",         "default_base_url": "https://api.deepseek.com"},
	}
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

// UpdateUpstream handles PUT /admin/v1/models/:id/upstream
// Updates the upstream credentials and proxy for a cloud/external model endpoint.
// All fields are optional — only provided (non-null pointer) fields are updated.
// To clear a field, send an explicit empty string "".
func (h *RuntimeHandler) UpdateUpstream(c *gin.Context) {
	modelID := c.Param("id")
	var input struct {
		// APIKey replaces the upstream Authorization: Bearer token.
		// Send "" to clear (direct / no auth upstream).
		APIKey *string `json:"upstream_api_key"`
		// BaseURL replaces the upstream host:port override.
		// Send "" to fall back to the endpoint's host:port.
		BaseURL *string `json:"upstream_base_url"`
		// Proxy sets the HTTP/SOCKS5 proxy for outbound calls to this endpoint.
		// Send "" to remove the proxy and connect directly.
		// Example: "http://squid.corp:3128" or "socks5://proxy:1080"
		Proxy *string `json:"upstream_proxy"`
		// ModelName sets the model identifier sent to the upstream backend.
		// Send "" to forward the NexusLLM model name unchanged.
		// Example: "large-v3" for faster-whisper-server when the NexusLLM model is "whisper"
		ModelName *string `json:"upstream_model_name"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.APIKey == nil && input.BaseURL == nil && input.Proxy == nil && input.ModelName == nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "at least one of upstream_api_key, upstream_base_url, upstream_proxy, upstream_model_name must be provided",
		})
		return
	}

	if input.APIKey != nil {
		_, _ = h.db.ExecContext(c.Request.Context(),
			`UPDATE model_endpoints SET upstream_api_key = NULLIF($1,''), updated_at = NOW()
			 WHERE model_id = $2`, *input.APIKey, modelID)
	}
	if input.BaseURL != nil {
		_, _ = h.db.ExecContext(c.Request.Context(),
			`UPDATE model_endpoints SET upstream_base_url = NULLIF($1,''), updated_at = NOW()
			 WHERE model_id = $2`, *input.BaseURL, modelID)
	}
	if input.Proxy != nil {
		_, _ = h.db.ExecContext(c.Request.Context(),
			`UPDATE model_endpoints SET upstream_proxy = $1, updated_at = NOW()
			 WHERE model_id = $2`, *input.Proxy, modelID)
	}
	if input.ModelName != nil {
		_, _ = h.db.ExecContext(c.Request.Context(),
			`UPDATE model_endpoints SET upstream_model_name = $1, updated_at = NOW()
			 WHERE model_id = $2`, *input.ModelName, modelID)
	}

	_ = h.registry.Reload(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{
		"message":         "upstream config updated",
		"model_id":        modelID,
		"proxy_set":       input.Proxy != nil && *input.Proxy != "",
		"model_name_set":  input.ModelName != nil && *input.ModelName != "",
	})
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
		ModelID             string     `db:"model_id"             json:"model_id"`
		UpstreamBaseURL     string     `db:"upstream_base_url"    json:"upstream_base_url"`
		UpstreamProxy       string     `db:"upstream_proxy"       json:"upstream_proxy"`
		// upstream_api_key is never returned — only its presence is indicated.
		UpstreamAPIKeySet   bool       `db:"upstream_api_key_set" json:"upstream_api_key_set"`
	}
	rows := make([]epRow, 0)
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT id, host, port, health_status, lifecycle_state, container_id,
		       consecutive_failures, response_time_ms, last_checked_at,
		       model_id,
		       COALESCE(upstream_base_url, '') AS upstream_base_url,
		       COALESCE(upstream_proxy, '')    AS upstream_proxy,
		       (upstream_api_key IS NOT NULL AND upstream_api_key != '') AS upstream_api_key_set
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
	lifecycle := c.Query("lifecycle")
	if lifecycle == "" {
		lifecycle = "active"
	}

	type mRow struct {
		ID                 string `db:"id"                  json:"id"`
		Name               string `db:"name"                json:"name"`
		DisplayName        string `db:"display_name"        json:"display_name"`
		Provider           string `db:"provider"            json:"provider"`
		BackendType        string `db:"backend_type"        json:"backend_type"`
		ServiceType        string `db:"service_type"        json:"service_type"`
		MaxContext         int    `db:"max_context"         json:"max_context"`
		MaxOutput          int    `db:"max_output"          json:"max_output"`
		Enabled            bool   `db:"enabled"             json:"enabled"`
		Lifecycle          string `db:"lifecycle"           json:"lifecycle"`
		EndpointCnt        int    `db:"endpoint_cnt"        json:"endpoint_count"`
		HealthyCnt         int    `db:"healthy_cnt"         json:"healthy_count"`
		SupportsThinking   bool   `db:"supports_thinking"   json:"supports_thinking"`
		ThinkingEnabled    bool   `db:"thinking_enabled"    json:"thinking_enabled"`
		MinThinkingTokens  int    `db:"min_thinking_tokens" json:"min_thinking_tokens"`
		// Provider columns (migration 044) — zero/false for local models.
		ProviderIsExternal bool   `db:"provider_is_external" json:"provider_is_external"`
		ProviderName       string `db:"provider_name"        json:"provider_name,omitempty"`
		UpstreamBaseURL    string `db:"upstream_base_url"    json:"upstream_base_url,omitempty"`
		UpstreamModelName  string `db:"upstream_model_name"  json:"upstream_model_name,omitempty"`
		HasUpstreamKey     bool   `db:"has_upstream_key"     json:"upstream_api_key_set"`
	}
	rows := make([]mRow, 0)

	query := `
		SELECT m.id, m.name, m.display_name, m.provider, m.backend_type,
		       COALESCE(m.service_type, 'CHAT') AS service_type,
		       m.max_context, m.max_output, m.enabled,
		       COALESCE(m.lifecycle,'active') AS lifecycle,
		       COUNT(me.id) AS endpoint_cnt,
		       COUNT(me.id) FILTER (WHERE me.health_status='healthy') AS healthy_cnt,
		       COALESCE(m.supports_thinking, FALSE)  AS supports_thinking,
		       COALESCE(m.thinking_enabled,  FALSE)  AS thinking_enabled,
		       COALESCE(m.min_thinking_tokens, 500)  AS min_thinking_tokens,
		       COALESCE(m.provider_is_external, FALSE) AS provider_is_external,
		       COALESCE(m.provider_name, '')            AS provider_name,
		       COALESCE(MIN(me.upstream_base_url), '')  AS upstream_base_url,
		       COALESCE(MIN(me.upstream_model_name), '') AS upstream_model_name,
		       BOOL_OR(me.upstream_api_key IS NOT NULL AND me.upstream_api_key != '') AS has_upstream_key
		FROM models m
		LEFT JOIN model_endpoints me ON me.model_id = m.id AND me.is_enabled = TRUE`

	switch lifecycle {
	case "all":
		query += " WHERE m.lifecycle != 'deleted'"
	case "archived":
		query += " WHERE m.lifecycle = 'archived'"
	default:
		query += " WHERE COALESCE(m.lifecycle,'active') = 'active'"
	}
	query += " GROUP BY m.id ORDER BY m.name"

	if err := h.db.SelectContext(c.Request.Context(), &rows, query); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": len(rows)})
}

// SetThinkingMode handles PUT /admin/v1/models/:id/thinking
// Updates the thinking capability flags for an existing model.
func (h *RuntimeHandler) SetThinkingMode(c *gin.Context) {
	modelID := c.Param("id")
	var input struct {
		SupportsThinking  *bool `json:"supports_thinking"`
		ThinkingEnabled   *bool `json:"thinking_enabled"`
		MinThinkingTokens *int  `json:"min_thinking_tokens"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	_, err := h.db.ExecContext(c.Request.Context(), `
		UPDATE models SET
		  supports_thinking   = COALESCE($2, supports_thinking),
		  thinking_enabled    = COALESCE($3, thinking_enabled),
		  min_thinking_tokens = COALESCE($4, min_thinking_tokens),
		  updated_at          = NOW()
		WHERE id = $1`,
		modelID,
		input.SupportsThinking,
		input.ThinkingEnabled,
		input.MinThinkingTokens,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "thinking mode updated", "model_id": modelID})
}

// UpdateCapabilities handles PUT /admin/v1/models/:id/capabilities
//
// Replaces the capabilities list for a model. After this call the gateway
// immediately enforces the new list — no restart required.
//
// Example request body:
//
//	{"capabilities": ["chat", "completion"]}
//
// Setting capabilities to an empty array ([]) is valid and means the model
// will reject all endpoint-guarded requests until capabilities are re-added.
func (h *RuntimeHandler) UpdateCapabilities(c *gin.Context) {
	modelID := c.Param("id")
	var input struct {
		Capabilities []string `json:"capabilities" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	capJSON, err := json.Marshal(input.Capabilities)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode capabilities"})
		return
	}

	res, err := h.db.ExecContext(c.Request.Context(), `
		UPDATE models SET capabilities = $2::jsonb, updated_at = NOW()
		WHERE id = $1`, modelID, string(capJSON))
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
	c.JSON(http.StatusOK, gin.H{
		"model_id":     modelID,
		"capabilities": input.Capabilities,
		"message":      "capabilities updated — gateway is now enforcing the new list",
	})
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// capabilitiesFromInput resolves the capability JSON string to store in the DB.
// When an explicit list is provided it is used as-is (after JSON-encoding).
// When the list is empty/nil it falls back to DefaultCapabilities(serviceType).
func capabilitiesFromInput(explicit []string, serviceType string) string {
	if len(explicit) > 0 {
		b, _ := json.Marshal(explicit)
		return string(b)
	}
	// Derive from service_type using the same rules as migration 033.
	caps := runtime.DefaultCapabilities(serviceType)
	if len(caps) == 0 {
		return "[]"
	}
	strs := make([]string, len(caps))
	for i, c := range caps {
		strs[i] = string(c)
	}
	b, _ := json.Marshal(strs)
	return string(b)
}

func tagsJSON(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return "[]"
	}
	return string(b)
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
		EndpointID     string    `db:"id"              json:"endpoint_id"`
		Host           string    `db:"host"            json:"host"`
		Port           int       `db:"port"            json:"port"`
		LifecycleState string    `db:"lifecycle_state" json:"lifecycle_state"`
		HealthStatus   string    `db:"health_status"   json:"health_status"`
		ContainerID    string    `db:"container_id"    json:"container_id"`
		RuntimeImage   string    `db:"runtime_image"   json:"runtime_image"`
		UpdatedAt      time.Time `db:"updated_at"      json:"updated_at"`
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
// Also syncs port/host from the most recent active agent_runtime so the
// watcher probes the correct address after a container restart.
func (h *RuntimeHandler) ResetHealth(c *gin.Context) {
	modelID := c.Param("id")
	epID := c.Query("endpoint_id") // optional — reset specific endpoint or all

	// Sync port and host from the most recently updated active agent_runtime.
	// This corrects the stale port that accumulates when the container restarts
	// on a different port (port=0 OS-allocated, or conflict scan) while the
	// model_endpoints row still holds the previous port.
	_, _ = h.db.ExecContext(c.Request.Context(), `
		UPDATE model_endpoints me
		SET port        = CASE WHEN ar.bind_port > 0 THEN ar.bind_port ELSE me.port END,
		    host        = CASE WHEN ar.bind_host != '' THEN ar.bind_host ELSE me.host END,
		    updated_at  = NOW()
		FROM (
		    SELECT bind_port, bind_host
		    FROM agent_runtimes
		    WHERE model_id = $1
		      AND bind_port > 0
		      AND state NOT IN ('stopped','deleted','failed')
		    ORDER BY updated_at DESC
		    LIMIT 1
		) ar
		WHERE me.model_id = $1`, modelID)

	query := `UPDATE model_endpoints
	          SET health_status = 'unknown',
	              is_enabled    = TRUE,
	              lifecycle_state = CASE
	                WHEN lifecycle_state IN ('failed','unloaded') THEN 'active'
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

// ─── Model Resource Reservations ─────────────────────────────────────────────

// GetReservation handles GET /admin/v1/models/:id/reservation
//
// Deprecated: The resource_reservations table is dropped in migration 036.
// Resource requirements are now expressed via model_runtime_configs.
// This endpoint remains for API clients that haven't migrated yet.
func (h *RuntimeHandler) GetReservation(c *gin.Context) {
	modelID := c.Param("id")
	var rr struct {
		ID               string    `db:"id"                json:"id"`
		ModelID          string    `db:"model_id"          json:"model_id"`
		MinVRAMMB        int64     `db:"min_vram_mb"       json:"min_vram_mb"`
		MaxVRAMMB        int64     `db:"max_vram_mb"       json:"max_vram_mb"`
		CPUCores         int       `db:"cpu_cores"         json:"cpu_cores"`
		NUMANodePref     int       `db:"numa_node_pref"    json:"numa_node_pref"`
		RAMMB            int64     `db:"ram_mb"            json:"ram_mb"`
		PreferredRuntime string    `db:"preferred_runtime" json:"preferred_runtime"`
		CreatedAt        time.Time `db:"created_at"        json:"created_at"`
		UpdatedAt        time.Time `db:"updated_at"        json:"updated_at"`
	}
	// Add deprecation header so clients know to migrate.
	c.Header("Deprecation", "true")
	c.Header("Link", `</admin/v1/models/`+modelID+`/runtime-config>; rel="successor-version"`)

	err := h.db.GetContext(c.Request.Context(), &rr, `
		SELECT id, model_id, min_vram_mb, max_vram_mb, cpu_cores,
		       numa_node_pref, ram_mb, preferred_runtime, created_at, updated_at
		FROM resource_reservations WHERE model_id = $1`, modelID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error":      "no reservation found for model " + modelID,
			"deprecated": "resource_reservations removed in migration 036 — use PUT /models/:id/lazy-config for resource hints",
		})
		return
	}
	c.JSON(http.StatusOK, rr)
}

// UpsertReservation handles PUT /admin/v1/models/:id/reservation
//
// Deprecated: The resource_reservations table is dropped in migration 036.
// Use PUT /admin/v1/models/:id/lazy-config for resource requirement hints.
func (h *RuntimeHandler) UpsertReservation(c *gin.Context) {
	modelID := c.Param("id")
	var req struct {
		MinVRAMMB        int64  `json:"min_vram_mb"`
		MaxVRAMMB        int64  `json:"max_vram_mb"`
		CPUCores         int    `json:"cpu_cores"`
		NUMANodePref     int    `json:"numa_node_pref"`
		RAMMB            int64  `json:"ram_mb"`
		PreferredRuntime string `json:"preferred_runtime"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.PreferredRuntime == "" {
		req.PreferredRuntime = "GPU_RUNTIME"
	}

	c.Header("Deprecation", "true")
	c.Header("Link", `</admin/v1/models/`+modelID+`/lazy-config>; rel="successor-version"`)

	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO resource_reservations
		  (id, model_id, min_vram_mb, max_vram_mb, cpu_cores,
		   numa_node_pref, ram_mb, preferred_runtime)
		VALUES (gen_random_uuid(),$1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (model_id) DO UPDATE SET
		  min_vram_mb      = EXCLUDED.min_vram_mb,
		  max_vram_mb      = EXCLUDED.max_vram_mb,
		  cpu_cores        = EXCLUDED.cpu_cores,
		  numa_node_pref   = EXCLUDED.numa_node_pref,
		  ram_mb           = EXCLUDED.ram_mb,
		  preferred_runtime = EXCLUDED.preferred_runtime,
		  updated_at       = NOW()`,
		modelID,
		req.MinVRAMMB, req.MaxVRAMMB,
		req.CPUCores, req.NUMANodePref,
		req.RAMMB, req.PreferredRuntime,
	)
	if err != nil {
		// Table was dropped in migration 036 — return a helpful message.
		c.JSON(http.StatusGone, gin.H{
			"error":     "resource_reservations table no longer exists (removed in migration 036)",
			"migration": "use PUT /admin/v1/models/" + modelID + "/lazy-config with min_vram_mb field instead",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "reservation updated", "model_id": modelID})
}

// ─── Model Lifecycle (Archive / Restore) ─────────────────────────────────────

// ArchiveModel handles POST /admin/v1/models/:id/archive
// Archived models stay in the DB and audit logs but cannot be deployed
// and do not appear in default listings.
func (h *RuntimeHandler) ArchiveModel(c *gin.Context) {
	modelID := c.Param("id")

	// First stop all running endpoints
	_, _ = h.db.ExecContext(c.Request.Context(), `
		UPDATE model_endpoints
		SET health_status = 'down', is_enabled = FALSE, updated_at = NOW()
		WHERE model_id = $1`, modelID)

	_, err := h.db.ExecContext(c.Request.Context(), `
		UPDATE models SET lifecycle = 'archived', enabled = FALSE, updated_at = NOW()
		WHERE id = $1 AND lifecycle = 'active'`, modelID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	_, _ = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO model_lifecycle_events_v2 (model_id, from_state, to_state, reason, actor)
		VALUES ($1, 'active', 'archived', 'admin archived', 'admin')`, modelID)

	_ = h.registry.Reload(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"message": "model archived", "model_id": modelID})
}

// RestoreModel handles POST /admin/v1/models/:id/restore
// Restores an archived model to active state.
func (h *RuntimeHandler) RestoreModel(c *gin.Context) {
	modelID := c.Param("id")

	res, err := h.db.ExecContext(c.Request.Context(), `
		UPDATE models SET lifecycle = 'active', enabled = TRUE, updated_at = NOW()
		WHERE id = $1 AND lifecycle = 'archived'`, modelID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model is not archived"})
		return
	}

	// Re-enable endpoints
	_, _ = h.db.ExecContext(c.Request.Context(), `
		UPDATE model_endpoints SET is_enabled = TRUE, health_status = 'unknown', updated_at = NOW()
		WHERE model_id = $1`, modelID)

	_, _ = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO model_lifecycle_events_v2 (model_id, from_state, to_state, reason, actor)
		VALUES ($1, 'archived', 'active', 'admin restored', 'admin')`, modelID)

	_ = h.registry.Reload(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"message": "model restored to active", "model_id": modelID})
}
