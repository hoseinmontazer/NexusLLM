// Package project — metrics.go
// Defines and updates the Prometheus gauge for active project runtimes.
//
//   nexus_project_active_runtimes{project_id, project_name, priority}
//
// Updated whenever a runtime transitions into or out of 'active' or 'warm'.
// The Collector runs a periodic refresh loop driven by DB queries.
package project

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

var activeRuntimesGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "nexus_project_active_runtimes",
		Help: "Number of agent_runtimes in state active or warm per project.",
	},
	[]string{"project_id", "project_name", "priority"},
)

func init() {
	prometheus.MustRegister(activeRuntimesGauge)
}

// MetricsCollector periodically refreshes nexus_project_active_runtimes from DB.
type MetricsCollector struct {
	db  *sqlx.DB
	log *zap.Logger
}

// NewMetricsCollector constructs a MetricsCollector.
func NewMetricsCollector(db *sqlx.DB, log *zap.Logger) *MetricsCollector {
	return &MetricsCollector{db: db, log: log}
}

// Start runs the refresh loop. Blocks until ctx is cancelled.
func (c *MetricsCollector) Start(ctx context.Context) {
	c.refresh(ctx)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refresh(ctx)
		}
	}
}

func (c *MetricsCollector) refresh(ctx context.Context) {
	type row struct {
		ProjectID   string  `db:"project_id"`
		ProjectName string  `db:"project_name"`
		Priority    string  `db:"priority"`
		Count       float64 `db:"cnt"`
	}
	var rows []row
	if err := c.db.SelectContext(ctx, &rows, `
		SELECT p.id::text AS project_id, p.name AS project_name, p.priority,
		       COUNT(ar.id)::float AS cnt
		FROM projects p
		LEFT JOIN agent_runtimes ar
		       ON ar.project_id = p.id AND ar.state IN ('active','warm')
		WHERE p.status = 'active'
		GROUP BY p.id, p.name, p.priority`); err != nil {
		c.log.Warn("project metrics refresh failed", zap.Error(err))
		return
	}

	// Reset and re-set all label combinations
	activeRuntimesGauge.Reset()
	for _, r := range rows {
		activeRuntimesGauge.WithLabelValues(r.ProjectID, r.ProjectName, r.Priority).Set(r.Count)
	}
}
