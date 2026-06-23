// Package preemption implements the Preemption Engine.
//
// The engine is responsible for:
//  1. Detecting resource pressure on nodes (GPU util >95%, VRAM below reserved floor, RAM <5% free).
//  2. Selecting candidate runtimes for eviction under resource pressure, in strict
//     priority order (BEST_EFFORT first, CRITICAL last).
//  3. Executing preemption: stop runtime → release resources → deploy higher-priority runtime.
//  4. Recording every evaluation and execution in the preemption_events table.
//
// Polling interval: ≤30 seconds (consistent with nodehealth.CheckInterval).
package preemption

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/project"
	"github.com/nexusllm/nexusllm/internal/taskmanager"
	"go.uber.org/zap"
)

// Engine evaluates resource pressure and performs preemption.
type Engine struct {
	db      *sqlx.DB
	taskMgr *taskmanager.Manager
	log     *zap.Logger
}

// NewEngine constructs a preemption Engine.
func NewEngine(db *sqlx.DB, taskMgr *taskmanager.Manager, log *zap.Logger) *Engine {
	return &Engine{db: db, taskMgr: taskMgr, log: log}
}

// Start runs the pressure detection loop. Blocks until ctx is cancelled.
// Polls every 30 seconds, consistent with nodehealth.CheckInterval.
func (e *Engine) Start(ctx context.Context) {
	e.log.Info("preemption engine started", zap.Duration("interval", 30*time.Second))
	// Run immediately on start
	e.sweep(ctx)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			e.log.Info("preemption engine stopped")
			return
		case <-ticker.C:
			e.sweep(ctx)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Pressure detection sweep
// ─────────────────────────────────────────────────────────────────────────────

type nodeRow struct {
	ID       string `db:"id"`
	Hostname string `db:"hostname"`
}

func (e *Engine) sweep(ctx context.Context) {
	var nodes []nodeRow
	if err := e.db.SelectContext(ctx, &nodes, `
		SELECT id, hostname FROM nodes WHERE status IN ('online','degraded')`); err != nil {
		e.log.Warn("preemption sweep: query nodes failed", zap.Error(err))
		return
	}
	for _, n := range nodes {
		e.evaluateNode(ctx, n.ID, n.Hostname)
	}
}

type pressureResult struct {
	trigger       string
	pressureValue float64
	needEvict     bool
}

func (e *Engine) evaluateNode(ctx context.Context, nodeID, hostname string) {
	pr := e.detectPressure(ctx, nodeID)
	if !pr.needEvict {
		return
	}

	e.log.Info("resource pressure detected",
		zap.String("node", hostname),
		zap.String("trigger", pr.trigger),
		zap.Float64("value", pr.pressureValue),
	)

	// Record the pressure evaluation event
	e.recordEvaluation(ctx, nodeID, pr)

	// Select the lowest-priority non-protected candidate to evict
	candidate := e.selectEvictionCandidate(ctx, nodeID)
	if candidate == nil {
		e.log.Info("no eviction candidates found", zap.String("node", hostname))
		return
	}

	e.log.Info("preempting runtime",
		zap.String("node", hostname),
		zap.String("runtime_id", candidate.RuntimeID),
		zap.String("priority", candidate.Priority),
	)

	e.executePreemption(ctx, candidate, nil, pr.trigger)
}

func (e *Engine) detectPressure(ctx context.Context, nodeID string) pressureResult {
	// ── GPU utilisation check ────────────────────────────────────────────────
	var maxUtil int
	_ = e.db.GetContext(ctx, &maxUtil, `
		SELECT COALESCE(MAX(gt.utilization_pct), 0)
		FROM gpu_devices d
		JOIN gpu_nodes gn ON gn.id = d.node_id
		LEFT JOIN LATERAL (
		    SELECT utilization_pct FROM gpu_telemetry
		    WHERE device_id = d.id ORDER BY recorded_at DESC LIMIT 1
		) gt ON TRUE
		WHERE gn.node_id = $1`, nodeID)

	if maxUtil > project.GPUPressureThresholdPct {
		return pressureResult{
			trigger:       project.TriggerGPUUtilization,
			pressureValue: float64(maxUtil),
			needEvict:     true,
		}
	}

	// ── VRAM reservation check ────────────────────────────────────────────────
	// Total free VRAM vs sum of CRITICAL+HIGH reservations for runtimes on this node
	var freeVRAM int64
	_ = e.db.GetContext(ctx, &freeVRAM, `
		SELECT COALESCE(SUM(d.vram_mb - COALESCE(gt.memory_used_mb, 0)), 0)
		FROM gpu_devices d
		JOIN gpu_nodes gn ON gn.id = d.node_id
		LEFT JOIN LATERAL (
		    SELECT memory_used_mb FROM gpu_telemetry
		    WHERE device_id = d.id ORDER BY recorded_at DESC LIMIT 1
		) gt ON TRUE
		WHERE gn.node_id = $1`, nodeID)

	var reservedVRAM int64
	_ = e.db.GetContext(ctx, &reservedVRAM, `
		SELECT COALESCE(SUM(pr.reserved_vram_mb), 0)
		FROM project_reservations pr
		JOIN projects p ON p.id = pr.project_id
		JOIN agent_runtimes ar ON ar.project_id = p.id AND ar.node_id = $1
		WHERE p.priority IN ('CRITICAL','HIGH')
		  AND ar.state IN ('ready','active','warm','idle','loading_model')`, nodeID)

	if freeVRAM < reservedVRAM {
		return pressureResult{
			trigger:       project.TriggerVRAMExhaustion,
			pressureValue: float64(freeVRAM),
			needEvict:     true,
		}
	}

	// ── RAM pressure check ─────────────────────────────────────────────────────
	var ramRow struct {
		Total int64 `db:"ram_total_mb"`
		Used  int64 `db:"ram_used_mb"`
	}
	err := e.db.GetContext(ctx, &ramRow, `
		SELECT ram_total_mb, ram_used_mb FROM node_telemetry
		WHERE node_id = $1 ORDER BY recorded_at DESC LIMIT 1`, nodeID)
	if err == nil && ramRow.Total > 0 {
		freeRAM := ramRow.Total - ramRow.Used
		thresholdMB := ramRow.Total * project.RAMPressureFreeThresholdPct / 100
		if freeRAM < thresholdMB {
			return pressureResult{
				trigger:       project.TriggerMemExhaustion,
				pressureValue: float64(freeRAM),
				needEvict:     true,
			}
		}
	}

	return pressureResult{needEvict: false}
}

// ─────────────────────────────────────────────────────────────────────────────
// Candidate selection
// ─────────────────────────────────────────────────────────────────────────────

type candidateRuntime struct {
	RuntimeID   string
	ContainerID string
	NodeID      string
	ProjectID   *string
	Priority    string
	LastUsedAt  *time.Time
	EndpointID  string
}

// selectEvictionCandidate picks the lowest-priority non-protected runtime
// on the node. Within the same tier, picks the least-recently-used runtime.
// NULL last_used_at is treated as oldest.
func (e *Engine) selectEvictionCandidate(ctx context.Context, nodeID string) *candidateRuntime {
	// Ordered by priority score ASC (lowest priority first), then least recently used.
	// Excludes protected runtimes (always_running=TRUE or protected=TRUE).
	type row struct {
		RuntimeID   string     `db:"id"`
		ContainerID string     `db:"container_id"`
		NodeID      string     `db:"node_id"`
		ProjectID   *string    `db:"project_id"`
		Priority    string     `db:"priority"`
		LastUsedAt  *time.Time `db:"last_used_at"`
		EndpointID  string     `db:"endpoint_id"`
	}
	var candidates []row
	if err := e.db.SelectContext(ctx, &candidates, `
		SELECT ar.id, COALESCE(ar.container_id,'') AS container_id,
		       ar.node_id, ar.project_id,
		       COALESCE(p.priority,'NORMAL') AS priority,
		       ar.last_used_at,
		       COALESCE(ar.endpoint_id::text,'') AS endpoint_id
		FROM agent_runtimes ar
		LEFT JOIN projects p ON p.id = ar.project_id
		LEFT JOIN project_configurations pc ON pc.project_id = p.id
		WHERE ar.node_id = $1
		  AND ar.state IN ('ready','active','warm','idle')
		  -- Exclude protected runtimes
		  AND COALESCE(pc.always_running, FALSE) = FALSE
		  AND COALESCE(pc.protected, FALSE) = FALSE
		ORDER BY project_priority_score(COALESCE(p.priority,'NORMAL')) ASC,
		         COALESCE(ar.last_used_at, '1970-01-01'::timestamptz) ASC
		LIMIT 5`, nodeID); err != nil {
		e.log.Warn("selectEvictionCandidate query failed", zap.Error(err), zap.String("node_id", nodeID))
		return nil
	}
	if len(candidates) == 0 {
		return nil
	}
	c := candidates[0]
	return &candidateRuntime{
		RuntimeID:   c.RuntimeID,
		ContainerID: c.ContainerID,
		NodeID:      c.NodeID,
		ProjectID:   c.ProjectID,
		Priority:    c.Priority,
		LastUsedAt:  c.LastUsedAt,
		EndpointID:  c.EndpointID,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// PreemptForProject executes preemption on behalf of a higher-priority project.
// Called by the scheduler when a CRITICAL/HIGH deployment cannot be placed.
// Returns true if preemption succeeded and resources were freed.
// ─────────────────────────────────────────────────────────────────────────────
func (e *Engine) PreemptForProject(
	ctx context.Context,
	nodeID string,
	requestingRuntimeID string,
	requestingProjectID string,
	requestingPriority project.Priority,
) (bool, error) {
	candidate := e.selectEvictionCandidate(ctx, nodeID)
	if candidate == nil {
		return false, fmt.Errorf("no eviction candidates available on node %s", nodeID)
	}

	// Verify the preemption rule: requestor must have strictly higher priority
	candidatePriority := project.Priority(candidate.Priority)
	if !requestingPriority.CanPreempt(candidatePriority) {
		return false, fmt.Errorf("priority %s cannot preempt %s (same or lower tier)",
			requestingPriority, candidatePriority)
	}

	// CRITICAL runtimes can never be preempted
	if candidatePriority == project.PriorityCritical {
		return false, fmt.Errorf("cannot preempt CRITICAL runtime %s", candidate.RuntimeID)
	}

	err := e.executePreemption(ctx, candidate, &requestingRuntimeID, project.TriggerAdmission)
	return err == nil, err
}

// ─────────────────────────────────────────────────────────────────────────────
// Preemption execution
// ─────────────────────────────────────────────────────────────────────────────

func (e *Engine) executePreemption(
	ctx context.Context,
	candidate *candidateRuntime,
	requestingRuntimeID *string,
	trigger string,
) error {
	const stopTimeout = 60 * time.Second

	// Step 1: set state = 'stopping'
	_, err := e.db.ExecContext(ctx, `
		UPDATE agent_runtimes SET state='stopping', updated_at=NOW()
		WHERE id=$1 AND state IN ('active','warm','idle')`, candidate.RuntimeID)
	if err != nil {
		return fmt.Errorf("mark stopping: %w", err)
	}

	// Step 2: disable the associated endpoint
	if candidate.EndpointID != "" {
		_, _ = e.db.ExecContext(ctx, `
			UPDATE model_endpoints
			SET is_enabled=FALSE, lifecycle_state='draining', health_status='draining', updated_at=NOW()
			WHERE id=$1`, candidate.EndpointID)
	}

	// Step 3: dispatch STOP_RUNTIME task
	payload := taskmanager.StopRuntimePayload{
		RuntimeID:   candidate.RuntimeID,
		ContainerID: candidate.ContainerID,
		DrainSecs:   10,
	}
	taskID, err := e.taskMgr.Enqueue(ctx, candidate.NodeID,
		taskmanager.TaskStopRuntime, payload,
		taskmanager.WithPriority(95), // preemption is high-urgency
		taskmanager.WithActor("preemption-engine"),
		taskmanager.WithRuntimeID(candidate.RuntimeID),
		taskmanager.WithTimeout(stopTimeout+10*time.Second),
		taskmanager.WithIdempotencyKey(fmt.Sprintf("preempt:%s", candidate.RuntimeID)),
	)
	if err != nil {
		e.log.Error("preemption: enqueue STOP_RUNTIME failed",
			zap.String("runtime_id", candidate.RuntimeID),
			zap.Error(err),
		)
		// Rollback state
		_, _ = e.db.ExecContext(ctx, `
			UPDATE agent_runtimes SET state='active', updated_at=NOW() WHERE id=$1`, candidate.RuntimeID)
		return fmt.Errorf("enqueue STOP_RUNTIME: %w", err)
	}

	e.log.Info("preemption STOP_RUNTIME dispatched",
		zap.String("runtime_id", candidate.RuntimeID),
		zap.String("task_id", taskID),
	)

	// Step 4: wait for the task to reach success or failed (with timeout)
	waitCtx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	if waitErr := e.waitForTask(waitCtx, taskID); waitErr != nil {
		e.log.Warn("preemption STOP_RUNTIME did not complete successfully",
			zap.String("runtime_id", candidate.RuntimeID),
			zap.Error(waitErr),
		)
		e.recordPreemptionEvent(ctx, candidate, requestingRuntimeID, trigger)
		return fmt.Errorf("STOP_RUNTIME failed/timeout for runtime %s: %w", candidate.RuntimeID, waitErr)
	}

	// Step 5: record the completed preemption
	e.recordPreemptionEvent(ctx, candidate, requestingRuntimeID, trigger)

	e.log.Info("preemption succeeded",
		zap.String("runtime_id", candidate.RuntimeID),
		zap.String("priority", candidate.Priority),
	)
	return nil
}

// waitForTask polls agent_tasks until success or failed/timeout.
func (e *Engine) waitForTask(ctx context.Context, taskID string) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for task %s: %w", taskID, ctx.Err())
		case <-ticker.C:
			task, err := e.taskMgr.GetTask(ctx, taskID)
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

// ─────────────────────────────────────────────────────────────────────────────
// Audit recording
// ─────────────────────────────────────────────────────────────────────────────

func (e *Engine) recordEvaluation(ctx context.Context, nodeID string, pr pressureResult) {
	pv := pr.pressureValue
	_, _ = e.db.ExecContext(ctx, `
		INSERT INTO preemption_events
		  (id, node_id, trigger, pressure_value, created_at)
		VALUES ($1,$2,$3,$4,NOW())`,
		uuid.New().String(), nodeID, pr.trigger, pv,
	)
}

func (e *Engine) recordPreemptionEvent(
	ctx context.Context,
	candidate *candidateRuntime,
	requestingRuntimeID *string,
	trigger string,
) {
	_, _ = e.db.ExecContext(ctx, `
		INSERT INTO preemption_events
		  (id, node_id, preempted_runtime_id, preempted_project_id, preempted_priority,
		   requesting_runtime_id, requesting_project_id, trigger, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,NULL,$7,NOW())`,
		uuid.New().String(),
		candidate.NodeID,
		candidate.RuntimeID,
		candidate.ProjectID,
		candidate.Priority,
		requestingRuntimeID,
		trigger,
	)
}
