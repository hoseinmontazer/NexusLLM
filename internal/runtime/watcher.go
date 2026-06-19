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
	// Pick the right backend for this endpoint.
	backend, err := w.registry.backendForEndpoint(ep.ModelID)
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
	// Update the main endpoint row.
	_, _ = w.db.ExecContext(ctx, `
		UPDATE model_endpoints
		SET health_status    = $1,
		    last_checked_at  = $2,
		    response_time_ms = $3,
		    consecutive_failures = CASE WHEN $1 = 'healthy' THEN 0
		                               ELSE consecutive_failures + 1 END,
		    last_success_at  = CASE WHEN $1 = 'healthy' THEN $2
		                            ELSE last_success_at END,
		    updated_at       = NOW()
		WHERE id = $4`,
		string(h.Status), h.CheckedAt, h.LatencyMs, epID,
	)

	// Append to health log (kept for trending / alerting).
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
