package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/controller"
	"github.com/nexusllm/nexusllm/internal/modelguard"
	"github.com/nexusllm/nexusllm/internal/nodeaddr"
	"github.com/nexusllm/nexusllm/internal/policy"
	"github.com/nexusllm/nexusllm/internal/project"
	"github.com/nexusllm/nexusllm/internal/replicaguard"
	"github.com/nexusllm/nexusllm/internal/runtime"
	"github.com/nexusllm/nexusllm/internal/scheduler"
	"github.com/nexusllm/nexusllm/internal/taskmanager"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// RuntimeHandler manages the runtime lifecycle API.
type RuntimeHandler struct {
	db       *sqlx.DB
	rdb      *redis.Client
	registry *runtime.Registry
	ctrl     *controller.ModelController
	sched    *scheduler.Scheduler // optional; nil = manual GPU assignment only
	taskMgr  *taskmanager.Manager // optional; nil = local Docker deployment only
	log      *zap.Logger
	engine   *policy.Engine // required for permission-restore Redis sync — see WithPolicyEngine
}

// NewRuntimeHandler constructs a RuntimeHandler.
func NewRuntimeHandler(db *sqlx.DB, rdb *redis.Client, registry *runtime.Registry, ctrl *controller.ModelController) *RuntimeHandler {
	log, _ := zap.NewProduction()
	return &RuntimeHandler{db: db, rdb: rdb, registry: registry, ctrl: ctrl, log: log}
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

// WithPolicyEngine attaches the gateway's policy engine so permission
// restoration (restorePermissionsFromSnapshot) can synchronize restored
// team_model_permissions rows into the live Redis ACL set via the same
// canonical SetModelAllowed path the admin grant endpoint uses — never a
// duplicate Redis write. Required for restore to report success; see
// restorePermissionsFromSnapshot's doc comment.
func (h *RuntimeHandler) WithPolicyEngine(e *policy.Engine) *RuntimeHandler {
	h.engine = e
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

		// DeploymentMode — who owns the container (migration 061).
		//   "managed" (default) — NexusLLM starts/stops/recovers it
		//   "manual"            — the operator already deployed it themselves
		//                         (docker compose, systemd, another host); the
		//                         model is registered and routed, but no
		//                         container is ever started or stopped for it
		DeploymentMode string `json:"deployment_mode"`

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
	if input.ServiceType == "" {
		input.ServiceType = "CHAT"
	}
	if input.BackendType == "" {
		switch input.ServiceType {
		case "STT", "EMBEDDING", "TTS", "OCR":
			input.BackendType = "cpu_native"
		default:
			input.BackendType = "vllm"
		}
	}
	// Force cpu_native for EMBEDDING on CPU.
	// openai_compat only tries /v1/embeddings; cpu_native tries both
	// /v1/embeddings and /embeddings, which is required for Infinity v2.
	// This override fires even when the caller explicitly sends openai_compat.
	if input.ServiceType == "EMBEDDING" &&
		(input.ExecutionMode == "cpu" || input.AcceleratorType == "cpu") &&
		input.BackendType == "openai_compat" {
		input.BackendType = "cpu_native"
	}
	// vllm/tgi are GPU-only in practice — there is no meaningful CPU mode for
	// either image (unlike llamacpp/cpu_native, which legitimately run on
	// CPU). The NodeAgent's own "auto" fallback (executor.go's wantsGPU
	// switch) only special-cases llamacpp's NGPULayers, so a vllm/tgi deploy
	// left at the default execution_mode="auto" with no explicit gpu_devices
	// silently produced a container with NO --gpus flag at all — vLLM then
	// fails immediately with "Failed to infer device type" (confirmed in
	// production: a gpt-oss-120b deploy with tensor_parallel set but no
	// gpu_devices/execution_mode came up with zero GPU visibility). Default
	// to "gpu" here so ExecutionMode reaches the agent already resolved,
	// matching this handler's own stated architecture (see executor.go's
	// "the control plane resolves auto → cpu|gpu before dispatch" comment) —
	// an operator who genuinely wants CPU-mode vllm/tgi can still set
	// execution_mode:"cpu" explicitly.
	if input.ExecutionMode == "" && (input.BackendType == "vllm" || input.BackendType == "tgi") {
		input.ExecutionMode = "gpu"
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
	// Default input.Host for the legacy no-node Docker path (Path B, below),
	// which reads input.Host directly. This default has no bearing on the
	// node-backed bind-host resolution further down, which never consults
	// input.Host when a node is assigned — see the invariant comment there.
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

	// A manual deployment declares that the container already exists and is
	// the operator's to manage: register and route it, but never dispatch a
	// START_MODEL for it. host and port must point at the running container.
	if input.DeploymentMode == "" {
		input.DeploymentMode = modelguard.ModeManaged
	}
	if input.DeploymentMode != modelguard.ModeManaged && input.DeploymentMode != modelguard.ModeManual {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "deployment_mode must be 'managed' or 'manual'",
		})
		return
	}
	if input.DeploymentMode == modelguard.ModeManual {
		if input.Host == "" || input.Port == 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "deployment_mode=manual requires host and port of the already-running container",
			})
			return
		}
		startNow = false
	}

	// Use HF model ID as the served model name if provided, else use name
	modelID := input.HFModelID
	if modelID == "" {
		modelID = input.Name
	}

	// ── Resolve effective NodeID from placement mode ───────────────────────
	// For specific_node: use SpecificNodeID directly (already validated by scheduler).
	// For legacy node_id field: honour it as-is.
	// For node_group / label_selector: the scheduler picks the node; NodeID stays empty
	// here and the caller should use the scheduler path instead.
	//
	// This MUST run before the bind-host resolution and the model_endpoints
	// INSERT below — that row's `host` column is what the admin API/UI
	// displays for this endpoint.
	if input.PlacementMode == "specific_node" && input.SpecificNodeID != "" && input.NodeID == "" {
		input.NodeID = input.SpecificNodeID
	}

	// ── Resolve bind host ──────────────────────────────────────────────────
	// INVARIANT (Case File 003): node_id != "" implies
	//   bind_host == nodeaddr.CanonicalHost(node_id)
	// unconditionally. A caller-supplied `host` can NEVER override a selected
	// node's canonical address — not even when the caller explicitly sends
	// "localhost", "127.0.0.1", or any other literal. This closes the gap
	// where the Admin Panel unconditionally sent host:"localhost" alongside a
	// real node_id: the previous "only resolve when the caller supplied none"
	// guard trusted that literal as genuine intent and never ran resolution.
	//
	// Only when no node is assigned at all (pure local/legacy/provider-backed
	// deployment) does a caller-supplied host — or the "localhost" default
	// above — apply, preserving prior behavior for that case.
	var bindHost string
	if input.NodeID != "" {
		bindHost = nodeaddr.CanonicalHost(c.Request.Context(), h.db, input.NodeID)
	} else if input.Host != "" {
		bindHost = input.Host
	} else {
		bindHost = "localhost"
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
		   supports_thinking, thinking_enabled, min_thinking_tokens,
		   deployment_mode)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,TRUE,$9,$10::jsonb,$11,$12,$13,$14)`,
		mID, input.Name, input.DisplayName, input.Provider, input.BackendType, input.ServiceType,
		input.MaxContext, input.MaxOutput,
		tagsJSON(input.Tags),
		capabilitiesJSON,
		input.SupportsThinking, input.ThinkingEnabled, minThinkTok,
		input.DeploymentMode,
	)
	if err != nil {
		// May fail if migration 027 (thinking columns) or 061
		// (deployment_mode) hasn't run — retry without the optional columns.
		if input.DeploymentMode == modelguard.ModeManual {
			h.log.Warn("deployment_mode could not be stored — model will be treated as NexusLLM-managed; apply migration 061",
				zap.String("model", input.Name), zap.Error(err))
		}
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

	// Restore team_model_permissions from snapshot if this model name was previously
	// soft-deleted.  No-op on first-ever deployment of a new name.
	if _, restoreErr := h.restorePermissionsFromSnapshot(c.Request.Context(), input.Name, mID); restoreErr != nil {
		h.log.Error("permission restore failed — this deploy will NOT have any team grants "+
			"automatically recovered from a previous deletion of this model name; grant access manually if needed",
			zap.String("model_name", input.Name), zap.String("model_id", mID), zap.Error(restoreErr))
	}

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
		input.BackendType == "tgi" || input.BackendType == "llamacpp" ||
		input.BackendType == "cpu_native" || input.BackendType == "openai_compat"
	shouldStart := startNow && canDeploy && input.Image != ""

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

		// Claim a replica slot before inserting. mID is brand new in this
		// request so it can't collide with itself, but the HA reconciler and
		// the stuck-runtime sweeper both run on independent timers and can
		// pick up this same model_id (e.g. via its own under-replication scan)
		// concurrently with this handler — without a shared claim, both this
		// INSERT and a concurrent reconciler/sweeper INSERT can succeed,
		// producing two runtimes for a model that wants one. ClaimSlot performs
		// the same atomic count-and-claim under a per-model advisory lock that
		// internal/ha/reconciler.go already uses.
		ctx := c.Request.Context()
		desiredReplicas, desErr := replicaguard.DesiredReplicas(ctx, h.db, mID)
		if desErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"model_id": mID, "endpoint_id": epID,
				"error": "failed to load desired_replicas: " + desErr.Error(),
			})
			return
		}
		tx, txErr := h.db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if txErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"model_id": mID, "endpoint_id": epID,
				"error": "failed to begin claim transaction: " + txErr.Error(),
			})
			return
		}
		slotAvailable, slotErr := replicaguard.ClaimSlot(ctx, tx, mID, desiredReplicas)
		if slotErr != nil {
			_ = tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{
				"model_id": mID, "endpoint_id": epID,
				"error": "claim_replica_slot error: " + slotErr.Error(),
			})
			return
		}
		if !slotAvailable {
			_ = tx.Rollback()
			c.JSON(http.StatusConflict, gin.H{
				"model_id": mID, "endpoint_id": epID,
				"error": "model already at desired_replicas capacity — a concurrent process claimed the slot first",
			})
			return
		}

		// Insert runtime row at state="pending" — the only value guaranteed
		// to be in the agent_runtimes_state_check constraint on all DB versions.
		// We check RowsAffected: if it's 0 the task enqueue must not proceed
		// because runtimeID won't exist and the FK on agent_tasks.runtime_id
		// will reject the insert.
		rtRes, rtErr := tx.ExecContext(ctx, `
			INSERT INTO agent_runtimes
			  (id, node_id, endpoint_id, model_id, runtime_name, backend,
			   state, gpu_ids, bind_host, bind_port, cpu_affinity, numa_node)
			VALUES ($1,$2,$3,$4,$5,$6,'pending',$7::jsonb,$8,$9,'',-1)`,
			runtimeID, input.NodeID, epID, mID,
			containerName, input.BackendType,
			gpuIDsJSON, bindHost, input.Port,
		)
		if rtErr != nil {
			_ = tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{
				"model_id": mID, "endpoint_id": epID,
				"error": "failed to create runtime record: " + rtErr.Error(),
			})
			return
		}
		if n, _ := rtRes.RowsAffected(); n == 0 {
			_ = tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{
				"model_id": mID, "endpoint_id": epID,
				"error": "runtime record not created (possible duplicate) — cannot dispatch task",
			})
			return
		}
		if commitErr := tx.Commit(); commitErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"model_id": mID, "endpoint_id": epID,
				"error": "failed to commit claimed runtime row: " + commitErr.Error(),
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
		"model_id":        mID,
		"model_name":      input.Name,
		"endpoint_id":     epID,
		"host":            input.Host,
		"port":            input.Port,
		"deployment_mode": input.DeploymentMode,
		"started":         shouldStart && containerID != "",
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
			if input.DeploymentMode == modelguard.ModeManual {
				return "Registered as a manual deployment — NexusLLM routes to " +
					fmt.Sprintf("%s:%d", input.Host, input.Port) +
					" and health-checks it, but never starts, stops or recreates its container. " +
					"Bring the container up yourself; it becomes routable as soon as the health check passes."
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

		// DeploymentMode — defaults to "manual" on this route: it registers a
		// model that is already running somewhere NexusLLM did not put it, and
		// this handler never creates a container or a runtime config. Pass
		// "managed" only to hand the container lifecycle to NexusLLM after
		// supplying a runtime config (PUT /admin/v1/models/:id/runtime-config).
		DeploymentMode string `json:"deployment_mode"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.DeploymentMode == "" {
		input.DeploymentMode = modelguard.ModeManual
	}
	if input.DeploymentMode != modelguard.ModeManaged && input.DeploymentMode != modelguard.ModeManual {
		c.JSON(http.StatusBadRequest, gin.H{"error": "deployment_mode must be 'managed' or 'manual'"})
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
		   max_context, max_output, enabled, tags, capabilities, deployment_mode)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,TRUE,$9,$10::jsonb,$11)`,
		mID, input.Name, input.DisplayName, input.Provider, input.BackendType, input.ServiceType,
		input.MaxContext, input.MaxOutput,
		tagsJSON(input.Tags),
		capabilitiesJSON,
		input.DeploymentMode,
	)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "model name already exists: " + err.Error()})
		return
	}

	_, _ = h.db.ExecContext(c.Request.Context(),
		`INSERT INTO model_versions (id, model_id, version, is_default) VALUES ($1,$2,'v1',TRUE)`,
		uuid.New().String(), mID)

	// Restore team_model_permissions from snapshot if this model name was previously
	// soft-deleted.  No-op on first-ever deployment of a new name.
	if _, restoreErr := h.restorePermissionsFromSnapshot(c.Request.Context(), input.Name, mID); restoreErr != nil {
		h.log.Error("permission restore failed — this deploy will NOT have any team grants "+
			"automatically recovered from a previous deletion of this model name; grant access manually if needed",
			zap.String("model_name", input.Name), zap.String("model_id", mID), zap.Error(restoreErr))
	}

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
		"model_id":        mID,
		"model_name":      input.Name,
		"endpoint_id":     epID,
		"capabilities":    capabilitiesJSON,
		"upstream_auth":   input.UpstreamAPIKey != "",
		"deployment_mode": input.DeploymentMode,
		"note": func() string {
			if input.DeploymentMode == modelguard.ModeManual {
				return "registered as a manual deployment — NexusLLM routes to it and health-checks it, " +
					"but never starts, stops or recreates its container"
			}
			return "registered without a runtime config — add one before starting it through NexusLLM"
		}(),
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
		Name         string   `json:"name"         binding:"required"`
		DisplayName  string   `json:"display_name" binding:"required"`
		ServiceType  string   `json:"service_type"`
		MaxContext   int      `json:"max_context"`
		MaxOutput    int      `json:"max_output"`
		Tags         []string `json:"tags"`
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

		// ── Per-provider HTTP transport (migration 046) ───────────────────────
		// All fields are optional. Zero values apply BuildProviderClient() defaults.
		// These settings are isolated per-endpoint: changing one provider's proxy
		// never affects any other provider.

		// ProxyURL: outbound proxy for this provider only.
		// Schemes: http, https, socks5. Credentials may be embedded.
		// Example: "socks5://user:pass@proxy.corp:1080"
		// Empty = direct connection. HTTP_PROXY env var is never consulted.
		ProxyURL string `json:"proxy_url"`

		// TLSInsecureSkipVerify: disable TLS cert verification.
		// Use only in controlled corporate environments with MITM proxies.
		TLSInsecureSkipVerify bool `json:"tls_insecure_skip_verify"`

		// TLSRootCAPEM: PEM-encoded CA bundle appended to system roots.
		// Use when a corporate proxy presents a self-signed certificate.
		TLSRootCAPEM string `json:"tls_root_ca_pem"`

		// ConnectTimeoutSeconds: TCP dial timeout. Default: 10 s.
		ConnectTimeoutSeconds int `json:"connect_timeout_seconds"`

		// ReadTimeoutSeconds: max time for a non-streaming response body.
		// 0 = unlimited (streaming-safe; use context deadline instead).
		ReadTimeoutSeconds int `json:"read_timeout_seconds"`

		// IdleConnTimeoutSeconds: idle keep-alive pool timeout. Default: 90 s.
		IdleConnTimeoutSeconds int `json:"idle_conn_timeout_seconds"`

		// ResponseHeaderTimeoutSeconds: max time to wait for response headers.
		// Default: 30 s. -1 = disabled.
		ResponseHeaderTimeoutSeconds int `json:"response_header_timeout_seconds"`

		// MaxIdleConnsPerHost: idle connections in pool per host. Default: 32.
		MaxIdleConnsPerHost int `json:"max_idle_conns_per_host"`

		// MaxConnsPerHost: total connections (idle + active) per host. 0 = unlimited.
		MaxConnsPerHost int `json:"max_conns_per_host"`

		// DisableHTTP2: prevent HTTP/2 negotiation. Default: false.
		DisableHTTP2 bool `json:"disable_http2"`

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

	// Restore team_model_permissions from snapshot if this model name was previously
	// soft-deleted.  No-op on first-ever registration of a new name.
	if _, restoreErr := h.restorePermissionsFromSnapshot(c.Request.Context(), input.Name, mID); restoreErr != nil {
		h.log.Error("permission restore failed — this deploy will NOT have any team grants "+
			"automatically recovered from a previous deletion of this model name; grant access manually if needed",
			zap.String("model_name", input.Name), zap.String("model_id", mID), zap.Error(restoreErr))
	}

	// Insert endpoint row — immediately active, no container lifecycle.
	// host/port are placeholders (0.0.0.0:0) since routing uses upstream_base_url.
	// health_status starts as 'unknown'; the watcher will probe on the next tick.
	epID := uuid.New().String()
	_, epErr := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO model_endpoints
		  (id, model_id, host, port, base_path, weight, priority,
		   health_status, is_enabled, lifecycle_state,
		   upstream_api_key, upstream_base_url, upstream_proxy, upstream_model_name,
		   provider_timeout_seconds, provider_max_retries, provider_extra_headers,
		   provider_proxy_url, provider_tls_insecure_skip_verify,
		   provider_tls_root_ca_pem,
		   provider_connect_timeout_seconds, provider_read_timeout_seconds,
		   provider_idle_conn_timeout_seconds, provider_response_header_timeout_seconds,
		   provider_max_idle_conns_per_host, provider_max_conns_per_host,
		   provider_disable_http2)
		VALUES ($1,$2,'0.0.0.0',0,'/v1',100,1,'unknown',TRUE,'active',
		        NULLIF($3,''), NULLIF($4,''), COALESCE(NULLIF($5,''),''), NULLIF($6,''),
		        $7,$8,$9::jsonb,
		        NULLIF($10,''), $11,
		        NULLIF($12,''),
		        $13,$14,$15,$16,$17,$18,$19)`,
		epID, mID,
		input.UpstreamAPIKey,
		input.UpstreamBaseURL,
		input.UpstreamProxy,
		input.UpstreamModelName,
		input.ProviderTimeoutSeconds,
		input.ProviderMaxRetries,
		extraHeadersJSON,
		// migration 046 transport fields
		input.ProxyURL,
		input.TLSInsecureSkipVerify,
		input.TLSRootCAPEM,
		input.ConnectTimeoutSeconds,
		input.ReadTimeoutSeconds,
		input.IdleConnTimeoutSeconds,
		input.ResponseHeaderTimeoutSeconds,
		input.MaxIdleConnsPerHost,
		input.MaxConnsPerHost,
		input.DisableHTTP2,
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
		SupportsStreaming bool   `db:"supports_streaming"  json:"supports_streaming"`
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
		{"provider_name": "openai_provider", "display_name": "OpenAI", "default_base_url": "https://api.openai.com"},
		{"provider_name": "anthropic_provider", "display_name": "Anthropic", "default_base_url": "https://api.anthropic.com"},
		{"provider_name": "google_provider", "display_name": "Google Gemini", "default_base_url": "https://generativelanguage.googleapis.com"},
		{"provider_name": "azure_openai_provider", "display_name": "Azure OpenAI", "default_base_url": ""},
		{"provider_name": "openrouter_provider", "display_name": "OpenRouter", "default_base_url": "https://openrouter.ai"},
		{"provider_name": "groq_provider", "display_name": "Groq", "default_base_url": "https://api.groq.com"},
		{"provider_name": "together_provider", "display_name": "Together AI", "default_base_url": "https://api.together.xyz"},
		{"provider_name": "mistral_provider", "display_name": "Mistral AI", "default_base_url": "https://api.mistral.ai"},
		{"provider_name": "cohere_provider", "display_name": "Cohere", "default_base_url": "https://api.cohere.com"},
		{"provider_name": "deepseek_provider", "display_name": "DeepSeek", "default_base_url": "https://api.deepseek.com"},
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
	// A soft-deleted model must not be silently re-enabled — that would set
	// enabled=TRUE while lifecycle stays 'deleted', which is exactly the
	// inconsistent state that let the reconciler and cold-start activator
	// treat a deleted model as legitimately desired (forensic audit, Case
	// File 003, round 6). Require an explicit redeploy instead.
	var lifecycle string
	if err := h.db.GetContext(c.Request.Context(), &lifecycle,
		`SELECT COALESCE(lifecycle,'active') FROM models WHERE id = $1`, modelID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "model not found: " + err.Error()})
		return
	}
	if lifecycle == "deleted" {
		c.JSON(http.StatusConflict, gin.H{"error": "model has been deleted — redeploy it instead of enabling the deleted record"})
		return
	}
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

// UpdateProviderTransport handles PUT /admin/v1/models/:id/transport
//
// Updates the per-provider HTTP transport configuration stored in the
// migration-046 columns on model_endpoints.
//
// Design:
//   - All fields are optional. Only provided (non-null pointer) fields are written.
//   - proxy_url is validated before writing — unsupported schemes are rejected with 400.
//   - After a successful DB update the registry is reloaded so the new per-endpoint
//     *http.Client is built immediately. No restart required.
//   - Changing one provider's transport never affects any other provider because
//     each endpoint has its own client (Registry.epClients).
//
// To clear proxy_url (revert to direct connection) send an explicit "".
// To disable TLS verification send tls_insecure_skip_verify: true.
func (h *RuntimeHandler) UpdateProviderTransport(c *gin.Context) {
	modelID := c.Param("id")

	var input struct {
		// ProxyURL is the outbound proxy for this provider only.
		// Supported schemes: http, https, socks5.
		// Credentials may be embedded: http://user:pass@host:port
		// Send "" to remove the proxy and connect directly.
		// Example: "socks5://192.168.0.207:3315"
		ProxyURL *string `json:"proxy_url"`

		// TLSInsecureSkipVerify disables server certificate verification.
		// Use only in controlled corporate environments with MITM proxies.
		TLSInsecureSkipVerify *bool `json:"tls_insecure_skip_verify"`

		// TLSRootCAPEM is a PEM-encoded root CA bundle appended to system roots.
		// Used when a corporate proxy presents a self-signed certificate.
		// Send "" to clear.
		TLSRootCAPEM *string `json:"tls_root_ca_pem"`

		// ConnectTimeoutSeconds: TCP dial + TLS handshake timeout.
		// 0 = use BuildProviderClient() default (10 s).
		ConnectTimeoutSeconds *int `json:"connect_timeout_seconds"`

		// ReadTimeoutSeconds: max time for a complete non-streaming response body.
		// 0 = unlimited (streaming-safe). Use context deadline for per-request limits.
		ReadTimeoutSeconds *int `json:"read_timeout_seconds"`

		// IdleConnTimeoutSeconds: keep-alive idle connection pool timeout.
		// 0 = use BuildProviderClient() default (90 s).
		IdleConnTimeoutSeconds *int `json:"idle_conn_timeout_seconds"`

		// ResponseHeaderTimeoutSeconds: max time to wait for response headers.
		// 0 = use BuildProviderClient() default (30 s). -1 = disabled.
		ResponseHeaderTimeoutSeconds *int `json:"response_header_timeout_seconds"`

		// MaxIdleConnsPerHost: idle connections in pool per host.
		// 0 = use BuildProviderClient() default (32).
		MaxIdleConnsPerHost *int `json:"max_idle_conns_per_host"`

		// MaxConnsPerHost: total connections (idle + active) per host.
		// 0 = unlimited.
		MaxConnsPerHost *int `json:"max_conns_per_host"`

		// DisableHTTP2: prevent HTTP/2 negotiation via ALPN.
		// Set true only for providers with known HTTP/2 compatibility issues.
		DisableHTTP2 *bool `json:"disable_http2"`
	}

	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// At least one field must be provided.
	if input.ProxyURL == nil && input.TLSInsecureSkipVerify == nil &&
		input.TLSRootCAPEM == nil && input.ConnectTimeoutSeconds == nil &&
		input.ReadTimeoutSeconds == nil && input.IdleConnTimeoutSeconds == nil &&
		input.ResponseHeaderTimeoutSeconds == nil && input.MaxIdleConnsPerHost == nil &&
		input.MaxConnsPerHost == nil && input.DisableHTTP2 == nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "at least one transport field must be provided",
			"fields": []string{
				"proxy_url", "tls_insecure_skip_verify", "tls_root_ca_pem",
				"connect_timeout_seconds", "read_timeout_seconds",
				"idle_conn_timeout_seconds", "response_header_timeout_seconds",
				"max_idle_conns_per_host", "max_conns_per_host", "disable_http2",
			},
		})
		return
	}

	// Validate proxy_url before touching the DB.
	if input.ProxyURL != nil {
		if err := runtime.ValidateProxyURL(*input.ProxyURL); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   err.Error(),
				"example": "http://proxy.corp:3128 or socks5://user:pass@proxy:1080 or \"\" to remove",
			})
			return
		}
	}

	// Apply each provided field individually so partial updates are safe.
	// All queries use WHERE model_id=$N so multi-endpoint models update all
	// endpoints — consistent with how UpdateUpstream behaves.
	ctx := c.Request.Context()

	if input.ProxyURL != nil {
		_, _ = h.db.ExecContext(ctx,
			`UPDATE model_endpoints
			 SET provider_proxy_url = NULLIF($1,''), updated_at = NOW()
			 WHERE model_id = $2`,
			*input.ProxyURL, modelID)
	}
	if input.TLSInsecureSkipVerify != nil {
		_, _ = h.db.ExecContext(ctx,
			`UPDATE model_endpoints
			 SET provider_tls_insecure_skip_verify = $1, updated_at = NOW()
			 WHERE model_id = $2`,
			*input.TLSInsecureSkipVerify, modelID)
	}
	if input.TLSRootCAPEM != nil {
		_, _ = h.db.ExecContext(ctx,
			`UPDATE model_endpoints
			 SET provider_tls_root_ca_pem = NULLIF($1,''), updated_at = NOW()
			 WHERE model_id = $2`,
			*input.TLSRootCAPEM, modelID)
	}
	if input.ConnectTimeoutSeconds != nil {
		_, _ = h.db.ExecContext(ctx,
			`UPDATE model_endpoints
			 SET provider_connect_timeout_seconds = $1, updated_at = NOW()
			 WHERE model_id = $2`,
			*input.ConnectTimeoutSeconds, modelID)
	}
	if input.ReadTimeoutSeconds != nil {
		_, _ = h.db.ExecContext(ctx,
			`UPDATE model_endpoints
			 SET provider_read_timeout_seconds = $1, updated_at = NOW()
			 WHERE model_id = $2`,
			*input.ReadTimeoutSeconds, modelID)
	}
	if input.IdleConnTimeoutSeconds != nil {
		_, _ = h.db.ExecContext(ctx,
			`UPDATE model_endpoints
			 SET provider_idle_conn_timeout_seconds = $1, updated_at = NOW()
			 WHERE model_id = $2`,
			*input.IdleConnTimeoutSeconds, modelID)
	}
	if input.ResponseHeaderTimeoutSeconds != nil {
		_, _ = h.db.ExecContext(ctx,
			`UPDATE model_endpoints
			 SET provider_response_header_timeout_seconds = $1, updated_at = NOW()
			 WHERE model_id = $2`,
			*input.ResponseHeaderTimeoutSeconds, modelID)
	}
	if input.MaxIdleConnsPerHost != nil {
		_, _ = h.db.ExecContext(ctx,
			`UPDATE model_endpoints
			 SET provider_max_idle_conns_per_host = $1, updated_at = NOW()
			 WHERE model_id = $2`,
			*input.MaxIdleConnsPerHost, modelID)
	}
	if input.MaxConnsPerHost != nil {
		_, _ = h.db.ExecContext(ctx,
			`UPDATE model_endpoints
			 SET provider_max_conns_per_host = $1, updated_at = NOW()
			 WHERE model_id = $2`,
			*input.MaxConnsPerHost, modelID)
	}
	if input.DisableHTTP2 != nil {
		_, _ = h.db.ExecContext(ctx,
			`UPDATE model_endpoints
			 SET provider_disable_http2 = $1, updated_at = NOW()
			 WHERE model_id = $2`,
			*input.DisableHTTP2, modelID)
	}

	// Reload the registry so BuildProviderClient() is called immediately with
	// the new config. The old per-endpoint *http.Client is replaced atomically.
	_ = h.registry.Reload(ctx)

	// Build a summary of what changed for the response.
	changed := map[string]interface{}{}
	if input.ProxyURL != nil {
		if *input.ProxyURL == "" {
			changed["proxy_url"] = "cleared (direct connection)"
		} else {
			changed["proxy_url"] = *input.ProxyURL
		}
	}
	if input.TLSInsecureSkipVerify != nil {
		changed["tls_insecure_skip_verify"] = *input.TLSInsecureSkipVerify
	}
	if input.TLSRootCAPEM != nil {
		if *input.TLSRootCAPEM == "" {
			changed["tls_root_ca_pem"] = "cleared"
		} else {
			changed["tls_root_ca_pem"] = fmt.Sprintf("%d bytes", len(*input.TLSRootCAPEM))
		}
	}
	if input.ConnectTimeoutSeconds != nil {
		changed["connect_timeout_seconds"] = *input.ConnectTimeoutSeconds
	}
	if input.ReadTimeoutSeconds != nil {
		changed["read_timeout_seconds"] = *input.ReadTimeoutSeconds
	}
	if input.IdleConnTimeoutSeconds != nil {
		changed["idle_conn_timeout_seconds"] = *input.IdleConnTimeoutSeconds
	}
	if input.ResponseHeaderTimeoutSeconds != nil {
		changed["response_header_timeout_seconds"] = *input.ResponseHeaderTimeoutSeconds
	}
	if input.MaxIdleConnsPerHost != nil {
		changed["max_idle_conns_per_host"] = *input.MaxIdleConnsPerHost
	}
	if input.MaxConnsPerHost != nil {
		changed["max_conns_per_host"] = *input.MaxConnsPerHost
	}
	if input.DisableHTTP2 != nil {
		changed["disable_http2"] = *input.DisableHTTP2
	}

	c.JSON(http.StatusOK, gin.H{
		"message":  "provider transport config updated — new HTTP client built immediately",
		"model_id": modelID,
		"changed":  changed,
		"note":     "transport isolation is guaranteed — only this provider's client was rebuilt",
	})
}

// GetProviderTransport handles GET /admin/v1/models/:id/transport
//
// Returns the current per-provider transport configuration for all endpoints
// of the given model. The proxy_url is returned in full so operators can verify
// the configuration. The upstream_api_key is never returned.
func (h *RuntimeHandler) GetProviderTransport(c *gin.Context) {
	modelID := c.Param("id")

	type transportRow struct {
		EndpointID                   string  `db:"id"                                       json:"endpoint_id"`
		ProviderProxyURL             *string `db:"provider_proxy_url"                       json:"proxy_url"`
		TLSInsecureSkipVerify        bool    `db:"provider_tls_insecure_skip_verify"        json:"tls_insecure_skip_verify"`
		TLSRootCAPEMSet              bool    `db:"tls_root_ca_pem_set"                      json:"tls_root_ca_pem_set"`
		ConnectTimeoutSeconds        int     `db:"provider_connect_timeout_seconds"         json:"connect_timeout_seconds"`
		ReadTimeoutSeconds           int     `db:"provider_read_timeout_seconds"            json:"read_timeout_seconds"`
		IdleConnTimeoutSeconds       int     `db:"provider_idle_conn_timeout_seconds"       json:"idle_conn_timeout_seconds"`
		ResponseHeaderTimeoutSeconds int     `db:"provider_response_header_timeout_seconds" json:"response_header_timeout_seconds"`
		MaxIdleConnsPerHost          int     `db:"provider_max_idle_conns_per_host"         json:"max_idle_conns_per_host"`
		MaxConnsPerHost              int     `db:"provider_max_conns_per_host"              json:"max_conns_per_host"`
		DisableHTTP2                 bool    `db:"provider_disable_http2"                   json:"disable_http2"`
		// Legacy field shown for awareness.
		UpstreamProxy *string `db:"upstream_proxy" json:"upstream_proxy_legacy,omitempty"`
	}

	var rows []transportRow
	err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT id,
		       provider_proxy_url,
		       provider_tls_insecure_skip_verify,
		       (provider_tls_root_ca_pem IS NOT NULL AND provider_tls_root_ca_pem != '') AS tls_root_ca_pem_set,
		       provider_connect_timeout_seconds,
		       provider_read_timeout_seconds,
		       provider_idle_conn_timeout_seconds,
		       provider_response_header_timeout_seconds,
		       provider_max_idle_conns_per_host,
		       provider_max_conns_per_host,
		       provider_disable_http2,
		       upstream_proxy
		FROM model_endpoints
		WHERE model_id = $1
		ORDER BY priority`, modelID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if len(rows) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "no endpoints found for model " + modelID})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"model_id":  modelID,
		"endpoints": rows,
		"count":     len(rows),
		"note":      "zero values mean BuildProviderClient() defaults apply (connect=10s idle=90s response_header=30s pool=32)",
	})
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
		"message":        "upstream config updated",
		"model_id":       modelID,
		"proxy_set":      input.Proxy != nil && *input.Proxy != "",
		"model_name_set": input.ModelName != nil && *input.ModelName != "",
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
		UpstreamAPIKeySet bool `db:"upstream_api_key_set" json:"upstream_api_key_set"`
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
	// deployment_mode tells the caller how to read an unhealthy endpoint: for a
	// managed model NexusLLM will bring it back itself, for a manual one the
	// operator has to start the container.
	deploymentMode := modelguard.ModeManaged
	_ = h.db.GetContext(c.Request.Context(), &deploymentMode,
		`SELECT COALESCE(deployment_mode,'managed') FROM models WHERE id = $1`, modelID)
	// Return 200 with empty endpoints list instead of 404
	c.JSON(http.StatusOK, gin.H{
		"model_id":                   modelID,
		"endpoints":                  rows,
		"count":                      len(rows),
		"deployment_mode":            deploymentMode,
		"lifecycle_managed_by_nexus": modelguard.ManagedByNexus(deploymentMode),
	})
}

func (h *RuntimeHandler) ListModels(c *gin.Context) {
	lifecycle := c.Query("lifecycle")
	if lifecycle == "" {
		lifecycle = "active"
	}

	type mRow struct {
		ID                string `db:"id"                  json:"id"`
		Name              string `db:"name"                json:"name"`
		DisplayName       string `db:"display_name"        json:"display_name"`
		Provider          string `db:"provider"            json:"provider"`
		BackendType       string `db:"backend_type"        json:"backend_type"`
		ServiceType       string `db:"service_type"        json:"service_type"`
		MaxContext        int    `db:"max_context"         json:"max_context"`
		MaxOutput         int    `db:"max_output"          json:"max_output"`
		Enabled           bool   `db:"enabled"             json:"enabled"`
		Lifecycle         string `db:"lifecycle"           json:"lifecycle"`
		DeploymentMode    string `db:"deployment_mode"     json:"deployment_mode"`
		EndpointCnt       int    `db:"endpoint_cnt"        json:"endpoint_count"`
		HealthyCnt        int    `db:"healthy_cnt"         json:"healthy_count"`
		SupportsThinking  bool   `db:"supports_thinking"   json:"supports_thinking"`
		ThinkingEnabled   bool   `db:"thinking_enabled"    json:"thinking_enabled"`
		MinThinkingTokens int    `db:"min_thinking_tokens" json:"min_thinking_tokens"`
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
		       COALESCE(m.deployment_mode,'managed') AS deployment_mode,
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

// SetDeploymentMode handles PUT /admin/v1/models/:id/deployment-mode
//
// Switches who owns the model's container (models.deployment_mode, migration
// 061). This is the escape hatch for a model that was registered one way and
// is really run the other way — typically a model NexusLLM was trying to
// cold-start while the operator was already running the container themselves
// with docker compose.
//
//	{"deployment_mode": "manual"}   — hands the container back to the operator:
//	    no cold start, no idle eviction, no HA replacement, no preemption, and
//	    admin start/stop/restart return 409. Health checks keep running, so a
//	    container that is down simply shows as unhealthy.
//	{"deployment_mode": "managed"}  — hands the container lifecycle to NexusLLM
//	    again. Requires a runtime config (image + node) to be startable.
//
// Switching to manual deliberately leaves any runtime rows and the container
// itself alone: NexusLLM stops managing the container, it does not stop it.
// Stop it with the admin stop endpoint BEFORE switching if that is what you
// want, otherwise NexusLLM keeps routing to whatever is listening there.
func (h *RuntimeHandler) SetDeploymentMode(c *gin.Context) {
	modelID := c.Param("id")
	var input struct {
		DeploymentMode string `json:"deployment_mode" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.DeploymentMode != modelguard.ModeManaged && input.DeploymentMode != modelguard.ModeManual {
		c.JSON(http.StatusBadRequest, gin.H{"error": "deployment_mode must be 'managed' or 'manual'"})
		return
	}

	var modelName string
	err := h.db.QueryRowContext(c.Request.Context(),
		`UPDATE models SET deployment_mode = $1, updated_at = NOW() WHERE id = $2 RETURNING name`,
		input.DeploymentMode, modelID,
	).Scan(&modelName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Count what is still running under NexusLLM's control, so the operator
	// knows whether a container is still up after handing ownership over.
	var liveRuntimes int
	_ = h.db.GetContext(c.Request.Context(), &liveRuntimes, `
		SELECT COUNT(*) FROM agent_runtimes
		WHERE model_id = $1 AND state IN ('ready','active','warm','idle','loading_model','starting','pending')`,
		modelID)

	h.log.Info("model deployment mode changed",
		zap.String("model", modelName),
		zap.String("deployment_mode", input.DeploymentMode),
		zap.Int("live_runtimes", liveRuntimes),
	)

	resp := gin.H{
		"model_id":        modelID,
		"model_name":      modelName,
		"deployment_mode": input.DeploymentMode,
	}
	if input.DeploymentMode == modelguard.ModeManual {
		resp["note"] = "NexusLLM no longer manages this container: no cold start, idle eviction, HA replacement " +
			"or preemption. It still routes to the endpoint and health-checks it."
		if liveRuntimes > 0 {
			resp["warning"] = fmt.Sprintf(
				"%d runtime(s) NexusLLM started for this model are still running — they are now unmanaged; "+
					"stop them on the host if you did not mean to keep them", liveRuntimes)
		}
	} else {
		resp["note"] = "NexusLLM manages this container again — it may be cold-started, idle-evicted and replaced. " +
			"Make sure no container you started yourself is still bound to the endpoint's port."
	}
	c.JSON(http.StatusOK, resp)
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
//
// Performs a SOFT DELETE: sets lifecycle='deleted' and enabled=FALSE rather
// than issuing a hard DELETE FROM models.  This preserves the model UUID row
// in the database so that:
//
//	a) team_model_permissions rows are NOT cascade-deleted (migration 053
//	   changed that FK to ON DELETE RESTRICT).
//	b) The DB trigger fn_snapshot_model_permissions fires on the lifecycle
//	   UPDATE and saves a snapshot of all current team grants into
//	   model_permission_snapshots, keyed by model name.  The next call to
//	   DeployModel / RegisterModel / RegisterExternalModel with the same name
//	   will find that snapshot and automatically restore the grants.
//	c) usage_events / audit_logs retain a valid foreign-key reference.
//
// If a hard purge is genuinely required (GDPR, storage cleanup) it must be
// done explicitly via a separate admin purge endpoint that checks for active
// runtimes first — not via this normal delete path.
//
// Does NOT stop running containers — call stop/drain before deleting.
func (h *RuntimeHandler) DeleteModel(c *gin.Context) {
	ctx := c.Request.Context()
	modelID := c.Param("id")

	// Verify the model exists and is not already soft-deleted.
	var modelName string
	var currentLifecycle string
	err := h.db.QueryRowContext(ctx,
		`SELECT name, COALESCE(lifecycle,'active') FROM models WHERE id = $1`, modelID,
	).Scan(&modelName, &currentLifecycle)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
		return
	}
	if currentLifecycle == "deleted" {
		c.JSON(http.StatusConflict, gin.H{"error": "model is already deleted"})
		return
	}

	// Disable all endpoints first so the gateway stops routing immediately.
	// The registry reload below removes the model from the in-memory pool.
	_, _ = h.db.ExecContext(ctx, `
		UPDATE model_endpoints
		SET is_enabled = FALSE, health_status = 'down', updated_at = NOW()
		WHERE model_id = $1`, modelID)

	// Soft-delete: the DB trigger fn_snapshot_model_permissions fires here and
	// writes the team grant snapshot to model_permission_snapshots.
	res, err := h.db.ExecContext(ctx, `
		UPDATE models
		SET lifecycle = 'deleted', enabled = FALSE, updated_at = NOW()
		WHERE id = $1 AND lifecycle != 'deleted'`, modelID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to soft-delete model: " + err.Error()})
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "model not found or already deleted"})
		return
	}

	// Audit trail.
	_, _ = h.db.ExecContext(ctx, `
		INSERT INTO model_lifecycle_events_v2 (model_id, from_state, to_state, reason, actor)
		VALUES ($1, $2, 'deleted', 'admin soft-deleted', 'admin')`,
		modelID, currentLifecycle)

	// Remove from registry hot-path immediately.
	_ = h.registry.Reload(ctx)

	h.log.Info("model soft-deleted",
		zap.String("model_id", modelID),
		zap.String("model_name", modelName),
		zap.String("previous_lifecycle", currentLifecycle),
	)
	c.JSON(http.StatusOK, gin.H{
		"message":  "model soft-deleted — permissions snapshot saved for restore on redeploy",
		"model_id": modelID,
		"name":     modelName,
	})
}

// restorePermissionsFromSnapshot looks up the most recent unrestored permission
// snapshot for modelName and re-inserts team_model_permissions rows for newModelID.
//
// This is called immediately after a new model row is inserted during
// DeployModel / RegisterModel / RegisterExternalModel so that a redeploy of a
// previously deleted model automatically recovers all team grants without
// requiring the admin to re-grant each team manually.
//
// If no snapshot exists (first-ever deployment of this name) this returns
// (nil, nil) — not an error. A non-nil error means NOTHING changed: no
// team_model_permissions row from this attempt survives, no Redis key was
// touched, and the snapshot remains available (restored=FALSE) for a future
// retry. Callers must not treat a non-nil error as a partial success.
func (h *RuntimeHandler) restorePermissionsFromSnapshot(ctx context.Context, modelName, newModelID string) ([]string, error) {
	return restorePermissionsFromSnapshot(ctx, h.db, h.engine, h.log, modelName, newModelID)
}

// restorePermissionsFromSnapshot is the package-level implementation shared by
// RuntimeHandler and CatalogHandler.
//
// Phase 2A/2B fix (forensic audit, Case File 002 §02 — the two confirmed,
// schema-independent bugs; the cross-tenant model-name collision itself,
// §02.B, is explicitly NOT addressed here and remains open, requiring the
// separate org/project architecture migration):
//
//  1. Redis enforcement sync (2A). The previous version wrote
//     team_model_permissions directly to Postgres and never touched the
//     Redis ACL set the gateway's policy engine actually enforces against —
//     so a restored grant showed up in the database and the admin panel, but
//     silently denied every real inference request. This version restores
//     each team through the SAME canonical function the admin grant endpoint
//     uses — engine.SetModelAllowed — never a duplicate rdb.SAdd call, so
//     there is exactly one code path that can ever write that Redis set.
//
//  2. Atomicity (2B). The entire operation — locking the snapshot, inserting
//     team_model_permissions rows, syncing Redis, and marking the snapshot
//     restored — now happens inside one transaction. SELECT ... FOR UPDATE
//     takes a row lock on the snapshot so a concurrent restore attempt for
//     the same model name blocks instead of racing; once the first commits,
//     the second finds restored=TRUE already and treats it as "no snapshot"
//     (see the re-check after acquiring the lock). Any failure — a bad
//     team_id, a Redis error, a failed final UPDATE — rolls back everything:
//     Postgres and Redis either both change together or neither does.
func restorePermissionsFromSnapshot(ctx context.Context, db *sqlx.DB, engine *policy.Engine, log *zap.Logger, modelName, newModelID string) ([]string, error) {
	if log == nil {
		log = zap.NewNop()
	}

	tx, err := db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("restore permissions: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Lock the most recent unconsumed snapshot row for this name. FOR UPDATE
	// blocks a concurrent restore attempt targeting the SAME row until this
	// transaction commits or rolls back. Once unblocked, Postgres re-checks
	// the row against the WHERE clause using its now-current (committed)
	// value — if the first transaction already consumed it, restored is now
	// TRUE, the row no longer matches, and this query correctly returns
	// sql.ErrNoRows instead of restoring the same snapshot twice.
	var snapID string
	var teamIDsJSON string
	var projectIDsJSON string
	err = tx.QueryRowContext(ctx, `
		SELECT id::text, team_ids::text, project_ids::text
		FROM model_permission_snapshots
		WHERE model_name = $1 AND restored = FALSE
		ORDER BY deleted_at DESC
		LIMIT 1
		FOR UPDATE`, modelName,
	).Scan(&snapID, &teamIDsJSON, &projectIDsJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// No unconsumed snapshot — first-ever deployment of this name, or
			// already restored by a concurrent/prior attempt. Not an error.
			return nil, nil
		}
		return nil, fmt.Errorf("restore permissions: lock snapshot: %w", err)
	}

	// Parse the JSON array of team UUIDs.
	var teamIDs []string
	if jsonErr := json.Unmarshal([]byte(teamIDsJSON), &teamIDs); jsonErr != nil || len(teamIDs) == 0 {
		log.Warn("permission snapshot found but team_ids is empty or unparseable",
			zap.String("snapshot_id", snapID),
			zap.String("model_name", modelName),
		)
		return nil, nil
	}

	// Re-insert one team_model_permissions row per team, inside the tx. Any
	// failure aborts the whole restore rather than silently skipping that
	// team.
	//
	// SECURITY/CONSISTENCY (forensic audit, production-readiness round):
	// this used to call engine.SetModelAllowed (a real Redis network call)
	// from WITHIN this still-open transaction, before commit — with a
	// doc-comment claiming "Postgres and Redis either both change together
	// or neither does". That claim was false whenever a snapshot covered
	// more than one team/project: if an EARLIER team's Redis write already
	// succeeded and a LATER one then failed, the deferred rollback undid
	// every Postgres INSERT in this function, but the earlier team's
	// already-executed Redis SAdd was never compensated — Postgres ends up
	// saying "no grant", Redis says "this team can access it".
	//
	// Fixed: ALL Postgres writes (team + project INSERTs, and marking the
	// snapshot restored) now happen and COMMIT as one all-or-nothing unit,
	// with zero Redis calls inside the transaction. Redis synchronization
	// happens strictly AFTER commit, below. This makes rollback-leaves-
	// stray-Redis-state structurally impossible (there is nothing to roll
	// back by the time Redis is ever touched) — at the cost of accepting a
	// different, already-detectable failure mode: Postgres commits but a
	// post-commit Redis sync call fails. That residual case is exactly what
	// the reconciliation mechanism (ReconcilePermissions) exists to find and
	// fix — see internal/admin/handlers/reconcile.go — rather than trying
	// (and previously failing) to prevent it via in-transaction Redis calls.
	var restoredTeamIDs []string
	for _, teamID := range teamIDs {
		teamID = strings.TrimSpace(teamID)
		if teamID == "" {
			continue
		}
		if _, insertErr := tx.ExecContext(ctx, `
			INSERT INTO team_model_permissions (team_id, model_id)
			VALUES ($1::uuid, $2::uuid)
			ON CONFLICT DO NOTHING`,
			teamID, newModelID,
		); insertErr != nil {
			return nil, fmt.Errorf("restore permissions: insert team_model_permissions (team %s): %w", teamID, insertErr)
		}
		restoredTeamIDs = append(restoredTeamIDs, teamID)
	}

	// Same restore for project-level grants (migration 058), same
	// all-or-nothing guarantee, same transaction.
	var projectIDs []string
	if jsonErr := json.Unmarshal([]byte(projectIDsJSON), &projectIDs); jsonErr != nil {
		log.Warn("permission snapshot found but project_ids is unparseable — continuing with team restore only",
			zap.String("snapshot_id", snapID),
			zap.String("model_name", modelName),
			zap.Error(jsonErr),
		)
		projectIDs = nil
	}
	var restoredProjectIDs []string
	for _, projectID := range projectIDs {
		projectID = strings.TrimSpace(projectID)
		if projectID == "" {
			continue
		}
		if _, insertErr := tx.ExecContext(ctx, `
			INSERT INTO project_model_permissions (project_id, model_id)
			VALUES ($1::uuid, $2::uuid)
			ON CONFLICT DO NOTHING`,
			projectID, newModelID,
		); insertErr != nil {
			return nil, fmt.Errorf("restore permissions: insert project_model_permissions (project %s): %w", projectID, insertErr)
		}
		restoredProjectIDs = append(restoredProjectIDs, projectID)
	}

	// Mark snapshot as consumed, in the SAME transaction as the INSERTs
	// above — this is the last Postgres statement before commit; nothing
	// after this point can partially apply.
	if _, err := tx.ExecContext(ctx, `
		UPDATE model_permission_snapshots
		SET restored = TRUE, restored_at = NOW()
		WHERE id = $1::uuid`, snapID,
	); err != nil {
		return nil, fmt.Errorf("restore permissions: mark snapshot restored: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("restore permissions: commit: %w", err)
	}
	committed = true

	// ── Post-commit: synchronize Redis ────────────────────────────────────
	// Postgres is now durably the source of truth regardless of what happens
	// below. A failure here is reported to the caller (so it's logged, not
	// silently swallowed) but does NOT undo the commit above — the
	// permission genuinely exists and the next ReconcilePermissions sweep
	// (internal/admin/handlers/reconcile.go) will detect and repair the
	// Redis-side gap on its own, without needing another redeploy.
	var syncErrs []error
	if len(restoredTeamIDs) > 0 {
		if engine == nil {
			syncErrs = append(syncErrs, fmt.Errorf("no policy engine configured — %d team grant(s) committed to Postgres but not synced to Redis", len(restoredTeamIDs)))
		} else {
			for _, teamID := range restoredTeamIDs {
				if syncErr := engine.SetModelAllowed(ctx, teamID, modelName); syncErr != nil {
					syncErrs = append(syncErrs, fmt.Errorf("sync team %s to gateway enforcement: %w", teamID, syncErr))
				}
			}
		}
	}
	if len(restoredProjectIDs) > 0 {
		if engine == nil {
			syncErrs = append(syncErrs, fmt.Errorf("no policy engine configured — %d project grant(s) committed to Postgres but not synced to Redis", len(restoredProjectIDs)))
		} else {
			for _, projectID := range restoredProjectIDs {
				if syncErr := engine.SetProjectModelAllowed(ctx, projectID, modelName); syncErr != nil {
					syncErrs = append(syncErrs, fmt.Errorf("sync project %s to gateway enforcement: %w", projectID, syncErr))
				}
			}
		}
	}

	if len(restoredTeamIDs) > 0 || len(restoredProjectIDs) > 0 {
		log.Info("restored team/project permissions from a previous deletion of this model name — "+
			"verify these are actually expected to have access before assuming this is routine",
			zap.String("model_name", modelName),
			zap.String("new_model_id", newModelID),
			zap.Strings("restored_team_ids", restoredTeamIDs),
			zap.Strings("restored_project_ids", restoredProjectIDs),
		)
	}

	if len(syncErrs) > 0 {
		log.Error("permissions committed to Postgres but Redis synchronization failed for some entries — "+
			"gateway enforcement is temporarily out of sync; the periodic reconciliation sweep will repair this",
			zap.String("model_name", modelName),
			zap.Errors("sync_errors", syncErrs),
		)
		return restoredTeamIDs, fmt.Errorf("restore permissions: committed to database but %d Redis sync error(s) occurred (will self-heal via reconciliation): %w",
			len(syncErrs), errors.Join(syncErrs...))
	}

	return restoredTeamIDs, nil
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
