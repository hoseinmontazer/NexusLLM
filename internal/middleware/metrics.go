package middleware

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// All Prometheus metrics use stable identifiers (team_id, project_id) as
// label dimensions. Mutable display strings such as TeamName must never
// appear as label values — cardinality aside, they change over time and
// break dashboard queries.
var (
	httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "gateway",
		Name:      "requests_total",
		Help:      "Total number of HTTP requests by team_id, project_id, model, and status.",
	}, []string{"team_id", "project_id", "model", "status"})

	httpRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "nexus",
		Subsystem: "gateway",
		Name:      "request_duration_seconds",
		Help:      "End-to-end HTTP request latency.",
		Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
	}, []string{"team_id", "project_id", "model"})

	tokensInputTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "gateway",
		Name:      "tokens_input_total",
		Help:      "Total input tokens forwarded to backend.",
	}, []string{"team_id", "project_id", "model"})

	tokensOutputTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "gateway",
		Name:      "tokens_output_total",
		Help:      "Total output tokens received from backend.",
	}, []string{"team_id", "project_id", "model"})

	ActiveRequests = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "nexus",
		Subsystem: "gateway",
		Name:      "active_requests",
		Help:      "Currently in-flight requests.",
	}, []string{"team_id", "project_id", "model"})

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
	}, []string{"team_id", "project_id", "model"})

	rejectedRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "gateway",
		Name:      "rejected_requests_total",
		Help:      "Requests rejected by the policy engine.",
	}, []string{"team_id", "project_id", "reason"})

	// Thinking / reasoning mode metrics
	ThinkingRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "gateway",
		Name:      "thinking_requests_total",
		Help:      "Total chat requests that used reasoning/thinking mode.",
	}, []string{"team_id", "project_id", "model", "mode"}) // mode: "thinking" | "fast"

	ThinkingTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "gateway",
		Name:      "thinking_tokens_total",
		Help:      "Total tokens consumed by internal reasoning chains.",
	}, []string{"team_id", "project_id", "model"})

	VisibleCompletionTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "gateway",
		Name:      "visible_completion_tokens_total",
		Help:      "Total visible (non-thinking) completion tokens.",
	}, []string{"team_id", "project_id", "model"})
)

// MetricsMiddleware records per-request Prometheus metrics.
// Uses stable identifiers (team_id, project_id) — never mutable display names.
func MetricsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		claims := GetClaims(c)
		teamID := "unknown"
		projectID := ""
		if claims != nil {
			teamID = claims.TeamID
			projectID = claims.ProjectID
		}

		model := c.GetString("model")
		if model == "" {
			model = "unknown"
		}

		status := strconv.Itoa(c.Writer.Status())
		duration := time.Since(start).Seconds()

		httpRequestsTotal.WithLabelValues(teamID, projectID, model, status).Inc()
		httpRequestDuration.WithLabelValues(teamID, projectID, model).Observe(duration)
	}
}

// RecordTokens records token counters after a completed inference request.
// teamID and projectID must be stable identifiers, never display names.
func RecordTokens(teamID, projectID, model string, inputTokens, outputTokens int) {
	tokensInputTotal.WithLabelValues(teamID, projectID, model).Add(float64(inputTokens))
	tokensOutputTotal.WithLabelValues(teamID, projectID, model).Add(float64(outputTokens))
}

// RecordRejection increments the rejected requests counter.
// teamID and projectID must be stable identifiers.
func RecordRejection(teamID, projectID, reason string) {
	rejectedRequestsTotal.WithLabelValues(teamID, projectID, reason).Inc()
}

// ObserveTTFT records the time-to-first-token for a streaming request.
func ObserveTTFT(teamID, projectID, model string, d time.Duration) {
	timeToFirstToken.WithLabelValues(teamID, projectID, model).Observe(d.Seconds())
}
