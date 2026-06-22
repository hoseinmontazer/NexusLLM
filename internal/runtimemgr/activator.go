// Package runtimemgr — activator.go
//
// Activator.EnsureRunning() is the heart of the lazy-load architecture.
// It is called by the proxy on every request where the registry returns no
// healthy endpoint for the requested model.
//
// Sequence:
//   1. Look up the model's config row in the DB.
//   2. Check the current state of the agent_runtime row (if any).
//   3. If model file not on node → dispatch PULL_MODEL, wait for completion.
//   4. If container doesn't exist → dispatch DEPLOY_RUNTIME.
//   5. If container stopped → dispatch WARM_RUNTIME (docker start).
//   6. Poll DB every HealthPollInterval until lifecycle_state becomes active/warm.
//   7. Trigger registry.Reload() so the gateway starts routing to it.
//   8. Return the endpoint URL so the proxy can forward the request immediately.
package runtimemgr

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/runtime"
	"github.com/nexusllm/nexusllm/internal/taskmanager"
	"go.uber.org/zap"
)

// RuntimeActivator implements the Activator interface.
// It is safe for concurrent use — multiple goroutines calling EnsureRunning
// for the same model are deduplicated via an in-memory singleflight map.
type RuntimeActivator struct {
	db       *sqlx.DB
	taskMgr  *taskmanager.Manager
	registry *runtime.Registry
	guard    *ResourceGuard
	cfg      Config
	log      *zap.Logger

	// inflight deduplicates concurrent EnsureRunning calls for the same model.
	// Key: modelName, Value: chan (closed when model becomes ready or fails).
	inflight inflightMap
}

// NewActivator constructs a RuntimeActivator.
func NewActivator(
	db *sqlx.DB,
	taskMgr *taskmanager.Manager,
	registry *runtime.Registry,
	guard *ResourceGuard,
	cfg Config,
	log *zap.Logger,
) *RuntimeActivator {
	return &RuntimeActivator{
		db:       db,
		taskMgr:  taskMgr,
		registry: registry,
		guard:    guard,
		cfg:      cfg,
		log:      log,
		inflight: newInflightMap(),
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// EnsureRunning
// ─────────────────────────────────────────────────────────────────────────────

// EnsureRunning guarantees the named model is healthy before returning.
// It deduplicates concurrent callers: if ten requests arrive simultaneously
// for a cold model, only one start sequence runs; the other nine wait.
func (a *RuntimeActivator) EnsureRunning(ctx context.Context, modelName string) (*RunningEndpoint, error) {
	start := time.Now()

	// Deduplicate: if another goroutine is already starting this model, wait.
	ch, owner := a.inflight.getOrCreate(modelName)
	if !owner {
		// We are a waiter — block until the owner finishes.
		select {
		case <-ch:
			// Owner finished (success or failure). Try registry first.
			if ep, _, err := a.registry.ResolveWithFailover(modelName, 3); err == nil {
				return &RunningEndpoint{
					EndpointID: ep.ID,
					URL:        ep.URL,
					WarmupMs:   time.Since(start).Milliseconds(),
				}, nil
			}
			// Owner may have failed — fall through to our own attempt.
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// We are the owner; release the channel when done so waiters unblock.
	defer a.inflight.release(modelName, ch)

	return a.doEnsureRunning(ctx, modelName, start)
}

// doEnsureRunning is the main logic, called by the inflight owner.
func (a *RuntimeActivator) doEnsureRunning(ctx context.Context, modelName string, startTime time.Time) (*RunningEndpoint, error) {
	// Apply the cold-start timeout — we have up to ColdStartTimeout total.
	ctx, cancel := context.WithTimeout(ctx, a.cfg.ColdStartTimeout)
	defer cancel()

	// 1. Load model config from DB.
	cfg, err := a.loadConfig(ctx, modelName)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrModelNotFound, err.Error())
	}
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("%w: model %q has no node assigned — set node_id on the endpoint or deploy via a node agent", ErrModelNotFound, modelName)
	}

	// 2. Load existing runtime row (if any).
	rt, err := a.loadRuntime(ctx, cfg.ModelID, cfg.NodeID)
	if err != nil {
		// No existing runtime row — need a full cold start.
		rt = nil
	}

	// 3. Resource pre-flight check.
	ramMB := a.estimateRAMMB(cfg)
	if guardErr := a.guard.CanStart(ctx, cfg.NodeID, ResourceRequest{
		ModelName:   modelName,
		RAMMBNeeded: ramMB,
		GPUDevices:  cfg.GPUDevices,
	}); guardErr != nil {
		return nil, guardErr
	}

	// 4. Determine what needs to happen.
	state := a.deriveState(rt)
	a.log.Info("EnsureRunning",
		zap.String("model", modelName),
		zap.String("state", string(state)),
	)

	switch state {
	case StateNotRegistered, StateUnknown:
		// Need download + fresh deploy.
		if err := a.downloadIfNeeded(ctx, cfg); err != nil {
			return nil, err
		}
		if err := a.deployFresh(ctx, cfg); err != nil {
			return nil, err
		}

	case StateDownloading:
		// Another process is already downloading — wait for it.
		if err := a.waitForState(ctx, cfg.ModelID, cfg.NodeID, []string{"loading", "warm", "active"}, []string{"failed"}); err != nil {
			return nil, fmt.Errorf("%w: download in progress failed: %s", ErrDownloadFailed, err.Error())
		}
		// After download, start container.
		if err := a.deployFresh(ctx, cfg); err != nil {
			return nil, err
		}

	case StateStopped:
		// Container exists, just needs docker start.
		if err := a.warmRuntime(ctx, cfg, rt); err != nil {
			return nil, err
		}

	case StateStarting, StateLoading:
		// Already starting — just wait.
		a.log.Info("model already starting, waiting for healthy", zap.String("model", modelName))

	case StateReady, StateIdle:
		// Already running — registry reload and return.
		_ = a.registry.Reload(ctx)
		ep, _, err := a.registry.ResolveWithFailover(modelName, 3)
		if err == nil {
			return &RunningEndpoint{
				EndpointID: ep.ID,
				URL:        ep.URL,
				WarmupMs:   0,
			}, nil
		}
		// Health check may have just expired — fall through to wait loop.

	case StateFailed:
		// Previous attempt failed — try a fresh deploy.
		a.log.Warn("model was in failed state, re-deploying", zap.String("model", modelName))
		if err := a.deployFresh(ctx, cfg); err != nil {
			return nil, err
		}
	}

	// 5. Wait until the endpoint is healthy.
	ep, err := a.waitForHealthy(ctx, cfg, startTime)
	if err != nil {
		return nil, err
	}
	return ep, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Step implementations
// ─────────────────────────────────────────────────────────────────────────────

// downloadIfNeeded checks node_model_cache; if the GGUF is absent, enqueues
// PULL_MODEL and waits for it to complete.
func (a *RuntimeActivator) downloadIfNeeded(ctx context.Context, cfg *ModelConfig) error {
	if cfg.GGUFPath == "" && cfg.HFRepo == "" {
		// No source configured — skip download step.
		return nil
	}

	// Check cache.
	var count int
	_ = a.db.GetContext(ctx, &count, `
		SELECT COUNT(*) FROM node_model_cache
		WHERE node_id = $1 AND model_ref = $2 AND is_cached = TRUE`,
		cfg.NodeID, a.modelRef(cfg))

	if count > 0 {
		a.log.Debug("model already cached on node", zap.String("model", cfg.ModelName))
		return nil
	}

	a.log.Info("model not cached — dispatching PULL_MODEL",
		zap.String("model", cfg.ModelName),
		zap.String("hf_repo", cfg.HFRepo),
	)

	vol := cfg.ModelsVolume
	if vol == "" {
		vol = a.cfg.DefaultModelsVolume
	}

	payload := taskmanager.PullModelPayload{
		ModelID:   cfg.ModelID,
		HFRepo:    cfg.HFRepo,
		HFToken:   cfg.HFToken,
		LocalPath: vol,
		Backend:   "llamacpp",
	}

	taskID, err := a.taskMgr.Enqueue(ctx, cfg.NodeID,
		taskmanager.TaskPullModel, payload,
		taskmanager.WithPriority(80),
		taskmanager.WithActor("runtimemgr"),
		taskmanager.WithTimeout(30*time.Minute), // downloads can be slow
		taskmanager.WithIdempotencyKey("pull:"+cfg.NodeID+":"+a.modelRef(cfg)),
	)
	if err != nil {
		return fmt.Errorf("enqueue PULL_MODEL: %w", err)
	}

	a.log.Info("PULL_MODEL enqueued", zap.String("task_id", taskID))

	// Wait for the task to reach success or failed.
	return a.waitForTask(ctx, taskID)
}

// deployFresh creates a new agent_runtime row and enqueues DEPLOY_RUNTIME.
func (a *RuntimeActivator) deployFresh(ctx context.Context, cfg *ModelConfig) error {
	runtimeID := uuid.New().String()
	containerName := "nexus-" + cfg.ModelName

	image := cfg.Image
	if image == "" {
		image = a.cfg.DefaultImage
	}
	vol := cfg.ModelsVolume
	if vol == "" {
		vol = a.cfg.DefaultModelsVolume
	}
	ctxSize := cfg.CtxSize
	if ctxSize == 0 {
		ctxSize = 4096
	}

	// Insert agent_runtime row in pending state.
	_, err := a.db.ExecContext(ctx, `
		INSERT INTO agent_runtimes
		  (id, node_id, endpoint_id, model_id, runtime_name, backend,
		   state, gpu_ids, bind_host, bind_port, cpu_affinity, numa_node)
		SELECT $1, $2, me.id, $3, $4, 'llamacpp', 'pending', '[]',
		       me.host, me.port, $5, $6
		FROM model_endpoints me
		WHERE me.model_id = $3
		  AND me.lifecycle_state NOT IN ('deleted')
		LIMIT 1
		ON CONFLICT DO NOTHING`,
		runtimeID, cfg.NodeID, cfg.ModelID, containerName,
		cfg.CPUSetCPUs, cfg.NUMANode,
	)
	if err != nil {
		return fmt.Errorf("insert agent_runtime: %w", err)
	}

	payload := taskmanager.DeployRuntimePayload{
		RuntimeID:            runtimeID,
		ModelID:              cfg.ModelID,
		RuntimeName:          containerName,
		Backend:              "llamacpp",
		Image:                image,
		ModelName:            cfg.ModelName,
		ServedAs:             cfg.ModelName,
		GPUDevices:           cfg.GPUDevices,
		CPUSetCPUs:           cfg.CPUSetCPUs,
		NUMANode:             cfg.NUMANode,
		HFToken:              cfg.HFToken,
		LlamaCppModelPath:    cfg.GGUFPath,
		LlamaCppHFRepo:       cfg.HFRepo,
		LlamaCppHFFile:       cfg.HFFile,
		LlamaCppCtxSize:      ctxSize,
		LlamaCppNGPULayers:   cfg.NGPULayers,
		LlamaCppModelsVolume: vol,
		Env:                  map[string]string{},
	}
	if cfg.MemoryLimit != "" {
		payload.MemoryLimit = cfg.MemoryLimit
	}
	if cfg.CPUThreads != "" {
		payload.CPULimit = cfg.CPUThreads
	}

	taskID, err := a.taskMgr.Enqueue(ctx, cfg.NodeID,
		taskmanager.TaskDeployRuntime, payload,
		taskmanager.WithPriority(85),
		taskmanager.WithActor("runtimemgr"),
		taskmanager.WithRuntimeID(runtimeID),
		taskmanager.WithIdempotencyKey("deploy:"+cfg.NodeID+":"+cfg.ModelID),
	)
	if err != nil {
		return fmt.Errorf("enqueue DEPLOY_RUNTIME: %w", err)
	}
	a.log.Info("DEPLOY_RUNTIME enqueued",
		zap.String("model", cfg.ModelName),
		zap.String("task_id", taskID),
		zap.String("runtime_id", runtimeID),
	)
	return nil
}

// warmRuntime enqueues WARM_RUNTIME (docker start on a stopped container).
func (a *RuntimeActivator) warmRuntime(ctx context.Context, cfg *ModelConfig, rt *agentRuntime) error {
	payload := taskmanager.WarmRuntimePayload{
		RuntimeID:   rt.ID,
		ContainerID: rt.ContainerID,
	}
	taskID, err := a.taskMgr.Enqueue(ctx, cfg.NodeID,
		taskmanager.TaskWarmRuntime, payload,
		taskmanager.WithPriority(85),
		taskmanager.WithActor("runtimemgr"),
		taskmanager.WithRuntimeID(rt.ID),
		taskmanager.WithIdempotencyKey("warm:"+cfg.NodeID+":"+rt.ID),
	)
	if err != nil {
		return fmt.Errorf("enqueue WARM_RUNTIME: %w", err)
	}
	a.log.Info("WARM_RUNTIME enqueued",
		zap.String("model", cfg.ModelName),
		zap.String("task_id", taskID),
	)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Polling helpers
// ─────────────────────────────────────────────────────────────────────────────

// waitForTask polls agent_tasks until the task reaches success or failed/timeout.
func (a *RuntimeActivator) waitForTask(ctx context.Context, taskID string) error {
	ticker := time.NewTicker(a.cfg.HealthPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled while waiting for task %s: %w", taskID, ctx.Err())
		case <-ticker.C:
			task, err := a.taskMgr.GetTask(ctx, taskID)
			if err != nil {
				continue
			}
			switch task.Status {
			case taskmanager.StatusSuccess:
				return nil
			case taskmanager.StatusFailed, taskmanager.StatusTimeout, taskmanager.StatusCancelled:
				return fmt.Errorf("task %s ended with status %s: %s", taskID, task.Status, task.ErrorMsg)
			}
		}
	}
}

// waitForState polls agent_runtimes until state is one of wantStates.
// If state becomes one of failStates, returns an error.
func (a *RuntimeActivator) waitForState(ctx context.Context, modelID, nodeID string, wantStates, failStates []string) error {
	ticker := time.NewTicker(a.cfg.HealthPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			var state string
			err := a.db.GetContext(ctx, &state, `
				SELECT state FROM agent_runtimes
				WHERE model_id = $1 AND node_id = $2
				ORDER BY updated_at DESC LIMIT 1`, modelID, nodeID)
			if err != nil {
				continue
			}
			for _, w := range wantStates {
				if state == w {
					return nil
				}
			}
			for _, f := range failStates {
				if state == f {
					return fmt.Errorf("runtime entered state %q", state)
				}
			}
		}
	}
}

// waitForHealthy polls until the endpoint is healthy, then reloads registry.
func (a *RuntimeActivator) waitForHealthy(ctx context.Context, cfg *ModelConfig, startTime time.Time) (*RunningEndpoint, error) {
	ticker := time.NewTicker(a.cfg.HealthPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w after %s", ErrColdStartTimeout, time.Since(startTime))
		case <-ticker.C:
			// Check agent_runtime state.
			rt, err := a.loadRuntime(ctx, cfg.ModelID, cfg.NodeID)
			if err != nil {
				continue
			}
			switch rt.State {
			case "failed":
				return nil, fmt.Errorf("runtime failed to start: %s", rt.ErrorMsg)
			case "active", "warm", "loading":
				// Container is up — try HTTP health check.
				endpointURL := fmt.Sprintf("http://%s:%d", cfg.NodeID, cfg.BindPort)
				if rt.BindHost != "" {
					endpointURL = fmt.Sprintf("http://%s:%d", rt.BindHost, rt.BindPort)
				}
				if a.httpHealthCheck(endpointURL) {
					// Mark endpoint enabled and reload registry.
					a.enableEndpoint(ctx, cfg.ModelID)
					_ = a.registry.Reload(ctx)

					ep, _, resolveErr := a.registry.ResolveWithFailover(cfg.ModelName, 3)
					if resolveErr != nil {
						// Registry may need another second — keep polling.
						continue
					}
					a.log.Info("model ready",
						zap.String("model", cfg.ModelName),
						zap.String("url", ep.URL),
						zap.Duration("warmup", time.Since(startTime)),
					)
					return &RunningEndpoint{
						EndpointID: ep.ID,
						URL:        ep.URL,
						ContainerID: rt.ContainerID,
						WarmupMs:   time.Since(startTime).Milliseconds(),
					}, nil
				}
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// RecordActivity / Status
// ─────────────────────────────────────────────────────────────────────────────

// RecordActivity updates last_used_at on the agent_runtime row.
// Called by the proxy after successfully forwarding a request.
func (a *RuntimeActivator) RecordActivity(ctx context.Context, endpointID string) {
	_, _ = a.db.ExecContext(ctx, `
		UPDATE agent_runtimes
		SET last_used_at = NOW(), updated_at = NOW()
		WHERE endpoint_id = $1
		  AND state IN ('active','warm','loading')`, endpointID)
}

// Status returns the current runtime state of a model.
func (a *RuntimeActivator) Status(ctx context.Context, modelName string) (*ModelStatus, error) {
	var row struct {
		EndpointID  string    `db:"endpoint_id"`
		ContainerID string    `db:"container_id"`
		State       string    `db:"state"`
		BindHost    string    `db:"bind_host"`
		BindPort    int       `db:"bind_port"`
		LastUsedAt  *time.Time `db:"last_used_at"`
	}
	err := a.db.GetContext(ctx, &row, `
		SELECT ar.id AS endpoint_id,
		       COALESCE(ar.container_id,'') AS container_id,
		       ar.state,
		       ar.bind_host,
		       ar.bind_port,
		       ar.last_used_at
		FROM agent_runtimes ar
		JOIN models m ON m.id = ar.model_id
		WHERE m.name = $1
		ORDER BY ar.updated_at DESC
		LIMIT 1`, modelName)
	if err != nil {
		return nil, fmt.Errorf("model %q not found: %w", modelName, err)
	}

	ms := &ModelStatus{
		ModelName:   modelName,
		EndpointID:  row.EndpointID,
		ContainerID: row.ContainerID,
		State:       State(row.State),
		URL:         fmt.Sprintf("http://%s:%d", row.BindHost, row.BindPort),
	}
	if row.LastUsedAt != nil {
		ms.LastUsedAt = *row.LastUsedAt
		ms.IdleFor = time.Since(*row.LastUsedAt)
	}
	return ms, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// DB helpers
// ─────────────────────────────────────────────────────────────────────────────

type agentRuntime struct {
	ID          string `db:"id"`
	State       string `db:"state"`
	ContainerID string `db:"container_id"`
	BindHost    string `db:"bind_host"`
	BindPort    int    `db:"bind_port"`
	ErrorMsg    string `db:"error_msg"`
}

func (a *RuntimeActivator) loadRuntime(ctx context.Context, modelID, nodeID string) (*agentRuntime, error) {
	var rt agentRuntime
	err := a.db.GetContext(ctx, &rt, `
		SELECT id, state, COALESCE(container_id,'') AS container_id,
		       COALESCE(bind_host,'') AS bind_host, bind_port,
		       COALESCE(error_msg,'') AS error_msg
		FROM agent_runtimes
		WHERE model_id = $1 AND node_id = $2
		  AND state NOT IN ('deleted')
		ORDER BY updated_at DESC LIMIT 1`, modelID, nodeID)
	if err != nil {
		return nil, err
	}
	return &rt, nil
}

func (a *RuntimeActivator) loadConfig(ctx context.Context, modelName string) (*ModelConfig, error) {
	var row struct {
		ModelID      string  `db:"model_id"`
		NodeID       string  `db:"node_id"`
		BindPort     int     `db:"bind_port"`
		GGUFPath     string  `db:"gguf_path"`
		HFRepo       string  `db:"hf_repo"`
		HFFile       string  `db:"hf_file"`
		HFToken      string  `db:"hf_token"`
		Image        string  `db:"image"`
		CtxSize      int     `db:"ctx_size"`
		NGPULayers   int     `db:"n_gpu_layers"`
		CPUThreads   string  `db:"cpu_threads"`
		MemoryLimit  string  `db:"memory_limit"`
		ModelsVolume string  `db:"models_volume"`
		GPUIDs       string  `db:"gpu_ids"`
		CPUSetCPUs   string  `db:"cpu_affinity"`
		NUMANode     int     `db:"numa_node"`
		IdleTimeout  *int    `db:"idle_timeout_secs"`
	}
	err := a.db.GetContext(ctx, &row, `
		SELECT
		    m.id                                                   AS model_id,
		    COALESCE(
		        ar.node_id::text,
		        me.node_id::text,
		        ''
		    )                                                      AS node_id,
		    COALESCE(me.port, 8080)                                AS bind_port,
		    COALESCE(mrc.gguf_path, '')                            AS gguf_path,
		    COALESCE(mrc.hf_repo, '')                              AS hf_repo,
		    COALESCE(mrc.hf_file, '')                              AS hf_file,
		    COALESCE(mrc.hf_token, '')                             AS hf_token,
		    COALESCE(me.runtime_image, '')                         AS image,
		    COALESCE(mrc.ctx_size, 4096)                           AS ctx_size,
		    COALESCE(mrc.n_gpu_layers, 0)                          AS n_gpu_layers,
		    COALESCE(mrc.cpu_threads::text, '')                    AS cpu_threads,
		    COALESCE(mrc.memory_limit, '')                         AS memory_limit,
		    COALESCE(mrc.models_volume, '')                        AS models_volume,
		    COALESCE(ar.gpu_ids::text, '[]')                       AS gpu_ids,
		    COALESCE(ar.cpu_affinity, '')                          AS cpu_affinity,
		    COALESCE(ar.numa_node, -1)                             AS numa_node,
		    mrc.idle_timeout_secs                                  AS idle_timeout_secs
		FROM models m
		LEFT JOIN model_endpoints me        ON me.model_id = m.id
		LEFT JOIN model_runtime_configs mrc ON mrc.model_id = m.id
		LEFT JOIN agent_runtimes ar         ON ar.model_id = m.id
		                                   AND ar.state NOT IN ('deleted')
		WHERE m.name = $1
		  AND m.enabled = TRUE
		ORDER BY me.priority ASC, ar.updated_at DESC NULLS LAST
		LIMIT 1`, modelName)
	if err != nil {
		return nil, err
	}

	cfg := &ModelConfig{
		ModelName:    modelName,
		ModelID:      row.ModelID,
		NodeID:       row.NodeID,
		BindPort:     row.BindPort,
		GGUFPath:     row.GGUFPath,
		HFRepo:       row.HFRepo,
		HFFile:       row.HFFile,
		HFToken:      row.HFToken,
		Image:        row.Image,
		CtxSize:      row.CtxSize,
		NGPULayers:   row.NGPULayers,
		CPUThreads:   row.CPUThreads,
		MemoryLimit:  row.MemoryLimit,
		ModelsVolume: row.ModelsVolume,
		CPUSetCPUs:   row.CPUSetCPUs,
		NUMANode:     row.NUMANode,
	}
	if row.IdleTimeout != nil {
		cfg.IdleTimeout = time.Duration(*row.IdleTimeout) * time.Second
	}
	return cfg, nil
}

func (a *RuntimeActivator) enableEndpoint(ctx context.Context, modelID string) {
	_, _ = a.db.ExecContext(ctx, `
		UPDATE model_endpoints
		SET is_enabled = TRUE, lifecycle_state = 'active',
		    health_status = 'healthy', updated_at = NOW()
		WHERE model_id = $1`, modelID)
}

func (a *RuntimeActivator) deriveState(rt *agentRuntime) State {
	if rt == nil {
		return StateNotRegistered
	}
	switch rt.State {
	case "pending", "starting":
		return StateStarting
	case "pulling", "downloading":
		return StateDownloading
	case "loading":
		return StateLoading
	case "warm", "active", "idle":
		return StateReady
	case "stopped", "unloaded":
		return StateStopped
	case "failed":
		return StateFailed
	default:
		return StateUnknown
	}
}

func (a *RuntimeActivator) modelRef(cfg *ModelConfig) string {
	if cfg.HFRepo != "" && cfg.HFFile != "" {
		return cfg.HFRepo + "/" + cfg.HFFile
	}
	if cfg.HFRepo != "" {
		return cfg.HFRepo
	}
	return cfg.GGUFPath
}

func (a *RuntimeActivator) estimateRAMMB(cfg *ModelConfig) int64 {
	// Look up required_memory_mb from runtime_requirements if set.
	// Uses a short timeout so this never blocks the hot path.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var ramMB int64
	_ = a.db.QueryRowContext(ctx, `
		SELECT COALESCE(required_memory_mb, 0)
		FROM runtime_requirements WHERE model_id = $1`, cfg.ModelID,
	).Scan(&ramMB)
	return ramMB
}

func (a *RuntimeActivator) httpHealthCheck(baseURL string) bool {
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get(baseURL + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
