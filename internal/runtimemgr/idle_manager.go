package runtimemgr

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/modelguard"
	"github.com/nexusllm/nexusllm/internal/taskmanager"
	"go.uber.org/zap"
)

// IdleManager scans running agent_runtimes every EvictCheckInterval and stops
// containers that have been idle longer than their configured IdleTimeout.
//
// When a container is stopped:
//   - agent_runtimes.state → "stopped"
//   - model_endpoints.is_enabled → FALSE, lifecycle_state → "unloaded"
//   - The registry drops the endpoint on its next Reload tick
//
// The model files remain on the /models volume.  A subsequent request triggers
// Activator.EnsureRunning() → StartModel(), which recreates the container
// through the full unified startup pipeline.
type IdleManager struct {
	db        *sqlx.DB
	taskMgr   *taskmanager.Manager
	activator *RuntimeActivator // used by restoreAlwaysRunning to re-start models
	cfg       Config
	log       *zap.Logger
}

// NewIdleManager constructs an IdleManager.
func NewIdleManager(db *sqlx.DB, taskMgr *taskmanager.Manager, cfg Config, log *zap.Logger) *IdleManager {
	return &IdleManager{db: db, taskMgr: taskMgr, cfg: cfg, log: log}
}

// WithActivator attaches the RuntimeActivator so restoreAlwaysRunning can
// call StartModel() through the unified pipeline.
func (m *IdleManager) WithActivator(a *RuntimeActivator) *IdleManager {
	m.activator = a
	return m
}

// Start runs the idle eviction loop. Blocks until ctx is cancelled.
func (m *IdleManager) Start(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.EvictCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.evict(ctx)
			m.restoreAlwaysRunning(ctx)
		}
	}
}

type idleRow struct {
	RuntimeID      string     `db:"id"`
	NodeID         string     `db:"node_id"`
	ModelID        string     `db:"model_id"`
	ModelName      string     `db:"model_name"`
	ContainerID    string     `db:"container_id"`
	EndpointID     string     `db:"endpoint_id"`
	RuntimeState   string     `db:"state"`
	LastUsedAt     *time.Time `db:"last_used_at"`
	IdleTimeout    *int       `db:"idle_timeout_secs"`
	ProjectID      *string    `db:"project_id"`
	AlwaysRunning  bool       `db:"always_running"`
	Protected      bool       `db:"protected"`
	MinReplicas    int        `db:"minimum_replicas"`
	WorkloadPolicy string     `db:"workload_policy"`
	DeploymentMode string     `db:"deployment_mode"`
}

func (m *IdleManager) evict(ctx context.Context) {
	var rows []idleRow
	if err := m.db.SelectContext(ctx, &rows, `
		SELECT ar.id, ar.node_id, ar.model_id,
		       mo.name AS model_name,
		       COALESCE(ar.container_id,'') AS container_id,
		       COALESCE(ar.endpoint_id::text,'') AS endpoint_id,
		       ar.state,
		       ar.last_used_at,
		       mrc.idle_timeout_secs,
		       ar.project_id,
		       COALESCE(pc.always_running, FALSE)  AS always_running,
		       COALESCE(pc.protected, FALSE)        AS protected,
		       COALESCE(pc.minimum_replicas, 0)     AS minimum_replicas,
		       COALESCE(ar.workload_policy, 'lazy_load') AS workload_policy,
		       COALESCE(mo.deployment_mode, 'managed')   AS deployment_mode
		FROM agent_runtimes ar
		JOIN models mo ON mo.id = ar.model_id
		LEFT JOIN model_runtime_configs mrc ON mrc.model_id = ar.model_id
		LEFT JOIN project_configurations pc ON pc.project_id = ar.project_id
		WHERE ar.state IN ('ready','active','warm','idle','loading_model','loading')
		  AND ar.last_used_at IS NOT NULL`); err != nil {
		m.log.Warn("idle manager: failed to query runtimes", zap.Error(err))
		return
	}

	for _, row := range rows {
		// ── Diagnostic: log every candidate so the caller of UNLOAD_RUNTIME
		// can be pinpointed exactly.  Logged at debug so it doesn't spam
		// production; raise to info temporarily when debugging.
		configuredTimeout := m.cfg.DefaultIdleTimeout
		if row.IdleTimeout != nil && *row.IdleTimeout > 0 {
			configuredTimeout = time.Duration(*row.IdleTimeout) * time.Second
		}
		idleForDiag := time.Duration(0)
		if row.LastUsedAt != nil {
			idleForDiag = time.Since(*row.LastUsedAt)
		}
		timeoutSource := "default"
		if row.IdleTimeout != nil && *row.IdleTimeout > 0 {
			timeoutSource = "model_runtime_configs"
		}
		m.log.Debug("idle manager: evaluating runtime",
			zap.String("caller", "IdleManager.evict"),
			zap.String("runtime_id", row.RuntimeID),
			zap.String("model_id", row.ModelID),
			zap.String("model_name", row.ModelName),
			zap.String("state", row.RuntimeState),
			zap.String("workload_policy", row.WorkloadPolicy),
			zap.Bool("always_running", row.AlwaysRunning),
			zap.Bool("protected", row.Protected),
			zap.String("last_used_at", lastUsedStr(row.LastUsedAt)),
			zap.Duration("idle_for", idleForDiag),
			zap.Duration("configured_timeout", configuredTimeout),
			zap.String("timeout_source", timeoutSource),
			zap.Any("idle_timeout_secs_raw", row.IdleTimeout),
		)

		// Deployment ownership: a manually-deployed model's container belongs
		// to the operator. Stopping it would take down something NexusLLM
		// cannot bring back up. (A manual model normally has no
		// agent_runtimes row at all — this guards the case where one was left
		// behind by an earlier managed deployment of the same model.)
		if !modelguard.ManagedByNexus(row.DeploymentMode) {
			continue
		}

		// Workload policy protection: never evict always_on services
		if row.WorkloadPolicy == "always_on" {
			continue
		}

		// Project protection: skip always_running or protected runtimes
		if row.AlwaysRunning || row.Protected {
			continue
		}

		// Project protection: skip if eviction would drop below minimum_replicas
		if row.MinReplicas > 0 && row.ProjectID != nil {
			var activeCount int
			_ = m.db.GetContext(ctx, &activeCount, `
				SELECT COUNT(*) FROM agent_runtimes
				WHERE project_id=$1 AND state IN ('ready','active','warm','idle')`, *row.ProjectID)
			if activeCount <= row.MinReplicas {
				continue
			}
		}

		timeout := m.cfg.DefaultIdleTimeout
		if row.IdleTimeout != nil && *row.IdleTimeout > 0 {
			timeout = time.Duration(*row.IdleTimeout) * time.Second
		}

		if row.LastUsedAt == nil {
			continue
		}

		// Never evict a runtime that is still in startup states (loading_model,
		// loading). These states indicate the container was just launched and the
		// last_used_at value may be stale from a previous runtime row.  The idle
		// clock only starts once the runtime reaches a stable operational state
		// (ready, active, warm, idle).  Use the idle timeout as a grace period:
		// if the row has been in a loading state for less than the configured
		// timeout, skip it unconditionally.
		if row.RuntimeState == "loading_model" || row.RuntimeState == "loading" {
			continue
		}

		idleFor := time.Since(*row.LastUsedAt)
		if idleFor < timeout {
			continue
		}

		// ── Will evict: log at INFO with full context so the trigger is always visible ──
		m.log.Info("idle manager: UNLOAD_RUNTIME will be enqueued",
			zap.String("caller", "IdleManager.evict"),
			zap.String("runtime_id", row.RuntimeID),
			zap.String("model_id", row.ModelID),
			zap.String("model_name", row.ModelName),
			zap.String("state", row.RuntimeState),
			zap.String("workload_policy", row.WorkloadPolicy),
			zap.String("last_used_at", lastUsedStr(row.LastUsedAt)),
			zap.Duration("idle_for", idleFor),
			zap.Duration("configured_timeout", timeout),
			zap.String("timeout_source", timeoutSource),
			zap.String("reason", "idle_timeout_exceeded"),
		)

		m.stopRuntime(ctx, row)
	}
}

// lastUsedStringer is a fmt.Stringer wrapper for *time.Time that prints
// "<nil>" for nil pointers instead of panicking.
func (m *IdleManager) stopRuntime(ctx context.Context, row idleRow) {
	// Mark stopping immediately so we don't double-dispatch.
	_, _ = m.db.ExecContext(ctx, `
		UPDATE agent_runtimes SET state='stopping', updated_at=NOW()
		WHERE id = $1 AND state IN ('ready','active','warm','idle','loading_model','loading')`, row.RuntimeID)

	// Disable endpoint so gateway stops routing to it right away.
	if row.EndpointID != "" {
		_, _ = m.db.ExecContext(ctx, `
			UPDATE model_endpoints
			SET is_enabled = FALSE, lifecycle_state = 'unloaded',
			    health_status = 'down', updated_at = NOW()
			WHERE id = $1`, row.EndpointID)
	}

	idleFor := time.Duration(0)
	if row.LastUsedAt != nil {
		idleFor = time.Since(*row.LastUsedAt)
	}
	configuredTimeout := m.cfg.DefaultIdleTimeout
	if row.IdleTimeout != nil && *row.IdleTimeout > 0 {
		configuredTimeout = time.Duration(*row.IdleTimeout) * time.Second
	}
	timeoutSource := "default"
	if row.IdleTimeout != nil && *row.IdleTimeout > 0 {
		timeoutSource = "model_runtime_configs"
	}

	// Dispatch UNLOAD_RUNTIME task to the node agent.
	payload := taskmanager.StopRuntimePayload{
		RuntimeID:   row.RuntimeID,
		ContainerID: row.ContainerID,
		DrainSecs:   10,
	}
	taskID, err := m.taskMgr.Enqueue(ctx, row.NodeID,
		taskmanager.TaskUnloadRuntime, payload,
		taskmanager.WithPriority(60),
		taskmanager.WithActor("idle-manager"),
		taskmanager.WithRuntimeID(row.RuntimeID),
		taskmanager.WithIdempotencyKey(fmt.Sprintf("unload:%s:%s", row.NodeID, row.RuntimeID)),
	)
	if err != nil {
		m.log.Error("failed to enqueue UNLOAD_RUNTIME",
			zap.String("runtime_id", row.RuntimeID),
			zap.Error(err),
		)
		// Rollback state so we try again next tick.
		_, _ = m.db.ExecContext(ctx, `
			UPDATE agent_runtimes SET state='ready', updated_at=NOW()
			WHERE id=$1 AND state='stopping'`, row.RuntimeID)
		return
	}

	m.log.Info("UNLOAD_RUNTIME enqueued",
		zap.String("caller", "IdleManager.stopRuntime"),
		zap.String("task_id", taskID),
		zap.String("runtime_id", row.RuntimeID),
		zap.String("model_id", row.ModelID),
		zap.String("model_name", row.ModelName),
		zap.String("reason", "idle_timeout_exceeded"),
		zap.String("last_used_at", lastUsedStr(row.LastUsedAt)),
		zap.Duration("idle_for", idleFor),
		zap.Duration("configured_timeout", configuredTimeout),
		zap.String("timeout_source", timeoutSource),
		zap.Any("idle_timeout_secs_raw", row.IdleTimeout),
	)
}

// lastUsedStr formats a nullable time pointer for structured logging.
func lastUsedStr(t *time.Time) string {
	if t == nil {
		return "<nil>"
	}
	return t.Format(time.RFC3339)
}

// restoreAlwaysRunning checks projects with always_running=TRUE and ensures
// their active runtime count meets minimum_replicas.
//
// Uses the unified StartModel() pipeline for ALL restore triggers — whether
// the container is stopped, missing, or was lost due to a node crash.
// This is intentional: a stopped container with a container_id is no longer
// treated as a fast-path "warm restart"; START_MODEL always recreates fresh.
func (m *IdleManager) restoreAlwaysRunning(ctx context.Context) {
	type projRow struct {
		ProjectID   string `db:"project_id"`
		MinReplicas int    `db:"minimum_replicas"`
		ActiveCount int    `db:"active_count"`
	}
	var projs []projRow
	if err := m.db.SelectContext(ctx, &projs, `
		SELECT pc.project_id,
		       pc.minimum_replicas,
		       COUNT(ar.id) FILTER (
		           WHERE ar.state IN ('ready','active','warm','idle')
		       ) AS active_count
		FROM project_configurations pc
		LEFT JOIN agent_runtimes ar ON ar.project_id = pc.project_id
		WHERE pc.always_running = TRUE AND pc.minimum_replicas > 0
		GROUP BY pc.project_id, pc.minimum_replicas
		HAVING COUNT(ar.id) FILTER (
		    WHERE ar.state IN ('ready','active','warm','idle')
		) < pc.minimum_replicas`); err != nil {
		return
	}

	if m.activator == nil {
		m.log.Warn("restoreAlwaysRunning: no activator attached — cannot restore models")
		return
	}

	for _, p := range projs {
		deficit := p.MinReplicas - p.ActiveCount
		if deficit <= 0 {
			continue
		}

		// Find model names for this project that need restoring.
		type modelRow struct {
			ModelName string `db:"model_name"`
		}
		var models []modelRow
		_ = m.db.SelectContext(ctx, &models, `
			SELECT DISTINCT mo.name AS model_name
			FROM agent_runtimes ar
			JOIN models mo ON mo.id = ar.model_id
			WHERE ar.project_id = $1
			  AND ar.state NOT IN ('ready','active','warm','idle','stopping','deleted')
			  -- Never restore a manually-deployed model: NexusLLM does not own
			  -- its container (modelguard.SQLManagedCondition, alias mo).
			  AND COALESCE(mo.deployment_mode,'managed') != 'manual'
			LIMIT $2`, p.ProjectID, deficit)

		for _, mdl := range models {
			if err := m.activator.StartModel(ctx, mdl.ModelName); err != nil {
				m.log.Warn("restoreAlwaysRunning: StartModel failed",
					zap.String("model", mdl.ModelName),
					zap.Error(err),
				)
			} else {
				m.log.Info("restoreAlwaysRunning: START_MODEL enqueued",
					zap.String("model", mdl.ModelName),
				)
			}
		}
	}
}
