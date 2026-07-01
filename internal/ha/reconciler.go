// Package ha — reconciler.go
//
// The Reconciler is the HA self-healing loop. It runs every 30 seconds and:
//  1. Loads the live runtime_replica_status view (desired vs actual).
//  2. For each model that is under-replicated or has LOST replicas:
//     a. Load the full model config (image, backend, gguf_path, etc.)
//     b. Allocate a unique port on the selected node via allocate_node_port()
//     c. Pre-create an agent_runtimes row with all required fields
//     d. Dispatch a fully-populated START_MODEL task (all fields the agent needs)
//     e. Write a runtime_recovery_log entry for every action.
//  3. Update reconciler_state with last sweep timestamp and counters.
//
// Design rule: the reconciler builds the SAME payload that DeployModel does —
// every field the agent validates (runtime_name, image, bind_port, backend)
// is present. No field is left to the agent to derive.
package ha

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/taskmanager"
	"go.uber.org/zap"
)

const (
	// ReconcileInterval is how often the reconciler runs its sweep.
	ReconcileInterval = 30 * time.Second
)

// Reconciler continuously compares desired replica state vs actual state
// and triggers recovery for under-replicated or lost models.
type Reconciler struct {
	db      *sqlx.DB
	taskMgr *taskmanager.Manager
	log     *zap.Logger
}

// NewReconciler constructs a Reconciler.
func NewReconciler(db *sqlx.DB, taskMgr *taskmanager.Manager, log *zap.Logger) *Reconciler {
	return &Reconciler{db: db, taskMgr: taskMgr, log: log}
}

// Start begins the reconciliation loop. Blocks until ctx is cancelled.
func (r *Reconciler) Start(ctx context.Context) {
	r.log.Info("HA reconciler started", zap.Duration("interval", ReconcileInterval))
	r.sweep(ctx) // immediate first sweep
	ticker := time.NewTicker(ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.log.Info("HA reconciler stopped")
			return
		case <-ticker.C:
			r.sweep(ctx)
		}
	}
}

// sweep runs one full reconciliation cycle.
func (r *Reconciler) sweep(ctx context.Context) {
	// Clean up failed/stuck containers before checking replica counts.
	// This ensures failed replicas don't block new healthy ones from spawning.
	r.sweepFailedContainers(ctx)

	statuses, err := r.loadReplicaStatuses(ctx)
	if err != nil {
		r.log.Warn("reconciler: failed to load replica statuses", zap.Error(err))
		return
	}

	var modelsChecked, recoveriesTriggered int
	for _, status := range statuses {
		modelsChecked++
		actions := r.plan(ctx, status)
		for _, action := range actions {
			if action.Action != "start_replica" {
				continue
			}
			if err := r.execute(ctx, status, action); err != nil {
				r.log.Warn("reconciler: recovery failed",
					zap.String("model", status.ModelName),
					zap.String("reason", action.Reason),
					zap.Error(err),
				)
			} else {
				recoveriesTriggered++
			}
		}
	}

	_, _ = r.db.ExecContext(ctx, `
		UPDATE reconciler_state
		SET last_sweep_at        = NOW(),
		    models_checked       = $1,
		    recoveries_triggered = $2,
		    updated_at           = NOW()
		WHERE singleton = TRUE`,
		modelsChecked, recoveriesTriggered,
	)

	if recoveriesTriggered > 0 {
		r.log.Info("reconciler sweep complete",
			zap.Int("models_checked", modelsChecked),
			zap.Int("recoveries_triggered", recoveriesTriggered),
		)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Planning
// ─────────────────────────────────────────────────────────────────────────────

func (r *Reconciler) plan(ctx context.Context, status ReplicaStatus) []ReconcileAction {
	if !status.AutoRecover {
		return nil
	}

	// Direct DB count of ALL non-terminal rows — bypasses view lag.
	// IMPORTANT: 'failed' is intentionally included here as non-terminal.
	// A container that failed health checks is still a Docker container on disk.
	// Excluding 'failed' causes the reconciler to spawn replacements while the
	// failed containers accumulate, leading to N×desired containers over time.
	// The stuck-runtime sweeper handles cleanup of failed containers separately.
	var nonTerminal int
	_ = r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM agent_runtimes
		WHERE model_id = $1
		  AND state NOT IN ('stopped','deleted','archived','unloaded','lost')`,
		status.ModelID,
	).Scan(&nonTerminal)

	if nonTerminal >= status.DesiredReplicas {
		return nil
	}

	// 6-minute cooldown: must be longer than the sweepFailedContainers grace
	// period (5 minutes) so a failed row is guaranteed to be moved to 'stopped'
	// before the cooldown expires and the reconciler is allowed to spawn again.
	// Using 90 seconds was shorter than the 5-minute sweep grace, which allowed
	// the cooldown to expire while old failed rows still counted as non-terminal,
	// causing repeated spawns and accumulation of containers.
	var recentLog int
	_ = r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM runtime_recovery_log
		WHERE model_id = $1
		  AND trigger   = 'reconcile'
		  AND created_at > NOW() - INTERVAL '6 minutes'`,
		status.ModelID,
	).Scan(&recentLog)

	if recentLog > 0 {
		return nil
	}

	// Recovery delay: when replicas are lost, wait before recovering
	// to give the node a chance to come back online.
	if status.LostReplicas > 0 {
		delay := r.recoveryDelay(ctx, status.ModelID)
		if time.Since(r.lostSince(ctx, status.ModelID)) < delay {
			return nil
		}
	}

	// Spawn exactly ONE new replica per sweep.
	node, err := r.selectNode(ctx, status)
	if err != nil {
		r.log.Warn("reconciler: no suitable node",
			zap.String("model", status.ModelName), zap.Error(err))
		return nil
	}

	return []ReconcileAction{{
		ModelID:    status.ModelID,
		ModelName:  status.ModelName,
		Action:     "start_replica",
		TargetNode: node,
		ReplicaIdx: nonTerminal,
		Reason: fmt.Sprintf("ha_recovery: non_terminal=%d lost=%d desired=%d",
			nonTerminal, status.LostReplicas, status.DesiredReplicas),
	}}
}

// ─────────────────────────────────────────────────────────────────────────────
// Execution — builds a fully-populated runtime row + START_MODEL task
// ─────────────────────────────────────────────────────────────────────────────

// runtimeConfig holds everything needed to start a container.
type runtimeConfig struct {
	ModelID          string
	ModelName        string
	Backend          string
	Image            string
	GGUFPath         string
	HFRepo           string
	HFFile           string
	HFToken          string
	ModelsVolume     string
	CtxSize          int
	NGPULayers       int
	ExecutionMode    string
	WorkloadPolicy   string
	MemoryLimit      string
	CPUThreads       string
	TensorParallel   int
	GPUMemoryUtil    float64
	MaxModelLen      int
	Dtype            string
	Quantization     string
	ExtraArgs        []string
	SupportsThinking bool
	ThinkingEnabled  bool
}

// loadRuntimeConfig reads all fields needed to build a StartModelPayload.
func (r *Reconciler) loadRuntimeConfig(ctx context.Context, modelID string) (*runtimeConfig, error) {
	var row struct {
		ModelID          string  `db:"model_id"`
		ModelName        string  `db:"model_name"`
		Backend          string  `db:"backend"`
		Image            string  `db:"image"`
		GGUFPath         string  `db:"gguf_path"`
		HFRepo           string  `db:"hf_repo"`
		HFFile           string  `db:"hf_file"`
		HFToken          string  `db:"hf_token"`
		ModelsVolume     string  `db:"models_volume"`
		CtxSize          int     `db:"ctx_size"`
		NGPULayers       int     `db:"n_gpu_layers"`
		ExecutionMode    string  `db:"execution_mode"`
		WorkloadPolicy   string  `db:"workload_policy"`
		MemoryLimit      string  `db:"memory_limit"`
		CPUThreads       string  `db:"cpu_threads"`
		TensorParallel   int     `db:"tensor_parallel"`
		GPUMemoryUtil    float64 `db:"gpu_memory_util"`
		MaxModelLen      int     `db:"max_model_len"`
		Dtype            string  `db:"dtype"`
		Quantization     string  `db:"quantization"`
		ExtraArgsJSON    string  `db:"extra_args_json"`
		SupportsThinking bool    `db:"supports_thinking"`
		ThinkingEnabled  bool    `db:"thinking_enabled"`
	}
	err := r.db.GetContext(ctx, &row, `
		SELECT
		    m.id                                        AS model_id,
		    m.name                                      AS model_name,
		    COALESCE(m.backend_type, 'llamacpp')        AS backend,
		    COALESCE(me.runtime_image, '')              AS image,
		    COALESCE(mrc.gguf_path,   '')               AS gguf_path,
		    COALESCE(mrc.hf_repo,     '')               AS hf_repo,
		    COALESCE(mrc.hf_file,     '')               AS hf_file,
		    COALESCE(mrc.hf_token,    '')               AS hf_token,
		    COALESCE(mrc.models_volume, '')             AS models_volume,
		    COALESCE(mrc.ctx_size,    4096)             AS ctx_size,
		    COALESCE(mrc.n_gpu_layers, 0)               AS n_gpu_layers,
		    COALESCE(mrc.execution_mode, 'auto')        AS execution_mode,
		    COALESCE(mrc.workload_policy, 'lazy_load')  AS workload_policy,
		    COALESCE(mrc.memory_limit, '')              AS memory_limit,
		    COALESCE(mrc.cpu_threads::text, '')         AS cpu_threads,
		    COALESCE(mrc.tensor_parallel, 1)            AS tensor_parallel,
		    COALESCE(mrc.gpu_memory_util, 0.90)         AS gpu_memory_util,
		    COALESCE(mrc.max_model_len, 0)              AS max_model_len,
		    COALESCE(mrc.dtype, 'auto')                 AS dtype,
		    COALESCE(mrc.quantization, '')              AS quantization,
		    COALESCE(mrc.extra_args::text, '[]')        AS extra_args_json,
		    COALESCE(m.supports_thinking, FALSE)        AS supports_thinking,
		    COALESCE(m.thinking_enabled, FALSE)         AS thinking_enabled
		FROM models m
		LEFT JOIN model_endpoints me  ON me.model_id = m.id AND me.lifecycle_state NOT IN ('deleted')
		LEFT JOIN model_runtime_configs mrc ON mrc.model_id = m.id
		WHERE m.id = $1 AND m.enabled = TRUE
		ORDER BY me.priority ASC
		LIMIT 1`, modelID)
	if err != nil {
		return nil, fmt.Errorf("loadRuntimeConfig %s: %w", modelID, err)
	}

	var extraArgs []string
	if row.ExtraArgsJSON != "" && row.ExtraArgsJSON != "[]" {
		_ = json.Unmarshal([]byte(row.ExtraArgsJSON), &extraArgs)
	}

	// Inject --reasoning off for llamacpp thinking models with thinking disabled.
	// This is the same logic as injectReasoningFlag in runtimemgr/activator.go.
	// HA-recovered replicas must have the same startup args as the original deploy.
	if row.Backend == "llamacpp" && row.SupportsThinking && !row.ThinkingEnabled {
		alreadySet := false
		for _, a := range extraArgs {
			if a == "--reasoning" || a == "-rea" {
				alreadySet = true
				break
			}
		}
		if !alreadySet {
			extraArgs = append([]string{"--reasoning", "off"}, extraArgs...)
		}
	}

	return &runtimeConfig{
		ModelID:          row.ModelID,
		ModelName:        row.ModelName,
		Backend:          row.Backend,
		Image:            row.Image,
		GGUFPath:         row.GGUFPath,
		HFRepo:           row.HFRepo,
		HFFile:           row.HFFile,
		HFToken:          row.HFToken,
		ModelsVolume:     row.ModelsVolume,
		CtxSize:          row.CtxSize,
		NGPULayers:       row.NGPULayers,
		ExecutionMode:    row.ExecutionMode,
		WorkloadPolicy:   row.WorkloadPolicy,
		MemoryLimit:      row.MemoryLimit,
		CPUThreads:       row.CPUThreads,
		TensorParallel:   row.TensorParallel,
		GPUMemoryUtil:    row.GPUMemoryUtil,
		MaxModelLen:      row.MaxModelLen,
		Dtype:            row.Dtype,
		Quantization:     row.Quantization,
		ExtraArgs:        extraArgs,
		SupportsThinking: row.SupportsThinking,
		ThinkingEnabled:  row.ThinkingEnabled,
	}, nil
}

// allocatePort calls the DB allocate_node_port() function.
// The function uses pg_advisory_xact_lock internally and an atomic CTE INSERT,
// so it is safe to call without wrapping in a transaction.
func (r *Reconciler) allocatePort(ctx context.Context, nodeID, modelID string) (int, error) {
	var port int
	err := r.db.QueryRowContext(ctx,
		`SELECT allocate_node_port($1::uuid, $2::uuid)`, nodeID, modelID,
	).Scan(&port)
	if err != nil {
		return 0, fmt.Errorf("allocate_node_port: %w", err)
	}
	if port == 0 {
		return 0, fmt.Errorf("no free ports on node %s (range 8100–8999 exhausted)", nodeID)
	}
	return port, nil
}

// nodeIP returns the IP or hostname of a node for container bind_host.
func (r *Reconciler) nodeIP(ctx context.Context, nodeID string) string {
	var ip string
	_ = r.db.QueryRowContext(ctx,
		`SELECT COALESCE(host(ip_address), hostname, 'localhost') FROM nodes WHERE id = $1`, nodeID,
	).Scan(&ip)
	if ip == "" {
		return "localhost"
	}
	return ip
}

// execute carries out a single start_replica action.
// It mimics what the admin DeployModel handler does: pre-creates the runtime
// row with all required fields, allocates a port, then dispatches the task.
func (r *Reconciler) execute(ctx context.Context, status ReplicaStatus, action ReconcileAction) error {
	logID := uuid.New().String()

	// ── 1. Load full model config ─────────────────────────────────────────────
	cfg, err := r.loadRuntimeConfig(ctx, action.ModelID)
	if err != nil {
		r.recordLog(ctx, logID, action, "", "failed", "loadRuntimeConfig: "+err.Error())
		return err
	}
	if cfg.Image == "" {
		r.recordLog(ctx, logID, action, "", "failed", "model has no runtime_image configured")
		return fmt.Errorf("model %s has no runtime_image", action.ModelName)
	}

	// ── 2. Allocate unique port on the target node ────────────────────────────
	port, err := r.allocatePort(ctx, action.TargetNode, action.ModelID)
	if err != nil {
		r.recordLog(ctx, logID, action, "", "failed", err.Error())
		return err
	}
	bindHost := r.nodeIP(ctx, action.TargetNode)

	// ── 3. Generate unique runtime identity ──────────────────────────────────
	runtimeID := uuid.New().String()
	// Make container name unique per replica: nexus-<model>-r<idx>-<short-id>
	suffix := strings.Replace(runtimeID, "-", "", -1)[:6]
	containerName := fmt.Sprintf("nexus-%s-r%d-%s", sanitize(cfg.ModelName), action.ReplicaIdx, suffix)

	// ── 4. Resolve execution mode ─────────────────────────────────────────────
	effectiveMode := r.resolveExecutionMode(ctx, action.TargetNode, cfg)
	gpuDevicesJSON := "[]"
	nGPULayers := cfg.NGPULayers
	if effectiveMode == "cpu" {
		nGPULayers = 0
	}

	// ── 5. Insert agent_runtimes row FIRST (task FK requires it) ─────────────
	res, dbErr := r.db.ExecContext(ctx, `
		INSERT INTO agent_runtimes
		  (id, node_id, endpoint_id, model_id, runtime_name, backend,
		   state, gpu_ids, bind_host, bind_port, cpu_affinity, numa_node,
		   requested_mode, effective_mode, workload_policy,
		   replica_index, recovery_attempt)
		VALUES ($1,$2,NULL,$3,$4,$5,'pending',
		        $6::jsonb,$7,$8,'',-1,
		        $9,$10,$11,$12,1)`,
		runtimeID, action.TargetNode, action.ModelID, containerName, cfg.Backend,
		gpuDevicesJSON, bindHost, port,
		cfg.ExecutionMode, effectiveMode, cfg.WorkloadPolicy,
		action.ReplicaIdx,
	)
	if dbErr != nil {
		// Release port since we won't use it
		_, _ = r.db.ExecContext(ctx, `SELECT release_node_port($1::uuid, $2)`, action.TargetNode, port)
		r.recordLog(ctx, logID, action, runtimeID, "failed", "insert agent_runtime: "+dbErr.Error())
		return fmt.Errorf("insert runtime row: %w", dbErr)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_, _ = r.db.ExecContext(ctx, `SELECT release_node_port($1::uuid, $2)`, action.TargetNode, port)
		return fmt.Errorf("insert runtime row: 0 rows affected")
	}

	// Link the port lease to the runtime row
	_, _ = r.db.ExecContext(ctx, `
		UPDATE node_port_leases SET runtime_id = $1
		WHERE node_id = $2 AND port = $3 AND released_at IS NULL`,
		runtimeID, action.TargetNode, port,
	)

	// ── 6. Build fully-populated StartModelPayload ────────────────────────────
	modelsVolume := cfg.ModelsVolume
	if modelsVolume == "" {
		modelsVolume = "llamacpp_models"
	}
	ctxSize := cfg.CtxSize
	if ctxSize == 0 {
		ctxSize = 4096
	}

	payload := taskmanager.StartModelPayload{
		RuntimeID:      runtimeID,
		ModelID:        cfg.ModelID,
		RuntimeName:    containerName,
		Backend:        cfg.Backend,
		Image:          cfg.Image,
		ModelName:      cfg.ModelName,
		ServedAs:       cfg.ModelName,
		BindHost:       bindHost,
		BindPort:       port,
		GPUDevices:     nil, // CPU mode or resolved from node
		MemoryLimit:    cfg.MemoryLimit,
		CPULimit:       cfg.CPUThreads,
		GGUFPath:       cfg.GGUFPath,
		HFRepo:         cfg.HFRepo,
		HFFile:         cfg.HFFile,
		HFToken:        cfg.HFToken,
		ModelsVolume:   modelsVolume,
		CtxSize:        ctxSize,
		NGPULayers:     nGPULayers,
		TensorParallel: cfg.TensorParallel,
		GPUMemoryUtil:  cfg.GPUMemoryUtil,
		MaxModelLen:    cfg.MaxModelLen,
		Dtype:          cfg.Dtype,
		Quantization:   cfg.Quantization,
		ExtraArgs:      cfg.ExtraArgs, // includes --reasoning off when thinking is disabled
		ExecutionMode:  effectiveMode,
		WorkloadPolicy: cfg.WorkloadPolicy,
		Env:            map[string]string{},
	}
	if cfg.HFToken != "" {
		payload.Env["HUGGING_FACE_HUB_TOKEN"] = cfg.HFToken
	}

	// ── 7. Dispatch START_MODEL task ──────────────────────────────────────────
	taskID, taskErr := r.taskMgr.Enqueue(ctx, action.TargetNode,
		taskmanager.TaskStartModel, payload,
		taskmanager.WithPriority(85),
		taskmanager.WithActor("ha-reconciler"),
		taskmanager.WithRuntimeID(runtimeID),
		taskmanager.WithIdempotencyKey(fmt.Sprintf("ha-recover:%s:%s:%s", action.ModelID, action.TargetNode, runtimeID)),
	)
	if taskErr != nil {
		// Clean up: release port, delete orphan runtime row
		_, _ = r.db.ExecContext(ctx, `UPDATE agent_runtimes SET state='failed' WHERE id=$1`, runtimeID)
		_, _ = r.db.ExecContext(ctx, `SELECT release_node_port($1::uuid, $2)`, action.TargetNode, port)
		r.recordLog(ctx, logID, action, runtimeID, "failed", "enqueue: "+taskErr.Error())
		return fmt.Errorf("enqueue START_MODEL: %w", taskErr)
	}

	r.recordLog(ctx, logID, action, runtimeID, "success", action.Reason)

	r.log.Info("HA recovery initiated",
		zap.String("model", action.ModelName),
		zap.String("node", action.TargetNode),
		zap.String("runtime_id", runtimeID),
		zap.String("container", containerName),
		zap.String("task_id", taskID),
		zap.Int("port", port),
		zap.Int("replica", action.ReplicaIdx),
	)
	return nil
}

// resolveExecutionMode determines whether to use GPU or CPU on the target node.
func (r *Reconciler) resolveExecutionMode(ctx context.Context, nodeID string, cfg *runtimeConfig) string {
	if cfg.ExecutionMode == "cpu" {
		return "cpu"
	}
	if cfg.ExecutionMode == "gpu" {
		return "gpu"
	}
	// auto: check node capability
	var gpuCount int
	_ = r.db.QueryRowContext(ctx,
		`SELECT COALESCE(gpu_count,0) FROM node_capabilities WHERE node_id=$1`, nodeID,
	).Scan(&gpuCount)
	if gpuCount > 0 {
		return "gpu"
	}
	return "cpu"
}

// ─────────────────────────────────────────────────────────────────────────────
// Node selection
// ─────────────────────────────────────────────────────────────────────────────

func (r *Reconciler) selectNode(ctx context.Context, status ReplicaStatus) (string, error) {
	type nodeRow struct {
		ID           string `db:"id"`
		Hostname     string `db:"hostname"`
		FreeVRAMMB   int64  `db:"free_vram_mb"`
		RuntimeCount int    `db:"runtime_count"`
	}
	var nodes []nodeRow
	err := r.db.SelectContext(ctx, &nodes, `
		SELECT n.id, n.hostname,
		       COALESCE(SUM(d.vram_mb) - SUM(COALESCE(gt.memory_used_mb,0)), 0) AS free_vram_mb,
		       COUNT(ar.id) FILTER (WHERE ar.state IN ('active','warm','ready','idle')) AS runtime_count
		FROM nodes n
		LEFT JOIN gpu_nodes gn ON gn.node_id = n.id
		LEFT JOIN gpu_devices d ON d.node_id = gn.id
		LEFT JOIN LATERAL (
		    SELECT memory_used_mb FROM gpu_telemetry WHERE device_id=d.id ORDER BY recorded_at DESC LIMIT 1
		) gt ON TRUE
		LEFT JOIN agent_runtimes ar ON ar.node_id = n.id
		WHERE n.status IN ('online','degraded')
		  AND n.cordoned = FALSE
		GROUP BY n.id, n.hostname ORDER BY n.id`)
	if err != nil || len(nodes) == 0 {
		return "", fmt.Errorf("no online uncordoned nodes available")
	}

	var existingNodes []string
	_ = r.db.SelectContext(ctx, &existingNodes, `
		SELECT DISTINCT node_id::text FROM agent_runtimes
		WHERE model_id=$1 AND state NOT IN ('stopped','deleted','archived','unloaded','lost')`,
		status.ModelID)
	existingSet := make(map[string]bool, len(existingNodes))
	for _, id := range existingNodes {
		existingSet[id] = true
	}

	policy := PlacementPolicy(status.PlacementPolicy)

	// anti_affinity: HARD rule — never place on a node that already has a replica
	if policy == PolicyAntiAffinity {
		for _, n := range nodes {
			if !existingSet[n.ID] {
				return n.ID, nil
			}
		}
		// All nodes have a replica — if single-node cluster, allow packing as fallback
		if len(nodes) == 1 {
			return nodes[0].ID, nil
		}
		return "", fmt.Errorf("anti_affinity: no node available without existing replica (have %d nodes, %d with replicas)", len(nodes), len(existingSet))
	}

	// spread: prefer nodes without existing replicas, fall back to least-loaded
	if policy == PolicySpread {
		for _, n := range nodes {
			if !existingSet[n.ID] {
				return n.ID, nil
			}
		}
		// All nodes have replicas — pick the one with most free VRAM
	}

	// pack: prefer nodes that already have replicas of this model
	if policy == PolicyPack {
		for _, n := range nodes {
			if existingSet[n.ID] {
				return n.ID, nil
			}
		}
		// No node has a replica yet — fall through to default
	}

	// Default: most free VRAM
	best := nodes[0]
	for _, n := range nodes[1:] {
		if n.FreeVRAMMB > best.FreeVRAMMB {
			best = n
		}
	}
	return best.ID, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// DB helpers
// ─────────────────────────────────────────────────────────────────────────────

func (r *Reconciler) loadReplicaStatuses(ctx context.Context) ([]ReplicaStatus, error) {
	var rows []ReplicaStatus
	err := r.db.SelectContext(ctx, &rows, `
		SELECT model_id, model_name, desired_replicas, min_available,
		       placement_policy, auto_recover,
		       active_replicas, starting_replicas, idle_replicas,
		       lost_replicas, node_count, ha_status
		FROM runtime_replica_status WHERE desired_replicas > 0`)
	return rows, err
}

func (r *Reconciler) recoveryDelay(ctx context.Context, modelID string) time.Duration {
	var secs int
	_ = r.db.GetContext(ctx, &secs,
		`SELECT COALESCE(recovery_delay_s,30) FROM model_replica_specs WHERE model_id=$1`, modelID)
	if secs <= 0 {
		return 30 * time.Second
	}
	return time.Duration(secs) * time.Second
}

func (r *Reconciler) lostSince(ctx context.Context, modelID string) time.Time {
	var t time.Time
	_ = r.db.GetContext(ctx, &t,
		`SELECT MIN(updated_at) FROM agent_runtimes WHERE model_id=$1 AND state='lost'`, modelID)
	if t.IsZero() {
		return time.Now()
	}
	return t
}

func (r *Reconciler) recordLog(ctx context.Context, logID string, action ReconcileAction, runtimeID, status, reason string) {
	var nodeIDVal, runtimeIDVal interface{}
	if action.TargetNode != "" {
		nodeIDVal = action.TargetNode
	}
	if runtimeID != "" {
		runtimeIDVal = runtimeID
	}
	_, _ = r.db.ExecContext(ctx, `
		INSERT INTO runtime_recovery_log
		  (id, model_id, model_name, new_runtime_id, new_node_id,
		   trigger, status, reason, replica_index, completed_at)
		VALUES ($1,$2,$3,$4,$5,'reconcile',$6,$7,$8,NOW())
		ON CONFLICT (id) DO UPDATE SET
		  status=$6, reason=$7, completed_at=NOW()`,
		logID, action.ModelID, action.ModelName,
		runtimeIDVal, nodeIDVal,
		status, reason, action.ReplicaIdx,
	)
}

// sanitize replaces characters invalid in container names.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32 // lowercase
		}
		return '-'
	}, s)
}

// sweepFailedContainers marks old failed agent_runtimes as 'stopped' once
// their container has been running for more than the failure grace period.
// This prevents the non-terminal count from being inflated by failed rows
// and allows the reconciler to spawn fresh replacements.
//
// Strategy: a 'failed' runtime that is older than 5 minutes AND whose
// container name is for an HA replica (contains "-r0-", "-r1-", etc.) is
// transitioned to 'stopped' so it no longer blocks replica count checks.
// The node agent is responsible for actually removing the Docker container.
func (r *Reconciler) sweepFailedContainers(ctx context.Context) {
	res, _ := r.db.ExecContext(ctx, `
		UPDATE agent_runtimes
		SET state      = 'stopped',
		    error_msg  = COALESCE(error_msg, '') || ' [ha-sweep: moved failed→stopped after grace period]',
		    updated_at = NOW()
		WHERE state = 'failed'
		  AND updated_at < NOW() - INTERVAL '5 minutes'`)
	if n, _ := res.RowsAffected(); n > 0 {
		r.log.Info("HA sweep: failed runtimes moved to stopped",
			zap.Int64("count", n),
		)
	}
}

// Ensure json import is used
var _ = json.Marshal
