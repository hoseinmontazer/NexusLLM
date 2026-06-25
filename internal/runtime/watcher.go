package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
)

// Watcher periodically health-checks every registered endpoint, updates the
// in-process Registry, writes state to PostgreSQL and Redis, and exposes
// Prometheus metrics for observability.
type Watcher struct {
	registry *Registry
	db       *sqlx.DB
	log      *zap.Logger
	interval time.Duration

	// Prometheus metrics
	endpointUp       *prometheus.GaugeVec
	endpointLatency  *prometheus.GaugeVec
	checkTotal       *prometheus.CounterVec
	consecutiveFails *prometheus.GaugeVec
	activeConns      *prometheus.GaugeVec
	gpuCacheUtil     *prometheus.GaugeVec
}

// NewWatcher constructs a Watcher.
func NewWatcher(registry *Registry, db *sqlx.DB, log *zap.Logger, interval time.Duration) *Watcher {
	w := &Watcher{
		registry: registry,
		db:       db,
		log:      log,
		interval: interval,
	}
	w.registerMetrics()
	return w
}

func (w *Watcher) registerMetrics() {
	w.endpointUp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "nexus",
		Subsystem: "runtime",
		Name:      "endpoint_up",
		Help:      "1 if endpoint is healthy, 0 otherwise.",
	}, []string{"model", "endpoint_id", "host"})

	w.endpointLatency = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "nexus",
		Subsystem: "runtime",
		Name:      "endpoint_health_latency_ms",
		Help:      "Last health check round-trip latency in milliseconds.",
	}, []string{"model", "endpoint_id"})

	w.checkTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "runtime",
		Name:      "health_checks_total",
		Help:      "Total number of health checks performed.",
	}, []string{"model", "endpoint_id", "status"})

	w.consecutiveFails = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "nexus",
		Subsystem: "runtime",
		Name:      "endpoint_consecutive_failures",
		Help:      "Number of consecutive health check failures.",
	}, []string{"model", "endpoint_id"})

	w.activeConns = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "nexus",
		Subsystem: "runtime",
		Name:      "endpoint_active_connections",
		Help:      "Current number of active (in-flight) connections to each endpoint.",
	}, []string{"model", "endpoint_id"})

	w.gpuCacheUtil = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "nexus",
		Subsystem: "runtime",
		Name:      "endpoint_gpu_cache_utilization",
		Help:      "GPU KV-cache utilisation reported by vLLM (0–1).",
	}, []string{"model", "endpoint_id"})
}

// Start launches the watch loop. It blocks until ctx is cancelled.
func (w *Watcher) Start(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	w.log.Info("runtime watcher started", zap.Duration("interval", w.interval))
	w.checkAll(ctx) // immediate first pass

	for {
		select {
		case <-ctx.Done():
			w.log.Info("runtime watcher stopped")
			return
		case <-ticker.C:
			w.checkAll(ctx)
		}
	}
}

// checkAll runs health checks for every endpoint in the registry in parallel.
func (w *Watcher) checkAll(ctx context.Context) {
	for _, modelName := range w.registry.ListModels() {
		w.registry.mu.RLock()
		pool, ok := w.registry.pools[modelName]
		w.registry.mu.RUnlock()
		if !ok {
			continue
		}

		for _, ep := range pool.Endpoints() {
			go w.checkOne(ctx, modelName, ep)
		}
	}
}

// checkOne health-checks a single endpoint, applies circuit-breaker logic,
// persists the result, and updates Prometheus metrics.
func (w *Watcher) checkOne(ctx context.Context, modelName string, ep *Endpoint) {
	// Pick the backend that matches THIS endpoint's type (ollama, vllm, tgi, etc.)
	backend, err := w.registry.BackendForEndpoint(ep)
	if err != nil {
		return
	}

	hCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	result := backend.Health(hCtx, ep.URL)
	result.EndpointID = ep.ID

	// ── Circuit breaker ────────────────────────────────────────────────────
	newStatus := result.Status
	if newStatus == StatusDown {
		// Only mark down after 3 consecutive failures to avoid flapping.
		fails := w.incrementFailures(ctx, ep.ID)
		if fails < 3 {
			newStatus = StatusDegraded
		}
	} else {
		w.resetFailures(ctx, ep.ID)
	}
	result.Status = newStatus

	// ── Update registry ────────────────────────────────────────────────────
	w.registry.UpdateEndpointHealth(ctx, ep.ID, modelName, result)

	// ── Persist to PostgreSQL ──────────────────────────────────────────────
	w.persistHealthResult(ctx, ep.ID, result)

	// ── Promote loading_model/waiting_ready → ready on first successful health check ────
	// HA reconciler creates agent_runtimes with endpoint_id=NULL. These enter the
	// routing pool with health_status='unknown' while loading. The first passing
	// health check here transitions them to 'ready' so requests start flowing.
	// This is the mechanism that closes the HA failover loop end-to-end.
	if result.Status == StatusHealthy {
		_, _ = w.db.ExecContext(ctx, `
			UPDATE agent_runtimes
			SET state = 'ready', last_used_at = COALESCE(last_used_at, NOW()), updated_at = NOW()
			WHERE id = $1
			  AND state IN ('loading_model','waiting_ready','loading')
			  AND endpoint_id IS NULL`,
			ep.ID)
	}

	// ── Immediately disable and remove DOWN endpoints from routing ─────────
	// When an endpoint is definitively down (circuit breaker fired), set
	// is_enabled=FALSE in DB so the next registry.Reload() drops it entirely.
	// This is faster than waiting for the node health monitor's 5-min timeout.
	// Also mark the agent_runtime row as 'failed' so the stuck-runtime sweeper
	// and the lazy-load activator know to restart the container.
	if result.Status == StatusDown {
		_, _ = w.db.ExecContext(ctx, `
			UPDATE model_endpoints
			SET health_status = 'down', is_enabled = FALSE, updated_at = NOW()
			WHERE id = $1 AND is_enabled = TRUE`, ep.ID)

		// Mark the agent_runtime row failed so the sweeper/activator restarts it.
		// ep.ID is either a model_endpoints.id (legacy path) or an agent_runtimes.id
		// (HA replica path). Update both tables — the wrong one will match 0 rows.
		_, _ = w.db.ExecContext(ctx, `
			UPDATE agent_runtimes
			SET state     = 'failed',
			    error_msg = 'health check failed 3 consecutive times — container may be gone',
			    updated_at = NOW()
			WHERE (id = $1 OR endpoint_id = $1)
			  AND state IN ('ready','active','warm','idle','loading_model','waiting_ready')`,
			ep.ID)
	}

	// ── Prometheus metrics ─────────────────────────────────────────────────
	upVal := 0.0
	if result.Status == StatusHealthy {
		upVal = 1.0
	}
	labels := prometheus.Labels{"model": modelName, "endpoint_id": ep.ID, "host": ep.URL}
	shortLabels := prometheus.Labels{"model": modelName, "endpoint_id": ep.ID}

	w.endpointUp.With(labels).Set(upVal)
	w.endpointLatency.With(shortLabels).Set(float64(result.LatencyMs))
	w.checkTotal.With(prometheus.Labels{
		"model": modelName, "endpoint_id": ep.ID, "status": string(result.Status),
	}).Inc()
	w.gpuCacheUtil.With(shortLabels).Set(result.GPUCacheUtil)

	w.log.Debug("health check",
		zap.String("model", modelName),
		zap.String("endpoint", ep.URL),
		zap.String("status", string(result.Status)),
		zap.Int("latency_ms", result.LatencyMs),
	)
}

func (w *Watcher) persistHealthResult(ctx context.Context, epID string, h EndpointHealth) {
	isHealthy := h.Status == StatusHealthy

	// Update the main endpoint row.
	// Use separate parameters for the CASE branches to avoid lib/pq type
	// inference issues when the same placeholder appears in both SET and WHERE
	// positions with different type contexts.
	res, err := w.db.ExecContext(ctx, `
		UPDATE model_endpoints
		SET health_status    = $1,
		    last_checked_at  = $2,
		    response_time_ms = $3,
		    consecutive_failures = CASE WHEN $4 THEN 0
		                               ELSE consecutive_failures + 1 END,
		    last_success_at  = CASE WHEN $4 THEN $2
		                            ELSE last_success_at END,
		    updated_at       = NOW()
		WHERE id = $5`,
		string(h.Status), h.CheckedAt, h.LatencyMs, isHealthy, epID,
	)
	if err != nil {
		w.log.Warn("health persist UPDATE failed",
			zap.String("endpoint_id", epID),
			zap.String("status", string(h.Status)),
			zap.Error(err),
		)
	} else if n, _ := res.RowsAffected(); n == 0 {
		w.log.Debug("health persist UPDATE matched 0 rows — endpoint may have been removed",
			zap.String("endpoint_id", epID),
		)
	}

	// Append to health log only for model_endpoints rows (not agent_runtimes HA replicas).
	// endpoint_health_log.endpoint_id has a FK to model_endpoints(id); inserting
	// an agent_runtimes UUID here would violate that constraint.
	var isRealEndpoint bool
	_ = w.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM model_endpoints WHERE id = $1)`, epID,
	).Scan(&isRealEndpoint)
	if isRealEndpoint {
		_, _ = w.db.ExecContext(ctx, `
			INSERT INTO endpoint_health_log (endpoint_id, status, latency_ms, error_msg, checked_at)
			VALUES ($1, $2, $3, $4, $5)`,
			epID, string(h.Status), h.LatencyMs, h.Error, h.CheckedAt,
		)

		// Prune old log entries (keep last 1000 per endpoint).
		_, _ = w.db.ExecContext(ctx, `
			DELETE FROM endpoint_health_log
			WHERE endpoint_id = $1
			  AND id NOT IN (
			      SELECT id FROM endpoint_health_log
			      WHERE endpoint_id = $1
			      ORDER BY checked_at DESC
			      LIMIT 1000
			  )`, epID,
		)
	}
}

func (w *Watcher) incrementFailures(ctx context.Context, epID string) int {
	key := fmt.Sprintf("nexus:ep:%s:failures", epID)
	val, _ := w.registry.rdb.Incr(ctx, key).Result()
	w.registry.rdb.Expire(ctx, key, 10*time.Minute)
	return int(val)
}

func (w *Watcher) resetFailures(ctx context.Context, epID string) {
	key := fmt.Sprintf("nexus:ep:%s:failures", epID)
	w.registry.rdb.Del(ctx, key)
}
