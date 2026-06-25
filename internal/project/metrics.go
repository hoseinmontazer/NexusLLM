// Package project — metrics.go
// Prometheus gauge for active project runtimes, keyed by numeric priority_weight.
//
//	nexus_project_active_runtimes{project_id, project_name, priority_weight, priority_label}
package project

import (
	"context"
	"fmt"
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
	[]string{"project_id", "project_name", "priority_weight", "priority_label"},
)

var effectivePriorityGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "nexus_project_effective_priority",
		Help: "Current effective scheduling priority per project (0–1000).",
	},
	[]string{"project_id", "project_name"},
)

func init() {
	prometheus.MustRegister(activeRuntimesGauge)
	prometheus.MustRegister(effectivePriorityGauge)
}

// MetricsCollector periodically refreshes project metrics from DB.
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
		ProjectID      string  `db:"project_id"`
		ProjectName    string  `db:"project_name"`
		PriorityWeight int     `db:"priority_weight"`
		Count          float64 `db:"cnt"`
		EffPriority    float64 `db:"effective_priority"`
	}
	var rows []row
	if err := c.db.SelectContext(ctx, &rows, `
		SELECT p.id::text AS project_id, p.name AS project_name,
		       p.priority_weight,
		       COUNT(ar.id)::float AS cnt,
		       COALESCE(ep.effective_priority, p.priority_weight)::float AS effective_priority
		FROM projects p
		LEFT JOIN agent_runtimes ar
		       ON ar.project_id = p.id AND ar.state IN ('active','warm')
		LEFT JOIN project_effective_priority ep ON ep.project_id = p.id
		WHERE p.status = 'active'
		GROUP BY p.id, p.name, p.priority_weight, ep.effective_priority`); err != nil {
		c.log.Warn("project metrics refresh failed", zap.Error(err))
		return
	}

	activeRuntimesGauge.Reset()
	effectivePriorityGauge.Reset()
	for _, r := range rows {
		weight := PriorityWeight(r.PriorityWeight)
		label := weight.Label()
		activeRuntimesGauge.WithLabelValues(
			r.ProjectID, r.ProjectName,
			itoa(r.PriorityWeight), label,
		).Set(r.Count)
		effectivePriorityGauge.WithLabelValues(
			r.ProjectID, r.ProjectName,
		).Set(r.EffPriority)
	}
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
