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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/modelguard"
	"github.com/nexusllm/nexusllm/internal/nodeaddr"
	"github.com/nexusllm/nexusllm/internal/replicaguard"
	"github.com/nexusllm/nexusllm/internal/runtime"
	"github.com/nexusllm/nexusllm/internal/taskmanager"
	"go.uber.org/zap"
)

// ErrRecoveryChainExhausted is returned by executeReturningID when the
// logical replica identified by a ReconcileAction's RecoveredFrom has already
// reached maxUnhealthyRecoveryAttempts — the row has been marked 'lost' and
// no replacement was created. Callers must not retry immediately.
var ErrRecoveryChainExhausted = errors.New("recovery chain exhausted: logical replica marked lost")

const (
	// ReconcileInterval is how often the reconciler runs its sweep.
	ReconcileInterval = 30 * time.Second
)

// Reconciler continuously compares desired replica state vs actual state
// and triggers recovery for under-replicated or lost models.
type Reconciler struct {
	db       *sqlx.DB
	taskMgr  *taskmanager.Manager
	registry *runtime.Registry // used for backend adapter lookups (PrepareStartupArgs)
	log      *zap.Logger
}

// NewReconciler constructs a Reconciler.
// registry is optional (nil disables PrepareStartupArgs — safe default returned).
func NewReconciler(db *sqlx.DB, taskMgr *taskmanager.Manager, registry *runtime.Registry, log *zap.Logger) *Reconciler {
	return &Reconciler{db: db, taskMgr: taskMgr, registry: registry, log: log}
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
	// ── Phase 1: rolling-replacement lifecycle management ─────────────────
	// Drive the UNHEALTHY → replacement → DRAINING → STOPPED state machine
	// before the regular under-replication check runs.
	r.rollingReplacementSweep(ctx)

	// ── Phase 2: clean up failed/stuck containers ─────────────────────────
	// This ensures failed replicas don't inflate the non-terminal count.
	r.sweepFailedContainers(ctx)

	// ── Phase 3: under-replication recovery ──────────────────────────────
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
			newRuntimeID, _, err := r.executeReturningID(ctx, status, action)
			if err != nil {
				if !errors.Is(err, ErrRecoveryChainExhausted) {
					r.log.Warn("reconciler: recovery failed",
						zap.String("model", status.ModelName),
						zap.String("reason", action.Reason),
						zap.Error(err),
					)
				}
				continue
			}
			recoveriesTriggered++
			if action.RecoveredFrom != "" && newRuntimeID != "" {
				// Advance the chain: mark the predecessor replaced so the NEXT
				// under-replication top-up (if this new replica also fails)
				// chains off THIS row instead of re-reading the same stale
				// predecessor's attempt count forever.
				_, _ = r.db.ExecContext(ctx,
					`UPDATE agent_runtimes SET replaced_by = $1, updated_at = NOW() WHERE id = $2`,
					newRuntimeID, action.RecoveredFrom)
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
	// IMPORTANT: 'failed' and 'unhealthy' are treated as terminal here,
	// matching the claim_replica_slot() DB function (migration 032).
	// A failed container is definitively gone; an unhealthy one has a rolling
	// replacement already in progress. Neither should block a new cold-start.
	var nonTerminal int
	_ = r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM agent_runtimes
		WHERE model_id = $1
		  AND state NOT IN (
		      'stopped','deleted','archived','unloaded','lost',
		      'draining','failed','unhealthy'
		  )`,
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
	//
	// Exception: if the most-recent recovery log was triggered by a confirmed
	// container death (container-dead tag), the grace was skipped and the failed
	// row is already stopped — use a 60s cooldown to allow fast recovery.
	var recentLog int
	_ = r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM runtime_recovery_log
		WHERE model_id = $1
		  AND trigger   = 'reconcile'
		  AND created_at > NOW() - INTERVAL '60 seconds'
		  AND reason LIKE '%container-dead%'`,
		status.ModelID,
	).Scan(&recentLog)

	if recentLog == 0 {
		// Standard path — use 6-minute cooldown.
		_ = r.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM runtime_recovery_log
			WHERE model_id = $1
			  AND trigger   = 'reconcile'
			  AND created_at > NOW() - INTERVAL '6 minutes'`,
			status.ModelID,
		).Scan(&recentLog)
	}

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

	// Bounded recovery: this under-replication top-up is filling a gap left
	// by the most recent not-yet-replaced failure for this model, if any —
	// chain it via RecoveredFrom so it shares the SAME attempt budget as the
	// rolling-replacement path below, instead of being a second, unbounded
	// recovery mechanism (production forensic audit: this path previously had
	// only a time-based cooldown — 6 minutes, or 60s for confirmed
	// container-death — with no cap on total attempts, and was the dominant
	// source of unbounded replacement rows since claim_replica_slot()
	// deliberately excludes 'unhealthy'/'failed' rows from its capacity
	// count).
	recoveredFrom := r.mostRecentUnrepairedFailure(ctx, status.ModelID)

	return []ReconcileAction{{
		ModelID:       status.ModelID,
		ModelName:     status.ModelName,
		Action:        "start_replica",
		TargetNode:    node,
		ReplicaIdx:    r.nextReplicaIndex(ctx, status.ModelID),
		RecoveredFrom: recoveredFrom,
		Reason: fmt.Sprintf("ha_recovery: non_terminal=%d lost=%d desired=%d",
			nonTerminal, status.LostReplicas, status.DesiredReplicas),
	}}
}

// mostRecentUnrepairedFailure returns the id of the most recently terminal
// (failed/stopped/lost) agent_runtimes row for modelID that has not already
// been linked to a replacement (replaced_by IS NULL), or "" if none exists
// (a brand-new deploy with no prior failure — attempt 1 of a fresh chain).
// This is the logical-replica identity plan()'s under-replication top-up
// chains its bounded-recovery attempt count from.
func (r *Reconciler) mostRecentUnrepairedFailure(ctx context.Context, modelID string) string {
	var id string
	err := r.db.GetContext(ctx, &id, `
		SELECT id FROM agent_runtimes
		WHERE model_id = $1
		  AND state IN ('failed','stopped','lost')
		  AND replaced_by IS NULL
		ORDER BY updated_at DESC LIMIT 1`, modelID)
	if err != nil {
		return ""
	}
	return id
}

// nextRecoveryAttempt computes the attempt number for a new replacement in
// the logical recovery chain rooted at recoveredFrom (empty = brand new
// chain — attempt 1, always allowed). Returns ok=false when creating this
// attempt would exceed maxUnhealthyRecoveryAttempts; the caller must not
// create the replacement row and must mark the chain terminal instead.
//
// This is the single, shared bounded-recovery check for BOTH creation paths
// that can replace a failing replica (plan()'s under-replication top-up and
// handleUnhealthyReplica's rolling replacement) — previously only the latter
// enforced any cap at all.
func (r *Reconciler) nextRecoveryAttempt(ctx context.Context, recoveredFrom string) (attempt int, ok bool) {
	if recoveredFrom == "" {
		return 1, true
	}
	var prior int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COALESCE(recovery_attempt, 0) FROM agent_runtimes WHERE id = $1`, recoveredFrom,
	).Scan(&prior); err != nil {
		// Predecessor row vanished (e.g. hard-deleted) — treat this as the
		// start of a fresh chain rather than blocking recovery entirely.
		return 1, true
	}
	next := prior + 1
	return next, next <= maxUnhealthyRecoveryAttempts
}

// nextReplicaIndex picks the smallest non-negative integer not currently
// held by any non-terminal replica of this model, so the human-readable
// "-r<N>-" container-name label doesn't collide during legitimate surge
// overlap (forensic audit, Case File 003, round 6 — production data showed
// two simultaneously-running replicas both labeled "-r1-", which is a
// confusing/misleading label collision, not a capacity violation: ClaimSlot
// already correctly bounds the total count to desired+max_surge independent
// of what label each row gets). replica_index is not treated as a unique
// logical identity anywhere in the codebase — id (UUID) is — so this is a
// display-collision fix, not a correctness fix, and intentionally does not
// add a DB uniqueness constraint that could reject a legitimate surge insert.
func (r *Reconciler) nextReplicaIndex(ctx context.Context, modelID string) int {
	return nextReplicaIndexUsing(ctx, r.db, modelID)
}

// nextReplicaIndexTx is the transactional twin of nextReplicaIndex — it MUST
// be called after ClaimSlot has acquired the per-model advisory lock inside
// tx, so this is the AUTHORITATIVE index actually written to the new row.
// Calling nextReplicaIndex before the transaction begins (as
// action.ReplicaIdx is, for the container-name label only) is a
// best-effort/display-only estimate with a TOCTOU window between concurrent
// callers; calling it again here, inside the lock, closes that window for
// the value that actually matters (production evidence: 12 rows created
// within ~400ms all sharing replica_index=0 on the same node — the
// pre-transaction estimate raced across concurrent reconcile passes).
func (r *Reconciler) nextReplicaIndexTx(ctx context.Context, tx *sqlx.Tx, modelID string) int {
	return nextReplicaIndexUsing(ctx, tx, modelID)
}

// replicaIndexQueryer is satisfied by both *sqlx.DB and *sqlx.Tx.
type replicaIndexQueryer interface {
	SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
}

func nextReplicaIndexUsing(ctx context.Context, q replicaIndexQueryer, modelID string) int {
	var used []int
	_ = q.SelectContext(ctx, &used, `
		SELECT COALESCE(replica_index, -1) FROM agent_runtimes
		WHERE model_id = $1
		  AND state NOT IN (
		      'stopped','deleted','archived','unloaded','lost',
		      'draining','failed','unhealthy'
		  )`, modelID)
	taken := make(map[int]bool, len(used))
	for _, idx := range used {
		taken[idx] = true
	}
	for i := 0; ; i++ {
		if !taken[i] {
			return i
		}
	}
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
	GPUDevices       []int
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
	Env              map[string]string
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
		GPUDevicesJSON   string  `db:"gpu_devices_json"`
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
		EnvJSON          string  `db:"env_json"`
		SupportsThinking bool    `db:"supports_thinking"`
		ThinkingEnabled  bool    `db:"thinking_enabled"`
	}
	err := r.db.GetContext(ctx, &row, `
		SELECT
		    m.id                                        AS model_id,
		    m.name                                      AS model_name,
		    COALESCE(m.backend_type, 'openai_compat')     AS backend,
		    COALESCE(me.runtime_image, '')              AS image,
		    COALESCE(mrc.gguf_path,   '')               AS gguf_path,
		    COALESCE(mrc.hf_repo,     '')               AS hf_repo,
		    COALESCE(mrc.hf_file,     '')               AS hf_file,
		    COALESCE(mrc.hf_token,    '')               AS hf_token,
		    COALESCE(mrc.models_volume, '')             AS models_volume,
		    COALESCE(mrc.ctx_size,    4096)             AS ctx_size,
		    COALESCE(mrc.n_gpu_layers, 0)               AS n_gpu_layers,
		    COALESCE(mrc.gpu_devices::text, '[]')       AS gpu_devices_json,
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
		    COALESCE(mrc.env::text, '{}')               AS env_json,
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

	var gpuDevices []int
	if row.GPUDevicesJSON != "" && row.GPUDevicesJSON != "[]" {
		_ = json.Unmarshal([]byte(row.GPUDevicesJSON), &gpuDevices)
	}

	var env map[string]string
	if row.EnvJSON != "" && row.EnvJSON != "{}" {
		_ = json.Unmarshal([]byte(row.EnvJSON), &env)
	}
	if env == nil {
		env = map[string]string{}
	}

	// Delegate startup arg preparation to the backend adapter.
	// This keeps all backend-specific logic (e.g. --reasoning off for llamacpp)
	// inside the adapter — the reconciler passes generic capability flags only.
	if r.registry != nil {
		caps := runtime.ModelStartupCaps{
			SupportsThinking: row.SupportsThinking,
			ThinkingEnabled:  row.ThinkingEnabled,
		}
		extraArgs = r.registry.BackendForType(row.Backend).PrepareStartupArgs(caps, extraArgs)
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
		GPUDevices:       gpuDevices,
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
		Env:              env,
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

// nodeIP returns the canonical reachable address of a node for container
// bind_host. Delegates to nodeaddr.CanonicalHost — the single shared
// implementation — rather than duplicating the resolution query here.
func (r *Reconciler) nodeIP(ctx context.Context, nodeID string) string {
	return nodeaddr.CanonicalHost(ctx, r.db, nodeID)
}

// executeReturningID carries out a single start_replica action.
// It mimics what the admin DeployModel handler does: pre-creates the runtime
// row with all required fields, allocates a port, then dispatches the task.
// The returned attempt is the position of the created row within its
// recovery chain (1 for a brand-new/first attempt) — callers that maintain a
// persistent "hub" row across many attempts (handleUnhealthyReplica) must
// write this value back onto that hub's own recovery_attempt column so the
// NEXT call's nextRecoveryAttempt lookup sees the correct, current chain
// depth rather than a stale one.
func (r *Reconciler) executeReturningID(ctx context.Context, status ReplicaStatus, action ReconcileAction) (string, int, error) {
	logID := uuid.New().String()

	// ── 0. Bounded recovery gate ───────────────────────────────────────────────
	// Checked BEFORE any side effect (port allocation, container naming) so an
	// exhausted chain costs nothing beyond one SELECT. This is the single
	// shared enforcement point for BOTH callers of this function (plan()'s
	// under-replication top-up and handleUnhealthyReplica's rolling
	// replacement) — previously only the latter had any cap at all, and even
	// that cap lived on a per-row counter that was never actually carried
	// forward into replacement rows (see migration 060 / Case File 004).
	attempt, attemptOK := r.nextRecoveryAttempt(ctx, action.RecoveredFrom)
	if !attemptOK {
		reason := fmt.Sprintf("recovery chain exhausted after %d attempts — manual intervention required", attempt-1)
		_, _ = r.db.ExecContext(ctx, `
			UPDATE agent_runtimes
			SET state = 'lost', error_msg = $2, updated_at = NOW()
			WHERE id = $1 AND state NOT IN ('lost','stopped','deleted','archived')`,
			action.RecoveredFrom, reason)
		abandonAction := action
		abandonAction.Action = "abandon_replacement"
		abandonAction.ReplicaIdx = -1
		abandonAction.Reason = reason
		r.recordLog(ctx, logID, abandonAction, action.RecoveredFrom, "abandoned", reason)
		r.log.Error("bounded recovery: chain exhausted — marking logical replica lost, no further automatic replacement",
			zap.String("model", action.ModelName),
			zap.String("recovered_from", action.RecoveredFrom),
			zap.Int("attempt", attempt),
		)
		return "", 0, ErrRecoveryChainExhausted
	}

	// ── 1. Load full model config ─────────────────────────────────────────────
	cfg, err := r.loadRuntimeConfig(ctx, action.ModelID)
	if err != nil {
		r.recordLog(ctx, logID, action, "", "failed", "loadRuntimeConfig: "+err.Error())
		return "", 0, err
	}
	if cfg.Image == "" {
		r.recordLog(ctx, logID, action, "", "failed", "model has no runtime_image configured")
		return "", 0, fmt.Errorf("model %s has no runtime_image", action.ModelName)
	}

	// ── 2. Allocate unique port on the target node ────────────────────────────
	port, err := r.allocatePort(ctx, action.TargetNode, action.ModelID)
	if err != nil {
		r.recordLog(ctx, logID, action, "", "failed", err.Error())
		return "", 0, err
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

	// ── 5. Atomically claim replica slot + insert agent_runtimes row ─────────
	// The claim_replica_slot() DB function acquires a per-model advisory lock,
	// re-counts non-terminal rows inside that lock, and returns TRUE only when
	// count < desired.  By doing the INSERT in the same transaction we guarantee
	// no other concurrent caller can also claim a slot between the check and the
	// insert — eliminating the TOCTOU race that causes N×desired containers.
	tx, txErr := r.db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if txErr != nil {
		_, _ = r.db.ExecContext(ctx, `SELECT release_node_port($1::uuid, $2)`, action.TargetNode, port)
		r.recordLog(ctx, logID, action, "", "failed", "begin tx: "+txErr.Error())
		return "", 0, fmt.Errorf("begin transaction: %w", txErr)
	}
	// Always rollback on error paths; committed tx ignores this.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Re-check capacity inside the transaction with the advisory lock held.
	// Pass max_surge so the slot guard allows surge replicas during rolling replacement.
	maxSurge := status.MaxSurge
	if maxSurge < 1 {
		maxSurge = 1
	}
	slotAvailable, slotErr := replicaguard.ClaimSlot(ctx, tx, action.ModelID, status.DesiredReplicas, maxSurge)
	if slotErr != nil {
		_, _ = r.db.ExecContext(ctx, `SELECT release_node_port($1::uuid, $2)`, action.TargetNode, port)
		r.recordLog(ctx, logID, action, "", "skipped", "claim_replica_slot error: "+slotErr.Error())
		return "", 0, fmt.Errorf("claim_replica_slot: %w", slotErr)
	}
	if !slotAvailable {
		_, _ = r.db.ExecContext(ctx, `SELECT release_node_port($1::uuid, $2)`, action.TargetNode, port)
		r.recordLog(ctx, logID, action, "", "skipped",
			fmt.Sprintf("at capacity: desired=%d — slot claimed by concurrent caller", status.DesiredReplicas))
		r.log.Info("HA reconciler: replica slot already taken by concurrent caller — skipping",
			zap.String("model", action.ModelName),
			zap.Int("desired", status.DesiredReplicas),
		)
		return "", 0, nil // not an error — another process already handled it
	}

	// Compute the authoritative replica_index INSIDE the advisory-lock scope
	// ClaimSlot just acquired, rather than before the transaction began (as
	// action.ReplicaIdx was computed) — this closes the TOCTOU window where
	// two concurrent callers for the same model could each read "index 0 is
	// free" before either had committed (production evidence: 12 rows created
	// within ~400ms all sharing replica_index=0 on the same node). Existing
	// non-terminal rows still exclude 'unhealthy'/'failed'/etc per the same
	// list ClaimSlot uses, matching nextReplicaIndex's semantics exactly.
	replicaIdx := r.nextReplicaIndexTx(ctx, tx, action.ModelID)

	res, dbErr := tx.ExecContext(ctx, `
		INSERT INTO agent_runtimes
		  (id, node_id, endpoint_id, model_id, runtime_name, backend,
		   state, gpu_ids, bind_host, bind_port, cpu_affinity, numa_node,
		   requested_mode, effective_mode, workload_policy,
		   replica_index, recovery_attempt, recovered_from)
		VALUES ($1,$2,NULL,$3,$4,$5,'pending',
		        $6::jsonb,$7,$8,'',-1,
		        $9,$10,$11,$12,$13,$14)`,
		runtimeID, action.TargetNode, action.ModelID, containerName, cfg.Backend,
		gpuDevicesJSON, bindHost, port,
		cfg.ExecutionMode, effectiveMode, cfg.WorkloadPolicy,
		replicaIdx, attempt, nullableID(action.RecoveredFrom),
	)
	if dbErr != nil {
		_, _ = r.db.ExecContext(ctx, `SELECT release_node_port($1::uuid, $2)`, action.TargetNode, port)
		r.recordLog(ctx, logID, action, runtimeID, "failed", "insert agent_runtime: "+dbErr.Error())
		return "", 0, fmt.Errorf("insert runtime row: %w", dbErr)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_, _ = r.db.ExecContext(ctx, `SELECT release_node_port($1::uuid, $2)`, action.TargetNode, port)
		return "", 0, fmt.Errorf("insert runtime row: 0 rows affected")
	}

	// Commit while the advisory lock is still held — the lock releases on commit,
	// making the new row visible to other transactions atomically.
	if commitErr := tx.Commit(); commitErr != nil {
		_, _ = r.db.ExecContext(ctx, `SELECT release_node_port($1::uuid, $2)`, action.TargetNode, port)
		r.recordLog(ctx, logID, action, runtimeID, "failed", "commit tx: "+commitErr.Error())
		return "", 0, fmt.Errorf("commit runtime row: %w", commitErr)
	}
	committed = true

	// Link the port lease to the runtime row
	_, _ = r.db.ExecContext(ctx, `
		UPDATE node_port_leases SET runtime_id = $1
		WHERE node_id = $2 AND port = $3 AND released_at IS NULL`,
		runtimeID, action.TargetNode, port,
	)

	// ── 6. Build fully-populated StartModelPayload ────────────────────────────
	modelsVolume := cfg.ModelsVolume
	if modelsVolume == "" {
		modelsVolume = "nexus_models"
	}
	ctxSize := cfg.CtxSize
	if ctxSize == 0 {
		ctxSize = 4096
	}

	payloadEnv := make(map[string]string, len(cfg.Env))
	for k, v := range cfg.Env {
		payloadEnv[k] = v
	}
	if cfg.HFToken != "" {
		payloadEnv["HUGGING_FACE_HUB_TOKEN"] = cfg.HFToken
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
		GPUDevices:     cfg.GPUDevices,
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
		Env:            payloadEnv,
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
		return "", 0, fmt.Errorf("enqueue START_MODEL: %w", taskErr)
	}

	loggedAction := action
	loggedAction.ReplicaIdx = replicaIdx
	r.recordLog(ctx, logID, loggedAction, runtimeID, "success", action.Reason)

	r.log.Info("HA recovery initiated",
		zap.String("model", action.ModelName),
		zap.String("node", action.TargetNode),
		zap.String("runtime_id", runtimeID),
		zap.String("container", containerName),
		zap.String("task_id", taskID),
		zap.Int("port", port),
		zap.Int("replica", replicaIdx),
		zap.Int("attempt", attempt),
		zap.String("recovered_from", action.RecoveredFrom),
	)
	return runtimeID, attempt, nil
}

// nullableID converts an empty string to a nil driver value so an empty
// RecoveredFrom is stored as SQL NULL in recovered_from (a UUID column)
// instead of failing to parse "" as a uuid.
func nullableID(id string) interface{} {
	if id == "" {
		return nil
	}
	return id
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
	// Exclude lazy_load models — they are managed by the IdleManager and the
	// proxy cold-start activator. The HA reconciler only manages always_on
	// workloads. Recovering a lazy_load model after idle eviction would cause
	// the container to be restarted immediately after every timeout, defeating
	// the purpose of idle eviction.
	err := r.db.SelectContext(ctx, &rows, `
		SELECT rrs.model_id, rrs.model_name, rrs.desired_replicas, rrs.min_available,
		       rrs.placement_policy, rrs.auto_recover,
		       rrs.max_surge, rrs.health_retry_interval_s, rrs.replacement_start_timeout_s,
		       rrs.drain_timeout_s, rrs.termination_grace_s,
		       rrs.active_replicas, rrs.starting_replicas, rrs.idle_replicas,
		       rrs.unhealthy_replicas, rrs.draining_replicas,
		       rrs.lost_replicas, rrs.node_count, rrs.ha_status
		FROM runtime_replica_status rrs
		LEFT JOIN model_runtime_configs mrc ON mrc.model_id = rrs.model_id
		WHERE rrs.desired_replicas > 0
		  AND COALESCE(mrc.workload_policy, 'lazy_load') = 'always_on'`)
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

// ─────────────────────────────────────────────────────────────────────────────
// Rolling replacement sweep
//
// This is the Kubernetes-style self-healing loop. It runs every reconcile cycle
// and drives the following state machine for each unhealthy/draining replica:
//
//	READY ──(3 consecutive health fails)──► UNHEALTHY
//	  │
//	  └──► (replacement already started? wait)
//	       (no replacement? spawn new replica via execute())
//
//	UNHEALTHY ──(replacement reaches READY)──► DRAINING
//	  │
//	  └──(replacement timed out)──► FAILED (retry on next sweep)
//
//	DRAINING ──(active_conns==0 OR drain_timeout expired)──► STOP
//
// Key invariant (large-model protection):
//   Never terminate the last READY replica of a model until a replacement
//   replica reaches READY state. Models > 30GB can take several minutes to
//   load; the old container keeps serving during that entire period.
// ─────────────────────────────────────────────────────────────────────────────

func (r *Reconciler) rollingReplacementSweep(ctx context.Context) {
	r.stepUnhealthyReplicas(ctx)
	r.stepDrainingReplicas(ctx)
}

// stepUnhealthyReplicas handles the UNHEALTHY → spawn replacement → DRAINING leg.
func (r *Reconciler) stepUnhealthyReplicas(ctx context.Context) {
	type unhealthyRow struct {
		ID                  string     `db:"id"`
		ModelID             string     `db:"model_id"`
		ModelName           string     `db:"model_name"`
		NodeID              string     `db:"node_id"`
		ReplacedBy          *string    `db:"replaced_by"`
		UpdatedAt           time.Time  `db:"updated_at"`
		DesiredReplicas     int        `db:"desired_replicas"`
		MaxSurge            int        `db:"max_surge"`
		ReplacementTimeoutS int        `db:"replacement_start_timeout_s"`
		ActiveReadyCount    int        `db:"active_ready_count"`
		NextRetryAt         *time.Time `db:"next_retry_at"`
	}

	var rows []unhealthyRow
	_ = r.db.SelectContext(ctx, &rows, `
		SELECT
		    ar.id,
		    ar.model_id,
		    m.name                                            AS model_name,
		    ar.node_id::text                                  AS node_id,
		    ar.replaced_by::text                             AS replaced_by,
		    ar.updated_at,
		    COALESCE(rs.desired_replicas, 1)                 AS desired_replicas,
		    COALESCE(rs.max_surge, 1)                        AS max_surge,
		    COALESCE(rs.replacement_start_timeout_s, 900)    AS replacement_start_timeout_s,
		    (SELECT COUNT(*) FROM agent_runtimes ar2
		     WHERE ar2.model_id = ar.model_id
		       AND ar2.state IN ('ready','active','warm','idle')
		       AND ar2.id != ar.id)                          AS active_ready_count,
		    ar.next_retry_at                                 AS next_retry_at
		FROM agent_runtimes ar
		JOIN models m ON m.id = ar.model_id
		LEFT JOIN model_replica_specs rs ON rs.model_id = ar.model_id
		LEFT JOIN model_runtime_configs mrc ON mrc.model_id = ar.model_id
		WHERE ar.state = 'unhealthy'
		  AND `+modelguard.SQLCondition+`
		  -- lazy_load models are managed by the cold-start activator, which
		  -- correctly respects model_endpoints.node_id pinning. This
		  -- reconciler path does unconstrained free-placement (selectNode has
		  -- no concept of "this model's files only exist on one node") — for
		  -- a node-local GGUF deployment that picks the wrong node and fails
		  -- instantly. loadReplicaStatuses (the sibling under-replication
		  -- path) already excludes lazy_load for the same reason (see its own
		  -- comment); this path never had the matching exclusion.
		  AND COALESCE(mrc.workload_policy,'lazy_load') = 'always_on'`)

	for _, row := range rows {
		r.handleUnhealthyReplica(ctx, row.ID, row.ModelID, row.ModelName,
			row.NodeID, row.ReplacedBy, row.UpdatedAt, row.NextRetryAt,
			row.DesiredReplicas, row.MaxSurge, row.ReplacementTimeoutS,
			row.ActiveReadyCount)
	}
}

// maxUnhealthyRecoveryAttempts bounds how many replacement attempts a single
// logical recovery chain (rooted at the first replica that went unhealthy,
// linked forward via recovered_from — see nextRecoveryAttempt) may accumulate
// before it is marked terminal ('lost') and no longer eligible for automatic
// replacement. This bound previously lived on a per-ROW recovery_attempt
// counter that was never actually carried into replacement rows — every new
// row was inserted with a hardcoded recovery_attempt=1 and recovered_from was
// never populated, so nothing could ever reach this limit no matter how many
// replacements were spawned (forensic audit, Case File 003 round 6 confirmed
// the earlier version of this bug; Case File 004 confirmed it was still
// present because the fix only incremented the OLD row's own counter, which
// is a different physical row every time replaced_by is cleared and
// re-spawned — production data showed a single logical replica accumulate
// ~639 replacement attempts over ~35 hours with recovery_attempt never
// climbing above 1). The fix (migration 060 / nextRecoveryAttempt) computes
// the attempt number by reading the PREDECESSOR row via recovered_from, so it
// is mathematically guaranteed that for any row: recovery_attempt =
// (chain length up to and including this row) <= maxUnhealthyRecoveryAttempts,
// enforced at the single shared insertion point (executeReturningID) rather
// than independently by each caller.
const maxUnhealthyRecoveryAttempts = 5

// unhealthyRecoveryCooldown returns the minimum wait before another
// replacement attempt may be spawned for a chain about to reach the given
// attempt number, growing exponentially so a persistently-failing cause
// doesn't spawn a new row every 30s tick.
func unhealthyRecoveryCooldown(attempt int) time.Duration {
	const base = 30 * time.Second
	const maxCooldown = 900 * time.Second
	d := base * time.Duration(uint(1)<<uint(attempt))
	if d > maxCooldown {
		return maxCooldown
	}
	return d
}

func (r *Reconciler) handleUnhealthyReplica(
	ctx context.Context,
	runtimeID, modelID, modelName, nodeID string,
	replacedBy *string,
	unhealthySince time.Time,
	nextRetryAt *time.Time,
	desiredReplicas, maxSurge, replacementTimeoutS int,
	activeReadyCount int,
) {
	replacementTimeout := time.Duration(replacementTimeoutS) * time.Second

	// ── Case 1: replacement already started — check its progress ─────────
	if replacedBy != nil && *replacedBy != "" {
		var replacementState string
		var replacementAttempt int
		err := r.db.QueryRowContext(ctx,
			`SELECT state, COALESCE(recovery_attempt, 1) FROM agent_runtimes WHERE id = $1`, *replacedBy,
		).Scan(&replacementState, &replacementAttempt)
		if err != nil {
			// Replacement row gone — clear the pointer and retry immediately
			// (this is an anomaly, not a normal failure, so no cooldown).
			_, _ = r.db.ExecContext(ctx,
				`UPDATE agent_runtimes SET replaced_by = NULL, updated_at = NOW() WHERE id = $1`, runtimeID)
			return
		}

		switch replacementState {
		case "ready", "active", "warm", "idle":
			// Replacement is READY. Transition the old runtime to DRAINING.
			// Gateway routing will exclude it immediately because 'draining'
			// is not in IsAvailable(). In-flight requests finish naturally.
			_, _ = r.db.ExecContext(ctx, `
				UPDATE agent_runtimes
				SET state = 'draining', updated_at = NOW()
				WHERE id = $1 AND state = 'unhealthy'`, runtimeID)
			r.log.Info("rolling replacement: replacement READY → old replica now DRAINING",
				zap.String("model", modelName),
				zap.String("old_runtime", runtimeID),
				zap.String("new_runtime", *replacedBy),
			)

		case "failed", "stopped", "deleted":
			// Replacement failed. Clear the pointer so we try again next
			// sweep, but persist next_retry_at on the ORIGINAL row so the
			// cooldown survives — it must NOT be re-derived from updated_at,
			// which this very statement is about to touch (production bug:
			// unhealthySince was read from updated_at, and every Case 1/2
			// transition bumped updated_at, so the cooldown clock silently
			// reset on every tick and never actually grew).
			cooldown := unhealthyRecoveryCooldown(replacementAttempt + 1)
			retryAt := time.Now().Add(cooldown)
			_, _ = r.db.ExecContext(ctx,
				`UPDATE agent_runtimes SET replaced_by = NULL, next_retry_at = $2, updated_at = NOW() WHERE id = $1`,
				runtimeID, retryAt)
			r.log.Warn("rolling replacement: replacement FAILED — will retry after cooldown",
				zap.String("model", modelName),
				zap.String("old_runtime", runtimeID),
				zap.String("failed_replacement", *replacedBy),
				zap.Duration("cooldown", cooldown),
				zap.Time("next_retry_at", retryAt),
			)

		default:
			// Replacement still starting. Check for overall timeout.
			if time.Since(unhealthySince) > replacementTimeout {
				r.log.Warn("rolling replacement: replacement timed out",
					zap.String("model", modelName),
					zap.String("old_runtime", runtimeID),
					zap.String("replacement", *replacedBy),
					zap.Duration("waited", time.Since(unhealthySince)),
				)
				// Mark replacement failed; clear pointer + set cooldown; retry next sweep.
				cooldown := unhealthyRecoveryCooldown(replacementAttempt + 1)
				retryAt := time.Now().Add(cooldown)
				_, _ = r.db.ExecContext(ctx,
					`UPDATE agent_runtimes SET state='failed',
					    error_msg='rolling replacement: startup timeout exceeded',
					    updated_at=NOW() WHERE id=$1`, *replacedBy)
				_, _ = r.db.ExecContext(ctx,
					`UPDATE agent_runtimes SET replaced_by=NULL, next_retry_at=$2, updated_at=NOW() WHERE id=$1`,
					runtimeID, retryAt)
			}
			// Still in progress — wait.
		}
		return
	}

	// ── Case 2: no replacement started yet — spawn one ────────────────────
	//
	// Cooldown: for every attempt AFTER the first, next_retry_at is persisted
	// explicitly (set above, in Case 1's failure branches) rather than
	// derived from updated_at/unhealthySince, so routine bookkeeping updates
	// on this row cannot silently reset it (the exact production bug: every
	// Case 1/2 transition touched updated_at, so a cooldown computed from it
	// never actually elapsed as intended).
	//
	// The very FIRST attempt has no next_retry_at yet (nothing has set it —
	// this row has never been through Case 1), so it falls back to
	// unhealthySince (the row's own updated_at, set by whatever marked it
	// unhealthy) as a one-time proxy — this preserves the original guarantee
	// that a replica isn't replaced the instant it flaps unhealthy.
	if nextRetryAt != nil {
		if time.Now().Before(*nextRetryAt) {
			return
		}
	} else if time.Since(unhealthySince) < unhealthyRecoveryCooldown(0) {
		return
	}

	// Large-model protection: if this is the LAST ready replica, do not
	// terminate it prematurely. Spawn the replacement, but leave the
	// unhealthy replica alive until the replacement is READY.
	// (This is the core zero-downtime guarantee.)
	if activeReadyCount == 0 {
		r.log.Info("rolling replacement: last ready replica is unhealthy — spawning replacement before draining",
			zap.String("model", modelName),
			zap.String("runtime_id", runtimeID),
		)
	}

	// Load status needed for selectNode and execute.
	status := ReplicaStatus{
		ModelID:         modelID,
		ModelName:       modelName,
		DesiredReplicas: desiredReplicas,
		MaxSurge:        maxSurge,
		PlacementPolicy: r.loadPlacementPolicy(ctx, modelID),
		AutoRecover:     true,
	}

	targetNode, err := r.selectNode(ctx, status)
	if err != nil {
		r.log.Warn("rolling replacement: no suitable node for replacement",
			zap.String("model", modelName),
			zap.Error(err),
		)
		return
	}

	action := ReconcileAction{
		ModelID:       modelID,
		ModelName:     modelName,
		Action:        "start_replica",
		TargetNode:    targetNode,
		ReplicaIdx:    r.nextReplicaIndex(ctx, modelID), // display-label estimate only — see nextReplicaIndexTx for the authoritative value
		RecoveredFrom: runtimeID,
		Reason:        fmt.Sprintf("rolling_replacement: old=%s unhealthy_since=%s", runtimeID, unhealthySince.Format(time.RFC3339)),
	}

	newRuntimeID, attemptNum, err := r.executeReturningID(ctx, status, action)
	if err != nil {
		if errors.Is(err, ErrRecoveryChainExhausted) {
			// Already marked 'lost' and logged inside executeReturningID —
			// nothing more to do here.
			return
		}
		r.log.Warn("rolling replacement: failed to start replacement",
			zap.String("model", modelName),
			zap.Error(err),
		)
		return
	}
	if newRuntimeID == "" {
		// A concurrent caller already claimed the slot for this model — not
		// an error, just nothing for this call to link.
		return
	}

	// Link the old runtime to its replacement. recovery_attempt bookkeeping
	// for the CHAIN lives on the new row (via recovered_from), but this hub
	// row's OWN recovery_attempt must be kept in sync with attemptNum too:
	// runtimeID stays 'unhealthy' and is re-evaluated on every future sweep
	// (it is never itself superseded/replaced-away — it coordinates the
	// whole chain), so the NEXT call to nextRecoveryAttempt(ctx, runtimeID)
	// must see the current chain depth, not a stale value. Without this, the
	// chain could never accumulate past attempt 1 no matter how many
	// replacements were spawned — exactly the production bug this fix closes.
	_, _ = r.db.ExecContext(ctx,
		`UPDATE agent_runtimes SET replaced_by = $1, recovery_attempt = $3, updated_at = NOW() WHERE id = $2`,
		newRuntimeID, runtimeID, attemptNum)

	r.log.Info("rolling replacement: replacement spawned",
		zap.String("model", modelName),
		zap.String("old_runtime", runtimeID),
		zap.String("new_runtime", newRuntimeID),
		zap.String("node", targetNode),
	)
}

// stepDrainingReplicas handles the DRAINING → STOPPED leg.
func (r *Reconciler) stepDrainingReplicas(ctx context.Context) {
	type drainingRow struct {
		ID            string    `db:"id"`
		ModelID       string    `db:"model_id"`
		ModelName     string    `db:"model_name"`
		NodeID        string    `db:"node_id"`
		ContainerID   string    `db:"container_id"`
		ActiveConns   int64     `db:"active_conns"`
		DrainStartAt  time.Time `db:"drain_start_at"`
		DrainTimeoutS int       `db:"drain_timeout_s"`
		TermGraceS    int       `db:"termination_grace_s"`
	}

	var rows []drainingRow
	// active_conns is tracked in-process in the pool's Endpoint.ActiveConns;
	// the DB doesn't have it. We use updated_at as proxy for "drain started at".
	_ = r.db.SelectContext(ctx, &rows, `
		SELECT
		    ar.id,
		    ar.model_id,
		    m.name                                      AS model_name,
		    ar.node_id::text                            AS node_id,
		    COALESCE(ar.container_id, '')               AS container_id,
		    0::bigint                                   AS active_conns,
		    ar.updated_at                               AS drain_start_at,
		    COALESCE(rs.drain_timeout_s, 30)            AS drain_timeout_s,
		    COALESCE(rs.termination_grace_s, 15)        AS termination_grace_s
		FROM agent_runtimes ar
		JOIN models m ON m.id = ar.model_id
		LEFT JOIN model_replica_specs rs ON rs.model_id = ar.model_id
		WHERE ar.state = 'draining'
		  AND m.enabled = TRUE`)

	for _, row := range rows {
		drainTimeout := time.Duration(row.DrainTimeoutS) * time.Second
		timeInDrain := time.Since(row.DrainStartAt)

		// Stop the draining runtime when:
		//   a) The drain timeout has elapsed (force stop).
		//   b) ActiveConns is 0 AND at least 2 seconds have passed
		//      (small buffer to avoid racing a request that just arrived).
		shouldStop := timeInDrain >= drainTimeout || row.ActiveConns == 0 && timeInDrain >= 2*time.Second

		if !shouldStop {
			continue
		}

		r.log.Info("rolling replacement: drain complete — stopping old runtime",
			zap.String("model", row.ModelName),
			zap.String("runtime_id", row.ID),
			zap.Duration("in_drain", timeInDrain),
			zap.Int64("active_conns", row.ActiveConns),
		)

		r.stopDrainedRuntime(ctx, row.ID, row.ModelID, row.NodeID, row.ContainerID, row.TermGraceS)
	}
}

// stopDrainedRuntime transitions a draining runtime to 'stopping' and
// dispatches an UNLOAD_RUNTIME task.
func (r *Reconciler) stopDrainedRuntime(ctx context.Context, runtimeID, modelID, nodeID, containerID string, termGraceS int) {
	// ── Diagnostic: capture last_used_at before we change state so the log
	// entry shows exactly how long the runtime was idle at the point of unload.
	var lastUsedAt *time.Time
	_ = r.db.GetContext(ctx, &lastUsedAt,
		`SELECT last_used_at FROM agent_runtimes WHERE id = $1`, runtimeID)
	idleFor := time.Duration(0)
	lastUsedStr := "<nil>"
	if lastUsedAt != nil {
		idleFor = time.Since(*lastUsedAt)
		lastUsedStr = lastUsedAt.Format(time.RFC3339)
	}

	r.log.Info("UNLOAD_RUNTIME will be enqueued",
		zap.String("caller", "ha.Reconciler.stopDrainedRuntime"),
		zap.String("runtime_id", runtimeID),
		zap.String("model_id", modelID),
		zap.String("node_id", nodeID),
		zap.String("reason", "rolling_replacement_drain_complete"),
		zap.String("last_used_at", lastUsedStr),
		zap.Duration("idle_for", idleFor),
		zap.String("configured_timeout", "n/a — HA drain path, not idle eviction"),
	)

	_, _ = r.db.ExecContext(ctx, `
		UPDATE agent_runtimes
		SET state = 'stopping', updated_at = NOW()
		WHERE id = $1 AND state = 'draining'`, runtimeID)

	payload := taskmanager.StopRuntimePayload{
		RuntimeID:   runtimeID,
		ContainerID: containerID,
		DrainSecs:   termGraceS,
	}
	_, err := r.taskMgr.Enqueue(ctx, nodeID,
		taskmanager.TaskUnloadRuntime, payload,
		taskmanager.WithPriority(70),
		taskmanager.WithActor("ha-reconciler-drain"),
		taskmanager.WithRuntimeID(runtimeID),
		taskmanager.WithIdempotencyKey(fmt.Sprintf("drain-stop:%s", runtimeID)),
	)
	if err != nil {
		r.log.Warn("rolling replacement: failed to enqueue UNLOAD_RUNTIME for drained replica",
			zap.String("runtime_id", runtimeID),
			zap.Error(err),
		)
		// Rollback to draining so we retry next sweep.
		_, _ = r.db.ExecContext(ctx, `
			UPDATE agent_runtimes SET state = 'draining', updated_at = NOW()
			WHERE id = $1 AND state = 'stopping'`, runtimeID)
		return
	}

	r.log.Info("UNLOAD_RUNTIME enqueued",
		zap.String("caller", "ha.Reconciler.stopDrainedRuntime"),
		zap.String("runtime_id", runtimeID),
		zap.String("model_id", modelID),
		zap.String("node_id", nodeID),
		zap.String("reason", "rolling_replacement_drain_complete"),
		zap.String("last_used_at", lastUsedStr),
		zap.Duration("idle_for", idleFor),
	)
}

// loadPlacementPolicy fetches the model's placement policy from DB.
func (r *Reconciler) loadPlacementPolicy(ctx context.Context, modelID string) string {
	var policy string
	_ = r.db.QueryRowContext(ctx,
		`SELECT COALESCE(placement_policy, 'spread') FROM model_replica_specs WHERE model_id = $1`,
		modelID,
	).Scan(&policy)
	if policy == "" {
		return "spread"
	}
	return policy
}

// sweepFailedContainers moves failed agent_runtimes to 'stopped'.
// Two paths:
//  1. Container confirmed dead by the node agent watcher ([container-dead] tag):
//     moved immediately — no grace period needed, container is already gone.
//  2. All other failed rows older than 5 minutes:
//     moved after grace period so the reconciler doesn't double-spawn while
//     the container is still exiting.
func (r *Reconciler) sweepFailedContainers(ctx context.Context) {
	// Path 1: confirmed dead — move immediately, no grace period.
	res1, _ := r.db.ExecContext(ctx, `
		UPDATE agent_runtimes
		SET state      = 'stopped',
		    updated_at = NOW()
		WHERE state = 'failed'
		  AND error_msg LIKE '%[container-dead]%'`)
	if n, _ := res1.RowsAffected(); n > 0 {
		r.log.Info("HA sweep: confirmed-dead containers moved to stopped immediately",
			zap.Int64("count", n),
		)
	}

	// Path 2: other failed rows — 5-minute grace period.
	res2, _ := r.db.ExecContext(ctx, `
		UPDATE agent_runtimes
		SET state      = 'stopped',
		    error_msg  = COALESCE(error_msg, '') || ' [ha-sweep: moved failed→stopped after grace period]',
		    updated_at = NOW()
		WHERE state = 'failed'
		  AND error_msg NOT LIKE '%[container-dead]%'
		  AND updated_at < NOW() - INTERVAL '5 minutes'`)
	if n, _ := res2.RowsAffected(); n > 0 {
		r.log.Info("HA sweep: failed runtimes moved to stopped after grace period",
			zap.Int64("count", n),
		)
	}
}
