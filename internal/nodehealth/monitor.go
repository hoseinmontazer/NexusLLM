// Package nodehealth implements the heartbeat-based node health monitor.
// It runs as a background goroutine inside nexus-admin and transitions
// node status based on time since last heartbeat:
//
//   < 30s since heartbeat  → ONLINE
//   30–90s since heartbeat → UNHEALTHY (missed 1–3 heartbeats)
//   90s–5min               → UNHEALTHY (will become OFFLINE soon)
//   > 5 minutes            → OFFLINE
//
// When a node goes OFFLINE:
//   - All runtimes on that node become LOST
//   - model_endpoints linked to that node have health_status = 'down'
//   - The gateway will stop routing to those endpoints on the next watcher tick
package nodehealth

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
)

const (
	// CheckInterval is how often the monitor runs its sweep.
	CheckInterval = 30 * time.Second

	// UnhealthyThreshold: no heartbeat for this long → UNHEALTHY.
	UnhealthyThreshold = 90 * time.Second

	// OfflineThreshold: no heartbeat for this long → OFFLINE.
	OfflineThreshold = 5 * time.Minute
)

// Monitor watches node heartbeats and transitions node states.
type Monitor struct {
	db  *sqlx.DB
	log *zap.Logger
}

// NewMonitor constructs a node health Monitor.
func NewMonitor(db *sqlx.DB, log *zap.Logger) *Monitor {
	return &Monitor{db: db, log: log}
}

// Start begins the health monitoring loop. Blocks until ctx is cancelled.
func (m *Monitor) Start(ctx context.Context) {
	m.log.Info("node health monitor started",
		zap.Duration("unhealthy_threshold", UnhealthyThreshold),
		zap.Duration("offline_threshold", OfflineThreshold),
	)

	// Run immediately on start
	m.sweep(ctx)

	ticker := time.NewTicker(CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.log.Info("node health monitor stopped")
			return
		case <-ticker.C:
			m.sweep(ctx)
		}
	}
}

// sweep checks every online/unhealthy node and transitions state if needed.
func (m *Monitor) sweep(ctx context.Context) {
	now := time.Now()

	type nodeRow struct {
		ID              string     `db:"id"`
		Hostname        string     `db:"hostname"`
		Status          string     `db:"status"`
		LastHeartbeatAt *time.Time `db:"last_heartbeat_at"`
	}

	var nodes []nodeRow
	if err := m.db.SelectContext(ctx, &nodes, `
		SELECT id, hostname, status, last_heartbeat_at
		FROM nodes
		WHERE status NOT IN ('offline','draining','maintenance')
		  AND status != 'unknown'
		  OR (status = 'unknown' AND last_heartbeat_at IS NOT NULL)`); err != nil {
		m.log.Warn("node health sweep: query failed", zap.Error(err))
		return
	}

	for _, node := range nodes {
		if node.LastHeartbeatAt == nil {
			continue // never sent a heartbeat — don't auto-transition
		}

		age := now.Sub(*node.LastHeartbeatAt)
		var targetStatus string

		switch {
		case age > OfflineThreshold:
			targetStatus = "offline"
		case age > UnhealthyThreshold:
			targetStatus = "unhealthy"
		default:
			if node.Status == "unhealthy" {
				// Recovered — heartbeat is fresh again
				targetStatus = "online"
			}
		}

		if targetStatus == "" || targetStatus == node.Status {
			continue
		}

		m.transition(ctx, node.ID, node.Hostname, node.Status, targetStatus, age)
	}
}

// transition updates a node's status and takes appropriate action.
func (m *Monitor) transition(ctx context.Context, nodeID, hostname, from, to string, heartbeatAge time.Duration) {
	_, err := m.db.ExecContext(ctx, `
		UPDATE nodes SET status = $1, updated_at = NOW() WHERE id = $2`,
		to, nodeID)
	if err != nil {
		m.log.Error("node status transition failed",
			zap.String("node", hostname),
			zap.String("from", from),
			zap.String("to", to),
			zap.Error(err),
		)
		return
	}

	// Record the transition
	_, _ = m.db.ExecContext(ctx, `
		INSERT INTO node_health_events (node_id, from_status, to_status, reason)
		VALUES ($1, $2, $3, $4)`,
		nodeID, from, to,
		"heartbeat age: "+heartbeatAge.Round(time.Second).String(),
	)

	m.log.Info("node status changed",
		zap.String("node", hostname),
		zap.String("from", from),
		zap.String("to", to),
		zap.Duration("heartbeat_age", heartbeatAge),
	)

	// When a node goes OFFLINE: mark all its runtimes as LOST
	// and take endpoints out of routing
	if to == "offline" {
		m.handleNodeOffline(ctx, nodeID, hostname)
	}
}

// handleNodeOffline marks all runtimes on the node as LOST and removes
// their endpoints from gateway routing.
func (m *Monitor) handleNodeOffline(ctx context.Context, nodeID, hostname string) {
	// 1. Mark all active runtimes as LOST
	res, _ := m.db.ExecContext(ctx, `
		UPDATE agent_runtimes
		SET state = 'lost', updated_at = NOW()
		WHERE node_id = $1
		  AND state NOT IN ('stopped','unloaded','deleted','archived','lost')`,
		nodeID)
	if n, _ := res.RowsAffected(); n > 0 {
		m.log.Info("runtimes marked LOST",
			zap.String("node", hostname),
			zap.Int64("count", n),
		)
	}

	// 2. Mark all model_endpoints on this node as DOWN
	// Gateway watcher reads health_status — setting it to 'down' removes
	// the endpoint from routing on the next watcher tick (within 5s).
	res2, _ := m.db.ExecContext(ctx, `
		UPDATE model_endpoints
		SET health_status = 'down',
		    lifecycle_state = 'failed',
		    updated_at = NOW()
		WHERE node_id = $1
		  AND health_status NOT IN ('down','draining')`,
		nodeID)
	if n, _ := res2.RowsAffected(); n > 0 {
		m.log.Info("endpoints taken offline",
			zap.String("node", hostname),
			zap.Int64("count", n),
		)
	}
}

// MarkDraining puts a node into DRAINING state (no new deploys, finish existing).
func (m *Monitor) MarkDraining(ctx context.Context, nodeID string) error {
	_, err := m.db.ExecContext(ctx, `
		UPDATE nodes SET status='draining', updated_at=NOW() WHERE id=$1`, nodeID)
	if err == nil {
		_, _ = m.db.ExecContext(ctx, `
			INSERT INTO node_health_events (node_id, from_status, to_status, reason)
			SELECT id, status, 'draining', 'admin requested drain'
			FROM nodes WHERE id=$1`, nodeID)
	}
	return err
}
