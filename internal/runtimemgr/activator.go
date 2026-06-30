// Package runtimemgr — activator.go
//
// RuntimeActivator is the single entry point for ALL model startup scenarios.
// Whether triggered by a proxy cold-start, an admin deploy, a crash recovery,
// an idle-restore, or a re-deploy, every path calls:
//
//	activator.StartModel(ctx, modelName)   — control-plane entry point
//
// which enqueues a START_MODEL task.  The node agent executes the full unified
// pipeline:
//
//	CREATED → VALIDATING → DOWNLOADING → STARTING →
//	LOADING_MODEL → WAITING_READY → READY
//
// A model is READY only when all four conditions hold:
//  1. Container is running.
//  2. Health endpoint responds HTTP 200.
//  3. Runtime reports model loaded.
//  4. Endpoint is routable in the registry.
//
// Connection refused during startup is NOT treated as failure.
//
// Failure triggers:
//   - Container exits unexpectedly.
//   - Startup timeout exceeded (ColdStartTimeout).
//   - Fatal runtime error in container logs.
//   - Validation fails (missing required fields).
package runtimemgr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/runtime"
	"github.com/nexusllm/nexusllm/internal/taskmanager"
	"go.uber.org/zap"
)

// RuntimeActivator implements the Activator interface.
// Safe for concurrent use — multiple goroutines calling EnsureRunning for the
// same model are deduplicated via an in-memory singleflight map.
type RuntimeActivator struct {
	db       *sqlx.DB
	taskMgr  *taskmanager.Manager
	registry *runtime.Registry
	guard    *ResourceGuard
	cfg      Config
	log      *zap.Logger

	// inflight deduplicates concurrent EnsureRunning calls for the same model.
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
// EnsureRunning — proxy cold-start entry point
// ─────────────────────────────────────────────────────────────────────────────

// EnsureRunning guarantees the named model is healthy before returning.
// It deduplicates concurrent callers via singleflight: if ten requests arrive
// simultaneously for a cold model, only one start sequence runs.
//
// The startup sequence runs on a background context so it survives client
// disconnects — the caller's ctx is only used for the wait portion.
func (a *RuntimeActivator) EnsureRunning(ctx context.Context, modelName string) (*RunningEndpoint, error) {
	start := time.Now()

	ch, owner := a.inflight.getOrCreate(modelName)
	if !owner {
		select {
		case <-ch:
			if ep, _, err := a.registry.ResolveWithFailover(modelName, 3); err == nil {
				return &RunningEndpoint{
					EndpointID: ep.ID,
					URL:        ep.URL,
					WarmupMs:   time.Since(start).Milliseconds(),
				}, nil
			}
			// Owner may have failed — fall through and try ourselves.
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	defer a.inflight.release(modelName, ch)

	bgCtx, bgCancel := context.WithTimeout(context.Background(), a.cfg.ColdStartTimeout)
	defer bgCancel()

	type result struct {
		ep  *RunningEndpoint
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		ep, err := a.doStartModel(bgCtx, modelName, start)
		resultCh <- result{ep, err}
	}()

	select {
	case r := <-resultCh:
		return r.ep, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// doStartModel — unified startup logic (all triggers converge here)
// ─────────────────────────────────────────────────────────────────────────────

// doStartModel is the single unified startup implementation for:
//   - Proxy cold start (EnsureRunning)
//   - Initial deployment (admin API → StartModel)
//   - Re-deploy (admin API → StartModel)
//   - Crash recovery (idle manager, node health monitor → StartModel)
//   - Lazy load (proxy on every uncached request)
//
// It ALWAYS re-validates config, then either waits for an in-flight start
// or kicks off a fresh START_MODEL task.
func (a *RuntimeActivator) doStartModel(ctx context.Context, modelName string, startTime time.Time) (*RunningEndpoint, error) {
	// Step 1: Load and validate model config.
	cfg, err := a.loadConfig(ctx, modelName)
	if err != nil {
		// sql.ErrNoRows means the model doesn't exist in the catalog at all.
		return nil, fmt.Errorf("%w: %s", ErrModelNotFound, err.Error())
	}
	if cfg.NodeID == "" {
		// The model exists in the catalog but has no node assignment.
		// This is a configuration issue, not a missing model.
		// Return a distinct, actionable error so operators know what to fix.
		return nil, fmt.Errorf(
			"model %q exists in catalog but has no node assigned — "+
				"set node_id on the model_endpoint or redeploy with node_id in the request body",
			modelName,
		)
	}

	// Step 2: Resource pre-flight (RAM / VRAM).
	// Only check the configured node when it is actually online. If the original
	// endpoint node is offline, skip the guard — a new HA replica may already be
	// starting on a different node.
	configuredNodeOnline := a.isNodeOnline(ctx, cfg.NodeID)
	if cfg.NodeID != "" && configuredNodeOnline {
		ramMB := a.estimateRAMMB(cfg)
		if guardErr := a.guard.CanStart(ctx, cfg.NodeID, ResourceRequest{
			ModelName:   modelName,
			RAMMBNeeded: ramMB,
			GPUDevices:  cfg.GPUDevices,
		}); guardErr != nil {
			return nil, guardErr
		}
	}

	// Step 3: Inspect existing runtime row.
	// Pass "" as nodeID so loadRuntime searches ALL online nodes — this finds
	// HA-reconciler-created replicas that may be on a different node than the
	// one recorded in model_endpoints.
	rt, _ := a.loadRuntime(ctx, cfg.ModelID, "")
	state := a.deriveState(rt)

	a.log.Info("StartModel",
		zap.String("model", modelName),
		zap.String("current_state", string(state)),
	)

	switch state {
	case StateReady, StateIdle:
		// Already healthy — fast path.
		_ = a.registry.Reload(ctx)
		if ep, _, err := a.registry.ResolveWithFailover(modelName, 3); err == nil {
			return &RunningEndpoint{EndpointID: ep.ID, URL: ep.URL, WarmupMs: 0}, nil
		}
		// Registry stale — fall through to wait.

	case StateCreated, StateLoadingModel, StateStarting, StateValidating, StateDownloading, StateWaitingReady:
		// A START_MODEL task is already in flight.
		// Stale detection: check how long ago the runtime entered its CURRENT state.
		// We query state_updated_at (or fall back to updated_at) to avoid being fooled
		// by heartbeat-driven updated_at refreshes that don't change state.
		var stateEnteredAt time.Time
		_ = a.db.QueryRowContext(ctx, `
			SELECT COALESCE(started_at, updated_at, created_at)
			FROM agent_runtimes WHERE id = $1`, rt.ID,
		).Scan(&stateEnteredAt)
		if stateEnteredAt.IsZero() {
			stateEnteredAt = time.Now()
		}

		staleThreshold := a.cfg.ColdStartTimeout
		// For loading_model specifically, use a tighter threshold — if it's been
		// loading for longer than ColdStartTimeout/2, it's stuck.
		if state == StateLoadingModel || state == StateWaitingReady {
			staleThreshold = a.cfg.ColdStartTimeout / 2
		}
		isStale := time.Since(stateEnteredAt) > staleThreshold

		// If the runtime is on a different (online) node than cfg, the HA reconciler
		// started a replacement replica — never reset or re-enqueue, just wait.
		onDifferentOnlineNode := rt.NodeID != "" && rt.NodeID != cfg.NodeID && a.isNodeOnline(ctx, rt.NodeID)

		if isStale && !onDifferentOnlineNode {
			a.log.Warn("startup stalled — resetting and re-enqueueing START_MODEL",
				zap.String("model", modelName),
				zap.String("state", string(state)),
				zap.Duration("stale_for", time.Since(stateEnteredAt)),
			)
			_, _ = a.db.ExecContext(ctx,
				`UPDATE agent_runtimes SET state='failed', error_msg='startup stalled — gateway reset', container_id='', updated_at=NOW() WHERE id=$1`, rt.ID)
			if err := a.enqueueStartModel(ctx, cfg); err != nil {
				return nil, err
			}
		} else {
			a.log.Info("START_MODEL already in flight — waiting",
				zap.String("model", modelName),
				zap.String("state", string(state)),
				zap.String("runtime_node", rt.NodeID),
				zap.Bool("ha_replica_on_different_node", onDifferentOnlineNode),
				zap.Duration("in_state_for", time.Since(stateEnteredAt)),
				zap.Duration("stale_threshold", staleThreshold),
			)
			// Do NOT enqueue a new task — just wait for the existing one.
		}

	default:
		// StateUnknown, StateNotRegistered, StateStopped, StateFailed, StateLost —
		// all trigger a fresh START_MODEL.  The node agent handles downloading
		// if needed and always recreates the container from scratch.
		if err := a.enqueueStartModel(ctx, cfg); err != nil {
			return nil, err
		}
	}

	// Step 4: Poll until READY (all four conditions satisfied).
	ep, waitErr := a.waitForReady(ctx, cfg, startTime)
	if waitErr != nil {
		// If the runtime transitioned to failed, retry once — but only if we can
		// find a node to start it on.
		if rt2, loadErr := a.loadRuntime(ctx, cfg.ModelID, ""); loadErr == nil && rt2.State == "failed" {
			a.log.Warn("startup failed — retrying START_MODEL once",
				zap.String("model", modelName),
				zap.Error(waitErr),
			)
			if startErr := a.enqueueStartModel(ctx, cfg); startErr != nil {
				return nil, startErr
			}
			return a.waitForReady(ctx, cfg, startTime)
		}
		return nil, waitErr
	}
	return ep, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// StartModel — public API used by admin handler, idle manager, recovery watchdog
// ─────────────────────────────────────────────────────────────────────────────

// StartModel is the public control-plane entry point for ALL model startup
// scenarios.  Callers:
//   - Admin handler (POST /admin/v1/models/deploy and /redeploy)
//   - IdleManager.restoreAlwaysRunning()
//   - Node health monitor (crash / lost-node recovery)
//   - Any future trigger that needs to start a model
//
// It does NOT block for readiness — it enqueues the START_MODEL task and
// returns.  Use EnsureRunning if you need to block until the model is READY.
func (a *RuntimeActivator) StartModel(ctx context.Context, modelName string) error {
	cfg, err := a.loadConfig(ctx, modelName)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrModelNotFound, err.Error())
	}
	if cfg.NodeID == "" {
		return fmt.Errorf(
			"model %q exists in catalog but has no node assigned — "+
				"set node_id on the model_endpoint or redeploy with node_id in the request body",
			modelName,
		)
	}
	return a.enqueueStartModel(ctx, cfg)
}

// ─────────────────────────────────────────────────────────────────────────────
// enqueueStartModel — inserts DB row + dispatches START_MODEL task
// ─────────────────────────────────────────────────────────────────────────────

// enqueueStartModel is called for EVERY startup trigger.  It:
//  1. Marks any non-terminal existing runtime rows as stopped.
//  2. Inserts a fresh runtime row in state="created".
//  3. Enqueues a START_MODEL task with the complete model config.
//
// The node agent is responsible for the full pipeline:
//
//	CREATED → VALIDATING → DOWNLOADING → STARTING →
//	LOADING_MODEL → WAITING_READY → READY
func (a *RuntimeActivator) enqueueStartModel(ctx context.Context, cfg *ModelConfig) error {
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
	// Derive backend from config; default to llamacpp if unset.
	backend := cfg.Backend
	if backend == "" {
		backend = "llamacpp"
	}

	// ── Resolve execution mode ─────────────────────────────────────────────
	// "auto" is resolved here against the node's actual GPU capability so that
	// the payload sent to the agent carries a concrete "cpu" or "gpu" value.
	requestedMode := cfg.ExecutionMode
	if requestedMode == "" {
		requestedMode = "auto"
	}
	effectiveMode := resolveExecutionMode(ctx, a.db, cfg.NodeID, backend, requestedMode, cfg.GPUDevices, cfg.NGPULayers)

	// For CPU mode: clear GPU devices and force n_gpu_layers=0 so the agent
	// never requests --gpus from Docker regardless of what the config says.
	gpuDevices := cfg.GPUDevices
	nGPULayers := cfg.NGPULayers
	if effectiveMode == "cpu" {
		gpuDevices = nil
		nGPULayers = 0
	}

	// Park any in-progress rows so the UI always shows a single current row.
	// IMPORTANT: do NOT park 'loading_model' or 'starting' rows — those indicate
	// a live container start in progress.  Parking them would cause the next
	// proxy request to create a new runtime row while the container is already
	// starting, leading to a perpetual pending/park loop.
	_, _ = a.db.ExecContext(ctx, `
		UPDATE agent_runtimes
		SET state = 'stopped', updated_at = NOW()
		WHERE model_id = $1 AND node_id = $2
		  AND state NOT IN (
		      'deleted','stopping','stopped','failed',
		      'loading_model','waiting_ready','starting','validating','downloading'
		  )`,
		cfg.ModelID, cfg.NodeID)

	// Insert new runtime row starting at state="pending".
	// We do NOT use ON CONFLICT DO NOTHING here — if the insert produces zero
	// rows (e.g. no matching model_endpoints row) we must detect that and fail
	// with a clear error rather than silently orphaning runtimeID (which would
	// cause an FK violation when the task references it).
	workloadPolicy := cfg.WorkloadPolicy
	if workloadPolicy == "" {
		workloadPolicy = "lazy_load" // default for LLMs
	}
	res, err := a.db.ExecContext(ctx, `
		INSERT INTO agent_runtimes
		  (id, node_id, endpoint_id, model_id, runtime_name, backend,
		   state, gpu_ids, bind_host, bind_port, cpu_affinity, numa_node,
		   requested_mode, effective_mode, workload_policy)
		SELECT $1, $2, me.id, $3, $4, $8, 'pending',
		       $7::jsonb, me.host, me.port, $5, $6,
		       $9, $10, $11
		FROM model_endpoints me
		WHERE me.model_id = $3
		  AND me.lifecycle_state NOT IN ('deleted')
		ORDER BY me.priority ASC
		LIMIT 1`,
		runtimeID, cfg.NodeID, cfg.ModelID, containerName,
		cfg.CPUSetCPUs, cfg.NUMANode,
		func() string {
			b, _ := json.Marshal(gpuDevices)
			return string(b)
		}(),
		backend,
		requestedMode,
		effectiveMode,
		workloadPolicy,
	)
	if err != nil {
		return fmt.Errorf("insert agent_runtime: %w", err)
	}

	// If the subquery matched no endpoint, rows affected == 0 and runtimeID
	// was never inserted.  Enqueueing a task with that runtimeID would violate
	// the agent_tasks_runtime_id_fkey FK.  Fail here with a clear message.
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf(
			"insert agent_runtime: no active model_endpoint found for model %q (model_id=%s) — "+
				"ensure the endpoint exists and is not in 'deleted' state before starting",
			cfg.ModelName, cfg.ModelID,
		)
	}

	payload := taskmanager.StartModelPayload{
		RuntimeID:      runtimeID,
		ModelID:        cfg.ModelID,
		RuntimeName:    containerName,
		Backend:        backend,
		Image:          image,
		ModelName:      cfg.ModelName,
		ServedAs:       cfg.ModelName,
		BindHost:       cfg.BindHost,
		BindPort:       cfg.BindPort,
		GPUDevices:     gpuDevices, // nil for cpu mode
		CPUSetCPUs:     cfg.CPUSetCPUs,
		NUMANode:       cfg.NUMANode,
		MemoryLimit:    cfg.MemoryLimit,
		CPULimit:       cfg.CPUThreads,
		GGUFPath:       cfg.GGUFPath,
		HFRepo:         cfg.HFRepo,
		HFFile:         cfg.HFFile,
		HFToken:        cfg.HFToken,
		ModelsVolume:   vol,
		CtxSize:        ctxSize,
		NGPULayers:     nGPULayers, // 0 for cpu mode
		TensorParallel: cfg.TensorParallel,
		GPUMemoryUtil:  cfg.GPUMemoryUtil,
		MaxModelLen:    cfg.MaxModelLen,
		Dtype:          cfg.Dtype,
		Quantization:   cfg.Quantization,
		ExtraArgs:      injectReasoningFlag(ctx, a.db, cfg.ModelID, backend, cfg.ExtraArgs),
		ExecutionMode:  effectiveMode,
		WorkloadPolicy: workloadPolicy,
		Env:            map[string]string{},
	}

	taskID, err := a.taskMgr.Enqueue(ctx, cfg.NodeID,
		taskmanager.TaskStartModel, payload,
		taskmanager.WithPriority(85),
		taskmanager.WithActor("runtimemgr"),
		taskmanager.WithRuntimeID(runtimeID),
		taskmanager.WithTimeout(a.cfg.ColdStartTimeout+5*time.Minute),
		// Per-runtimeID key: each start attempt gets its own task.
		taskmanager.WithIdempotencyKey("start:"+cfg.NodeID+":"+runtimeID),
	)
	if err != nil {
		return fmt.Errorf("enqueue START_MODEL: %w", err)
	}

	a.log.Info("START_MODEL enqueued",
		zap.String("model", cfg.ModelName),
		zap.String("task_id", taskID),
		zap.String("runtime_id", runtimeID),
		zap.String("backend", backend),
		zap.String("gguf_path", cfg.GGUFPath),
		zap.String("hf_repo", cfg.HFRepo),
		zap.Ints("gpu_devices", cfg.GPUDevices),
	)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// waitForReady — polls until all four readiness conditions are satisfied
// ─────────────────────────────────────────────────────────────────────────────

// waitForReady polls the agent_runtimes row and the HTTP health endpoint until
// the model is fully READY.  It enforces the four readiness conditions:
//  1. Container is running.
//  2. Health endpoint responds HTTP 200.
//  3. Runtime state is "ready" or "active" or "warm".
//  4. Registry can resolve the endpoint.
//
// "Connection refused" is explicitly NOT treated as failure — it is the
// expected state while the model is in LOADING_MODEL or WAITING_READY.
//
// Genuine failures:
//   - Runtime state transitions to "failed".
//   - Container is externally removed while we wait (state→stopped, no container_id).
//   - ColdStartTimeout exceeded while in LOADING_MODEL state.
func (a *RuntimeActivator) waitForReady(ctx context.Context, cfg *ModelConfig, startTime time.Time) (*RunningEndpoint, error) {
	ticker := time.NewTicker(a.cfg.HealthPollInterval)
	defer ticker.Stop()

	// loadingModelDeadline is set once the first time we see a loading_model state.
	// It is NOT reset on subsequent ticks — we want a hard wall-clock timeout
	// that covers the entire loading period across multiple waitForReady calls.
	// We use ColdStartTimeout / 2 as the loading-specific budget.
	staleLoadingThreshold := a.cfg.ColdStartTimeout / 2
	loadingModelEnteredAt := time.Time{} // zero = not yet in loading_model

	consecutiveHealthFails := 0

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w after %s", ErrColdStartTimeout, time.Since(startTime))
		case <-ticker.C:
			// Search ALL online nodes for this model's runtime — the HA reconciler
			// may have started a replacement on a different node than cfg.NodeID.
			rt, err := a.loadRuntime(ctx, cfg.ModelID, "")
			if err != nil {
				continue
			}

			switch rt.State {
			case "failed":
				return nil, fmt.Errorf("model startup failed: %s", rt.ErrorMsg)

			case "stopped", "unloaded":
				// Container removed while we were waiting.
				_, _ = a.db.ExecContext(ctx,
					`UPDATE agent_runtimes SET state='failed', error_msg='container removed during startup', updated_at=NOW() WHERE id=$1`, rt.ID)
				return nil, fmt.Errorf("container was removed during startup — will retry on next request")

			case "created", "pending", "validating", "downloading", "starting":
				// Early pipeline stages — the container hasn't started yet.
				// Reset the loading clock; stale detection only starts from loading_model.
				loadingModelEnteredAt = time.Time{}
				consecutiveHealthFails = 0
				continue

			case "loading_model", "waiting_ready", "active", "warm", "loading":
				// Record when we first entered the loading stage.
				if loadingModelEnteredAt.IsZero() {
					var stateUpdatedAt time.Time
					_ = a.db.QueryRowContext(ctx,
						`SELECT updated_at FROM agent_runtimes WHERE id = $1`, rt.ID,
					).Scan(&stateUpdatedAt)
					if !stateUpdatedAt.IsZero() {
						loadingModelEnteredAt = stateUpdatedAt
					} else {
						loadingModelEnteredAt = time.Now()
					}
				}

				// Container is running; check the health endpoint.
				host := rt.BindHost
				if host == "" {
					host = cfg.BindHost
				}
				if host == "" {
					var nodeIP string
					_ = a.db.QueryRowContext(ctx,
						`SELECT COALESCE(ip_address, hostname, '') FROM nodes WHERE id = $1`,
						rt.NodeID).Scan(&nodeIP)
					if nodeIP == "" {
						_ = a.db.QueryRowContext(ctx,
							`SELECT COALESCE(ip_address, hostname, '') FROM nodes WHERE id = $1`,
							cfg.NodeID).Scan(&nodeIP)
					}
					host = nodeIP
				}
				port := rt.BindPort
				if port == 0 {
					port = cfg.BindPort
				}

				// Port is still 0 — agent hasn't reported the allocated port yet.
				// This is normal for the first few ticks after bind_port=0 was sent.
				// Skip the health check and wait for the next poll to pick up the
				// agent's bind_port update.
				if port == 0 {
					a.log.Debug("waitForReady: port not yet reported by agent — waiting",
						zap.String("model", cfg.ModelName),
						zap.String("runtime_id", rt.ID),
					)
					continue
				}

				endpointURL := fmt.Sprintf("http://%s:%d", host, port)

				healthy := a.httpHealthCheck(endpointURL)
				if healthy {
					consecutiveHealthFails = 0

					_, _ = a.db.ExecContext(ctx, `
						UPDATE agent_runtimes
						SET state = 'ready', last_used_at = COALESCE(last_used_at, NOW()), updated_at = NOW()
						WHERE id = $1 AND state NOT IN ('idle','stopping','stopped','failed','deleted')`, rt.ID)

					a.enableEndpoint(ctx, cfg.ModelID)
					_ = a.registry.Reload(ctx)

					ep, _, resolveErr := a.registry.ResolveWithFailover(cfg.ModelName, 3)
					if resolveErr != nil {
						// Registry needs another tick — keep polling.
						continue
					}

					a.log.Info("model READY",
						zap.String("model", cfg.ModelName),
						zap.String("url", ep.URL),
						zap.String("runtime_id", rt.ID),
						zap.Duration("startup_time", time.Since(startTime)),
					)
					return &RunningEndpoint{
						EndpointID:  ep.ID,
						URL:         ep.URL,
						ContainerID: rt.ContainerID,
						WarmupMs:    time.Since(startTime).Milliseconds(),
					}, nil
				}

				// Health check failed (connection refused or non-200).
				consecutiveHealthFails++

				// Hard timeout: if we've been stuck in loading_model longer than the
				// stale threshold, mark failed and let the caller retry.
				loadingDuration := time.Since(loadingModelEnteredAt)
				if loadingDuration > staleLoadingThreshold {
					a.log.Warn("model stuck in loading — marking failed and clearing lock",
						zap.String("model", cfg.ModelName),
						zap.String("runtime_id", rt.ID),
						zap.Duration("loading_for", loadingDuration),
						zap.Int("health_fails", consecutiveHealthFails),
					)
					_, _ = a.db.ExecContext(ctx,
						`UPDATE agent_runtimes
						 SET state     = 'failed',
						     error_msg = $2,
						     updated_at = NOW()
						 WHERE id = $1`,
						rt.ID,
						fmt.Sprintf("startup timeout: health check did not pass after %s", loadingDuration.Round(time.Second)))
					return nil, fmt.Errorf("startup timeout: health check did not pass after %s", loadingDuration)
				}
				// Not timed out yet — connection refused is expected while model loads.
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
		SET last_used_at = NOW(),
		    updated_at   = NOW(),
		    state = CASE
		      WHEN state IN ('loading_model','waiting_ready','loading') THEN 'ready'
		      ELSE state
		    END
		WHERE endpoint_id = $1
		  AND state NOT IN ('stopping','stopped','failed','deleted')`, endpointID)
}

// Status returns the current runtime state of a model.
func (a *RuntimeActivator) Status(ctx context.Context, modelName string) (*ModelStatus, error) {
	var row struct {
		EndpointID  string     `db:"endpoint_id"`
		ContainerID string     `db:"container_id"`
		State       string     `db:"state"`
		BindHost    string     `db:"bind_host"`
		BindPort    int        `db:"bind_port"`
		LastUsedAt  *time.Time `db:"last_used_at"`
	}
	err := a.db.GetContext(ctx, &row, `
		SELECT ar.id AS endpoint_id,
		       COALESCE(ar.container_id,'') AS container_id,
		       ar.state,
		       COALESCE(ar.bind_host,'')    AS bind_host,
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
	NodeID      string `db:"node_id"`
	State       string `db:"state"`
	ContainerID string `db:"container_id"`
	BindHost    string `db:"bind_host"`
	BindPort    int    `db:"bind_port"`
	ErrorMsg    string `db:"error_msg"`
}

func (a *RuntimeActivator) loadRuntime(ctx context.Context, modelID, nodeID string) (*agentRuntime, error) {
	var rt agentRuntime
	// First try the specific node if provided
	if nodeID != "" {
		err := a.db.GetContext(ctx, &rt, `
			SELECT id, node_id::text AS node_id, state,
			       COALESCE(container_id,'') AS container_id,
			       COALESCE(bind_host,'') AS bind_host, bind_port,
			       COALESCE(error_msg,'') AS error_msg
			FROM agent_runtimes
			WHERE model_id = $1 AND node_id = $2
			  AND state NOT IN ('deleted')
			ORDER BY updated_at DESC LIMIT 1`, modelID, nodeID)
		if err == nil {
			return &rt, nil
		}
	}
	// Fallback: find the most recent healthy/active runtime for this model on ANY online node.
	// This handles HA recovery — the reconciler may have started a replacement on a new node.
	err := a.db.GetContext(ctx, &rt, `
		SELECT ar.id, ar.node_id::text AS node_id, ar.state,
		       COALESCE(ar.container_id,'') AS container_id,
		       COALESCE(ar.bind_host,'') AS bind_host, ar.bind_port,
		       COALESCE(ar.error_msg,'') AS error_msg
		FROM agent_runtimes ar
		JOIN nodes n ON n.id = ar.node_id
		WHERE ar.model_id = $1
		  AND ar.state NOT IN ('deleted')
		  AND n.status IN ('online','degraded')
		ORDER BY
		  CASE ar.state
		    WHEN 'ready'         THEN 1
		    WHEN 'active'        THEN 1
		    WHEN 'warm'          THEN 1
		    WHEN 'idle'          THEN 2
		    WHEN 'loading_model' THEN 3
		    WHEN 'waiting_ready' THEN 3
		    WHEN 'starting'      THEN 4
		    WHEN 'pending'       THEN 5
		    ELSE 9
		  END ASC,
		  ar.updated_at DESC
		LIMIT 1`, modelID)
	return &rt, err
}

// isNodeOnline returns true when the node with the given ID has status
// 'online' or 'degraded'. Returns false for empty ID, offline nodes, or
// nodes not found in the DB.
func (a *RuntimeActivator) isNodeOnline(ctx context.Context, nodeID string) bool {
	if nodeID == "" {
		return false
	}
	var status string
	err := a.db.QueryRowContext(ctx,
		`SELECT status FROM nodes WHERE id = $1`, nodeID,
	).Scan(&status)
	if err != nil {
		return false
	}
	return status == "online" || status == "degraded"
}

func (a *RuntimeActivator) loadConfig(ctx context.Context, modelName string) (*ModelConfig, error) {
	cfg, err := a.loadConfigQuery(ctx, modelName, true)
	if err != nil && isUndefinedColumnError(err) {
		// Migration 014 (execution_mode column) not yet applied.
		// Retry without that column and default to "auto".
		a.log.Warn("execution_mode column missing — migration 014 not applied; defaulting to 'auto'",
			zap.String("model", modelName))
		cfg, err = a.loadConfigQuery(ctx, modelName, false)
		if cfg != nil {
			cfg.ExecutionMode = "auto"
		}
	}
	return cfg, err
}

// isUndefinedColumnError returns true for Postgres error code 42703
// (undefined_column), which occurs when a column referenced in a query
// does not exist in the actual schema.
func isUndefinedColumnError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "42703") ||
		strings.Contains(err.Error(), "does not exist") &&
			strings.Contains(err.Error(), "column")
}

func (a *RuntimeActivator) loadConfigQuery(ctx context.Context, modelName string, withExecutionMode bool) (*ModelConfig, error) {
	var row struct {
		ModelID        string  `db:"model_id"`
		NodeID         string  `db:"node_id"`
		BindHost       string  `db:"bind_host"`
		BindPort       int     `db:"bind_port"`
		Backend        string  `db:"backend"`
		GGUFPath       string  `db:"gguf_path"`
		HFRepo         string  `db:"hf_repo"`
		HFFile         string  `db:"hf_file"`
		HFToken        string  `db:"hf_token"`
		Image          string  `db:"image"`
		CtxSize        int     `db:"ctx_size"`
		NGPULayers     int     `db:"n_gpu_layers"`
		CPUThreads     string  `db:"cpu_threads"`
		MemoryLimit    string  `db:"memory_limit"`
		ModelsVolume   string  `db:"models_volume"`
		TensorParallel int     `db:"tensor_parallel"`
		GPUMemoryUtil  float64 `db:"gpu_memory_util"`
		Dtype          string  `db:"dtype"`
		Quantization   string  `db:"quantization"`
		GPUDevicesJSON string  `db:"gpu_devices_json"`
		CPUSetCPUs     string  `db:"cpu_affinity"`
		NUMANode       int     `db:"numa_node"`
		IdleTimeout    *int    `db:"idle_timeout_secs"`
		ExecutionMode  string  `db:"execution_mode"`
		WorkloadPolicy string  `db:"workload_policy"`
		ExtraArgsJSON  string  `db:"extra_args_json"`
	}
	// Node assignment priority:
	//   1. model_endpoints.node_id  — set by admin deploy or placement engine
	//   2. agent_runtimes.node_id   — set by a previous runtime row (recovery)
	const baseQuery = `
		SELECT
		    m.id                                          AS model_id,
		    COALESCE(me.node_id::text, ar.node_id::text,
		             '')                                  AS node_id,
		    COALESCE(me.host,  '')                        AS bind_host,
		    COALESCE(me.port,  8080)                      AS bind_port,
		    COALESCE(m.backend_type, 'llamacpp')          AS backend,
		    COALESCE(mrc.gguf_path,  '')                  AS gguf_path,
		    COALESCE(mrc.hf_repo,    '')                  AS hf_repo,
		    COALESCE(mrc.hf_file,    '')                  AS hf_file,
		    COALESCE(mrc.hf_token,   '')                  AS hf_token,
		    COALESCE(me.runtime_image, '')                AS image,
		    COALESCE(mrc.ctx_size,   4096)                AS ctx_size,
		    COALESCE(mrc.n_gpu_layers, 0)                 AS n_gpu_layers,
		    COALESCE(mrc.cpu_threads::text, '')           AS cpu_threads,
		    COALESCE(mrc.memory_limit, '')                AS memory_limit,
		    COALESCE(mrc.models_volume, '')               AS models_volume,
		    COALESCE(mrc.tensor_parallel, 1)              AS tensor_parallel,
		    COALESCE(mrc.gpu_memory_util, 0.90)           AS gpu_memory_util,
		    COALESCE(mrc.dtype, 'auto')                   AS dtype,
		    COALESCE(mrc.quantization, '')                AS quantization,
		    COALESCE(ar.gpu_ids::text, '[]')              AS gpu_devices_json,
		    COALESCE(ar.cpu_affinity, '')                 AS cpu_affinity,
		    COALESCE(ar.numa_node, -1)                    AS numa_node,
		    mrc.idle_timeout_secs,
		    %s                                             AS execution_mode,
		    %s                                             AS workload_policy,
		    %s                                             AS extra_args_json
		FROM models m
		LEFT JOIN model_endpoints me
		       ON me.model_id = m.id
		LEFT JOIN model_runtime_configs mrc
		       ON mrc.model_id = m.id
		LEFT JOIN agent_runtimes ar
		       ON ar.model_id = m.id
		      AND ar.state NOT IN ('deleted')
		      -- Only consider runtimes on online nodes for node_id fallback
		      AND ar.node_id IN (SELECT id FROM nodes WHERE status IN ('online','degraded'))
		WHERE m.name    = $1
		  AND m.enabled = TRUE
		ORDER BY
		    (me.node_id IS NOT NULL) DESC,
		    me.priority ASC,
		    ar.updated_at DESC NULLS LAST
		LIMIT 1`

	execModeExpr := `COALESCE(mrc.execution_mode, 'auto')`
	policyExpr := `COALESCE(mrc.workload_policy, 'lazy_load')`
	extraArgsExpr := `COALESCE(mrc.extra_args::text, '[]')`
	if !withExecutionMode {
		execModeExpr = `'auto'`
		policyExpr = `'lazy_load'`
		extraArgsExpr = `'[]'`
	}
	q := fmt.Sprintf(baseQuery, execModeExpr, policyExpr, extraArgsExpr)

	err := a.db.GetContext(ctx, &row, q, modelName)
	if err != nil {
		return nil, err
	}

	var gpuDevices []int
	if row.GPUDevicesJSON != "" && row.GPUDevicesJSON != "[]" {
		_ = json.Unmarshal([]byte(row.GPUDevicesJSON), &gpuDevices)
	}
	var extraArgs []string
	if row.ExtraArgsJSON != "" && row.ExtraArgsJSON != "[]" {
		_ = json.Unmarshal([]byte(row.ExtraArgsJSON), &extraArgs)
	}
	cpuThreads := row.CPUThreads
	if cpuThreads == "0" {
		cpuThreads = ""
	}

	cfg := &ModelConfig{
		ModelName:      modelName,
		ModelID:        row.ModelID,
		NodeID:         row.NodeID,
		BindHost:       row.BindHost,
		BindPort:       row.BindPort,
		Backend:        row.Backend,
		GGUFPath:       row.GGUFPath,
		HFRepo:         row.HFRepo,
		HFFile:         row.HFFile,
		HFToken:        row.HFToken,
		Image:          row.Image,
		CtxSize:        row.CtxSize,
		NGPULayers:     row.NGPULayers,
		CPUThreads:     cpuThreads,
		MemoryLimit:    row.MemoryLimit,
		ModelsVolume:   row.ModelsVolume,
		TensorParallel: row.TensorParallel,
		GPUMemoryUtil:  row.GPUMemoryUtil,
		Dtype:          row.Dtype,
		Quantization:   row.Quantization,
		GPUDevices:     gpuDevices,
		CPUSetCPUs:     row.CPUSetCPUs,
		NUMANode:       row.NUMANode,
		ExecutionMode:  row.ExecutionMode,
		WorkloadPolicy: row.WorkloadPolicy,
		ExtraArgs:      extraArgs,
	}
	if row.IdleTimeout != nil {
		cfg.IdleTimeout = time.Duration(*row.IdleTimeout) * time.Second
	}
	return cfg, nil
}

func (a *RuntimeActivator) enableEndpoint(ctx context.Context, modelID string) {
	// Only enable endpoints whose node is currently online — never re-enable
	// endpoints that belong to offline/dead nodes.
	_, _ = a.db.ExecContext(ctx, `
		UPDATE model_endpoints
		SET is_enabled = TRUE, lifecycle_state = 'active',
		    health_status = 'healthy', updated_at = NOW()
		WHERE model_id = $1
		  AND (
		      node_id IS NULL
		      OR node_id IN (SELECT id FROM nodes WHERE status IN ('online','degraded'))
		  )`, modelID)
}

// deriveState maps the DB agent_runtimes.state string to the internal State type.
// Handles both new pipeline states and legacy state names for backward compat.
func (a *RuntimeActivator) deriveState(rt *agentRuntime) State {
	if rt == nil {
		return StateUnknown
	}
	switch rt.State {
	case "created", "pending":
		return StateCreated
	case "validating":
		return StateValidating
	case "downloading", "pulling":
		return StateDownloading
	case "starting":
		return StateStarting
	case "loading_model", "loading":
		return StateLoadingModel
	case "waiting_ready":
		return StateWaitingReady
	case "ready", "active", "warm":
		return StateReady
	case "idle":
		return StateIdle
	case "stopping":
		return StateStopping
	case "stopped", "unloaded":
		// Container was stopped cleanly.  If container_id is empty the container
		// was removed — treat as unknown so a fresh start is triggered.
		if rt.ContainerID == "" {
			return StateUnknown
		}
		return StateStopped
	case "failed":
		return StateFailed
	case "lost":
		// Node went offline.  Treat as unknown — a fresh START_MODEL will be
		// dispatched, and the node agent will handle it when the node recovers.
		return StateLost
	default:
		return StateUnknown
	}
}

// resolveExecutionMode converts an execution_mode of "auto" into a concrete
// "cpu" or "gpu" value by querying the node's GPU capability from the DB.
//
// Rules:
//   - "cpu"  → always returns "cpu" (GPUDevices/NGPULayers will be zeroed by caller)
//   - "gpu"  → always returns "gpu" (caller should have already validated GPU availability)
//   - "auto" → returns "gpu" if the node has GPUs AND the backend supports GPU,
//     otherwise returns "cpu"
//   - ""     → treated as "auto"
//
// Backends that never support GPU (cpu_native): always return "cpu" regardless.
// Backends that always require GPU (vllm, tgi): return "gpu" (node validation
// is the operator's responsibility; we don't block here).
func resolveExecutionMode(
	ctx context.Context,
	db *sqlx.DB,
	nodeID string,
	backend string,
	requestedMode string,
	gpuDevices []int,
	nGPULayers int,
) string {
	// Backends that have no GPU path at all.
	cpuOnlyBackends := map[string]bool{"cpu_native": true}
	if cpuOnlyBackends[backend] {
		return "cpu"
	}

	switch requestedMode {
	case "cpu":
		return "cpu"
	case "gpu":
		return "gpu"
	}

	// "auto" or "" — resolve from node capability and payload hints.

	// If GPUDevices is explicitly set, the operator already decided GPU.
	if len(gpuDevices) > 0 {
		return "gpu"
	}
	// If NGPULayers is non-zero for llamacpp, that is a GPU request.
	if backend == "llamacpp" && nGPULayers != 0 {
		// Check the node actually has a GPU before committing to gpu mode.
		var gpuAvailable bool
		_ = db.QueryRowContext(ctx, `
			SELECT COALESCE(gpu_available, gpu_count > 0, FALSE)
			FROM node_capabilities
			WHERE node_id = $1`, nodeID).Scan(&gpuAvailable)
		if gpuAvailable {
			return "gpu"
		}
		// Node has no GPU — downgrade to CPU despite the caller's preference.
		return "cpu"
	}

	// Pure auto with no hints — check node capability.
	var gpuCount int
	_ = db.QueryRowContext(ctx, `
		SELECT COALESCE(gpu_count, 0)
		FROM node_capabilities
		WHERE node_id = $1`, nodeID).Scan(&gpuCount)
	if gpuCount > 0 {
		return "gpu"
	}
	return "cpu"
}

func (a *RuntimeActivator) estimateRAMMB(cfg *ModelConfig) int64 {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var ramMB int64
	_ = a.db.QueryRowContext(ctx, `
		SELECT COALESCE(required_memory_mb, 0)
		FROM runtime_requirements WHERE model_id = $1`, cfg.ModelID,
	).Scan(&ramMB)
	return ramMB
}

// httpHealthCheck calls GET /health on the endpoint URL.
// Returns true only on HTTP 200.  Connection refused (ECONNREFUSED) returns
// false — it is treated as "not yet ready" not as failure.
func (a *RuntimeActivator) httpHealthCheck(baseURL string) bool {
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get(baseURL + "/health")
	if err != nil {
		// Connection refused, network unreachable, etc. — not a failure.
		if strings.Contains(err.Error(), "connection refused") ||
			strings.Contains(err.Error(), "EOF") {
			return false
		}
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// injectReasoningFlag ensures that llamacpp containers for thinking-capable
// models with thinking DISABLED always start with "--reasoning off".
//
// This is the authoritative mechanism for disabling thinking at the server
// level. Per-request injection via chat_template_kwargs is unreliable because
// it depends on the model's Jinja template honouring the flag, whereas
// --reasoning off is enforced by the llama-server binary itself (b9821+).
//
// Logic:
//   - Only applies to backend == "llamacpp"
//   - Queries supports_thinking and thinking_enabled from the models table
//   - If supports_thinking=true AND thinking_enabled=false: prepend "--reasoning off"
//   - If "--reasoning" is already in extraArgs, leaves it unchanged (operator override)
//   - All other cases: returns extraArgs unmodified
func injectReasoningFlag(ctx context.Context, db *sqlx.DB, modelID string, backend string, extraArgs []string) []string {
	if backend != "llamacpp" {
		return extraArgs
	}

	// Check if --reasoning is already explicitly set by the operator.
	for _, a := range extraArgs {
		if a == "--reasoning" || a == "-rea" {
			return extraArgs // operator already controls this — don't override
		}
	}

	var supportsThinking, thinkingEnabled bool
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(supports_thinking, FALSE), COALESCE(thinking_enabled, FALSE)
		FROM models WHERE id = $1`, modelID,
	).Scan(&supportsThinking, &thinkingEnabled)
	if err != nil {
		return extraArgs // table column may not exist — safe default
	}

	// Model supports thinking but it's disabled by default → enforce at server level.
	if supportsThinking && !thinkingEnabled {
		return append([]string{"--reasoning", "off"}, extraArgs...)
	}

	return extraArgs
}
