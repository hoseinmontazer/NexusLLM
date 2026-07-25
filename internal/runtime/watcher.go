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
	// Covers both deployment paths:
	//   1. HA reconciler replicas (endpoint_id IS NULL)
	//   2. Regular admin-deploy runtimes (endpoint_id IS NOT NULL)
	// Previously this only promoted HA replicas. Regular deployments could get
	// stuck in loading_model permanently if waitForReady timed out or the gateway
	// restarted before the model finished loading.
	if result.Status == StatusHealthy {
		_, _ = w.db.ExecContext(ctx, `
			UPDATE agent_runtimes
			SET state = 'ready', last_used_at = COALESCE(last_used_at, NOW()), updated_at = NOW()
			WHERE id = $1
			  AND state IN ('loading_model','waiting_ready','loading')`,
			ep.ID)

		// Re-enable model_endpoints if this runtime is now healthy.
		// ep.ID may be either a model_endpoints.id (legacy path) or an
		// agent_runtimes.id (agent-managed path). Handle both:
		//   • Direct match:   me.id = ep.ID  (legacy model_endpoints row)
		//   • Via runtime:    me.id = ar.endpoint_id WHERE ar.id = ep.ID
		_, _ = w.db.ExecContext(ctx, `
			UPDATE model_endpoints me
			SET is_enabled     = TRUE,
			    lifecycle_state = 'active',
			    health_status   = 'healthy',
			    port            = CASE WHEN ar.bind_port > 0 THEN ar.bind_port ELSE me.port END,
			    host            = CASE WHEN ar.bind_host != '' THEN ar.bind_host ELSE me.host END,
			    updated_at      = NOW()
			FROM (
			    -- ep.ID is an agent_runtimes.id — resolve to its endpoint
			    SELECT ar.bind_port, ar.bind_host, ar.endpoint_id AS me_id
			    FROM agent_runtimes ar
			    WHERE ar.id = $1 AND ar.bind_port > 0 AND ar.endpoint_id IS NOT NULL
			    UNION ALL
			    -- ep.ID is a model_endpoints.id directly (legacy / no runtime row)
			    SELECT 0 AS bind_port, '' AS bind_host, me2.id AS me_id
			    FROM model_endpoints me2
			    WHERE me2.id = $1
			      AND NOT EXISTS (
			          SELECT 1 FROM agent_runtimes ar2
			          WHERE ar2.id = $1
			      )
			) ar
			WHERE me.id = ar.me_id`,
			ep.ID)
	}

	// ── Immediately disable and remove DOWN endpoints from routing ─────────
	// When an endpoint is definitively down (circuit breaker fired), mark the
	// runtime as 'unhealthy' so the reconciler can start a rolling replacement
	// instead of immediately stopping the container.
	//
	// Exception: do NOT disable endpoints whose agent_runtime is still in a
	// loading state (loading_model, waiting_ready). Connection refused during
	// model loading is expected — the server starts accepting connections only
	// after the model weights are fully loaded into VRAM. Disabling here would
	// prevent the endpoint from ever becoming routable.
	if result.Status == StatusDown {
		// Resolve ep.ID to both the agent_runtimes.id and model_endpoints.id.
		// ep.ID may be either depending on which registry path loaded this endpoint.
		var runtimeID, endpointMEID string
		_ = w.db.QueryRowContext(ctx, `
			SELECT
			    ar.id::text,
			    COALESCE(ar.endpoint_id::text, '')
			FROM agent_runtimes ar
			WHERE ar.id = $1
			LIMIT 1`, ep.ID).Scan(&runtimeID, &endpointMEID)
		if runtimeID == "" {
			// ep.ID is a model_endpoints.id (legacy path — no agent_runtime row)
			endpointMEID = ep.ID
		}

		// Check if the runtime is still loading — if so, skip the disable.
		var loadingCount int
		_ = w.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM agent_runtimes
			WHERE id = $1
			  AND state IN ('loading_model','waiting_ready','loading',
			                'pending','validating','downloading','starting')`,
			ep.ID).Scan(&loadingCount)

		if loadingCount == 0 {
			// Runtime is not in a loading state — genuinely down.
			if endpointMEID != "" {
				_, _ = w.db.ExecContext(ctx, `
					UPDATE model_endpoints
					SET health_status = 'down', is_enabled = FALSE, updated_at = NOW()
					WHERE id = $1 AND is_enabled = TRUE`, endpointMEID)
			}

			// Transition runtime to 'unhealthy'.
			if runtimeID != "" {
				_, _ = w.db.ExecContext(ctx, `
					UPDATE agent_runtimes
					SET state     = 'unhealthy',
					    error_msg = 'health check failed 3 consecutive times',
					    updated_at = NOW()
					WHERE id = $1
					  AND state IN ('ready','active','warm','idle','loading_model','waiting_ready')`,
					runtimeID)
			} else if endpointMEID != "" {
				// Legacy path: no runtime row, mark by endpoint_id.
				_, _ = w.db.ExecContext(ctx, `
					UPDATE agent_runtimes
					SET state     = 'unhealthy',
					    error_msg = 'health check failed 3 consecutive times',
					    updated_at = NOW()
					WHERE endpoint_id = $1
					  AND state IN ('ready','active','warm','idle','loading_model','waiting_ready')`,
					endpointMEID)
			}
		}
		// If still loading: update health_status for observability but keep is_enabled=TRUE.
		if loadingCount > 0 && endpointMEID != "" {
			_, _ = w.db.ExecContext(ctx, `
				UPDATE model_endpoints
				SET health_status = 'down', updated_at = NOW()
				WHERE id = $1`, endpointMEID)
		}
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
		// HA agent_runtime replicas (endpoint_id IS NULL) are not in model_endpoints
		// so 0 rows affected is normal for them — don't log at debug to avoid spam.
		_ = n
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
