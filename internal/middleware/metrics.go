package middleware

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "gateway",
		Name:      "requests_total",
		Help:      "Total number of HTTP requests by team, model, and status.",
	}, []string{"team", "model", "status"})

	httpRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "nexus",
		Subsystem: "gateway",
		Name:      "request_duration_seconds",
		Help:      "End-to-end HTTP request latency.",
		Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
	}, []string{"team", "model"})

	tokensInputTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "gateway",
		Name:      "tokens_input_total",
		Help:      "Total input tokens forwarded to vLLM.",
	}, []string{"team", "model"})

	tokensOutputTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "gateway",
		Name:      "tokens_output_total",
		Help:      "Total output tokens received from vLLM.",
	}, []string{"team", "model"})

	ActiveRequests = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "nexus",
		Subsystem: "gateway",
		Name:      "active_requests",
		Help:      "Currently in-flight requests.",
	}, []string{"team", "model"})

	QueueDepth = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "nexus",
		Subsystem: "scheduler",
		Name:      "queue_depth",
		Help:      "Current depth of each priority queue.",
	}, []string{"priority"})

	timeToFirstToken = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "nexus",
		Subsystem: "gateway",
		Name:      "time_to_first_token_seconds",
		Help:      "Time from request start to first SSE token received.",
		Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	}, []string{"team", "model"})

	rejectedRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "gateway",
		Name:      "rejected_requests_total",
		Help:      "Requests rejected by the policy engine.",
	}, []string{"team", "reason"})
)

// MetricsMiddleware records per-request Prometheus metrics.
func MetricsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		claims := GetClaims(c)
		team := "unknown"
		if claims != nil {
			team = claims.TeamName
		}

		model := c.GetString("model") // set by proxy handler
		if model == "" {
			model = "unknown"
		}

		status := strconv.Itoa(c.Writer.Status())
		duration := time.Since(start).Seconds()

		httpRequestsTotal.WithLabelValues(team, model, status).Inc()
		httpRequestDuration.WithLabelValues(team, model).Observe(duration)
	}
}

// RecordTokens records token counters after a completed inference request.
func RecordTokens(team, model string, inputTokens, outputTokens int) {
	tokensInputTotal.WithLabelValues(team, model).Add(float64(inputTokens))
	tokensOutputTotal.WithLabelValues(team, model).Add(float64(outputTokens))
}

// RecordRejection increments the rejected requests counter.
func RecordRejection(team, reason string) {
	rejectedRequestsTotal.WithLabelValues(team, reason).Inc()
}

// ObserveTTFT records the time-to-first-token for a streaming request.
func ObserveTTFT(team, model string, d time.Duration) {
	timeToFirstToken.WithLabelValues(team, model).Observe(d.Seconds())
}
