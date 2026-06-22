package runtimemgr

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
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
// The model files remain on the /models volume — a subsequent request will
// trigger Activator.EnsureRunning() which uses WARM_RUNTIME (docker start)
// to bring it back without re-downloading.
type IdleManager struct {
	db      *sqlx.DB
	taskMgr *taskmanager.Manager
	cfg     Config
	log     *zap.Logger
}

// NewIdleManager constructs an IdleManager.
func NewIdleManager(db *sqlx.DB, taskMgr *taskmanager.Manager, cfg Config, log *zap.Logger) *IdleManager {
	return &IdleManager{db: db, taskMgr: taskMgr, cfg: cfg, log: log}
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
		}
	}
}

type idleRow struct {
	RuntimeID    string     `db:"id"`
	NodeID       string     `db:"node_id"`
	ModelID      string     `db:"model_id"`
	ModelName    string     `db:"model_name"`
	ContainerID  string     `db:"container_id"`
	EndpointID   string     `db:"endpoint_id"`
	LastUsedAt   *time.Time `db:"last_used_at"`
	IdleTimeout  *int       `db:"idle_timeout_secs"`
}

func (m *IdleManager) evict(ctx context.Context) {
	var rows []idleRow
	if err := m.db.SelectContext(ctx, &rows, `
		SELECT ar.id, ar.node_id, ar.model_id,
		       mo.name AS model_name,
		       COALESCE(ar.container_id,'') AS container_id,
		       COALESCE(ar.endpoint_id::text,'') AS endpoint_id,
		       ar.last_used_at,
		       mrc.idle_timeout_secs
		FROM agent_runtimes ar
		JOIN models mo ON mo.id = ar.model_id
		LEFT JOIN model_runtime_configs mrc ON mrc.model_id = ar.model_id
		WHERE ar.state IN ('active','warm','idle')
		  AND ar.last_used_at IS NOT NULL`); err != nil {
		m.log.Warn("idle manager: failed to query runtimes", zap.Error(err))
		return
	}

	for _, row := range rows {
		timeout := m.cfg.DefaultIdleTimeout
		if row.IdleTimeout != nil && *row.IdleTimeout > 0 {
			timeout = time.Duration(*row.IdleTimeout) * time.Second
		}

		if row.LastUsedAt == nil {
			continue
		}
		idleFor := time.Since(*row.LastUsedAt)
		if idleFor < timeout {
			continue
		}

		m.log.Info("idle timeout — stopping container",
			zap.String("model", row.ModelName),
			zap.String("runtime_id", row.RuntimeID),
			zap.Duration("idle_for", idleFor),
			zap.Duration("timeout", timeout),
		)

		m.stopRuntime(ctx, row)
	}
}

func (m *IdleManager) stopRuntime(ctx context.Context, row idleRow) {
	// Mark stopping immediately so we don't double-dispatch.
	_, _ = m.db.ExecContext(ctx, `
		UPDATE agent_runtimes SET state='stopping', updated_at=NOW()
		WHERE id = $1 AND state IN ('active','warm','idle')`, row.RuntimeID)

	// Disable endpoint so gateway stops routing to it right away.
	if row.EndpointID != "" {
		_, _ = m.db.ExecContext(ctx, `
			UPDATE model_endpoints
			SET is_enabled = FALSE, lifecycle_state = 'unloaded',
			    health_status = 'down', updated_at = NOW()
			WHERE id = $1`, row.EndpointID)
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
			UPDATE agent_runtimes SET state='active', updated_at=NOW() WHERE id=$1`, row.RuntimeID)
		return
	}

	m.log.Info("UNLOAD_RUNTIME enqueued",
		zap.String("model", row.ModelName),
		zap.String("task_id", taskID),
	)
}
