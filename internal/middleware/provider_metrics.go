package middleware

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Provider-specific Prometheus metrics.
//
// These metrics are orthogonal to the existing gateway metrics — they add a
// "provider" label dimension so operators can observe and alert on per-provider
// behaviour independently of project or team dimensions.
//
// Label conventions:
//   provider  — BackendType string (e.g. "openai_provider", "anthropic_provider")
//   model     — NexusLLM model name (stable identifier, NOT display name)
//   status    — "success" | "error" | "timeout" | "rate_limited"
var (
	// ProviderRequestsTotal counts every request forwarded to a provider.
	ProviderRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "provider",
		Name:      "requests_total",
		Help:      "Total requests forwarded to each external provider.",
	}, []string{"provider", "model", "status"})

	// ProviderLatencySeconds measures end-to-end provider round-trip latency.
	ProviderLatencySeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "nexus",
		Subsystem: "provider",
		Name:      "latency_seconds",
		Help:      "End-to-end latency for external provider requests.",
		Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
	}, []string{"provider", "model"})

	// ProviderCostTotal accumulates USD cost charged by providers.
	ProviderCostTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "provider",
		Name:      "cost_total",
		Help:      "Cumulative estimated cost in USD for external provider requests.",
	}, []string{"provider", "model"})

	// ProviderFailuresTotal counts provider errors by type.
	ProviderFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "provider",
		Name:      "failures_total",
		Help:      "Total failed requests to external providers, by failure type.",
	}, []string{"provider", "model", "failure_type"})
	// failure_type values: "timeout" | "rate_limited" | "auth_error" |
	//                      "server_error" | "network_error" | "invalid_response"

	// ProviderTokensTotal counts tokens billed by providers.
	ProviderTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "provider",
		Name:      "tokens_total",
		Help:      "Total tokens processed by external providers.",
	}, []string{"provider", "model", "token_type"})
	// token_type values: "input" | "output" | "cached" | "reasoning"

	// ProviderRetryTotal counts automatic retries on provider errors.
	ProviderRetryTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "provider",
		Name:      "retry_total",
		Help:      "Total automatic retries issued to external providers.",
	}, []string{"provider", "model"})

	// ProviderTimeoutTotal counts requests that timed out waiting for a provider.
	ProviderTimeoutTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "provider",
		Name:      "timeout_total",
		Help:      "Total requests that timed out waiting for an external provider.",
	}, []string{"provider", "model"})

	// ProviderProxyErrorsTotal counts failures connecting through the outbound proxy.
	// These are distinct from provider-side errors — they indicate a proxy
	// configuration or network connectivity problem rather than a provider API error.
	ProviderProxyErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "provider",
		Name:      "proxy_errors_total",
		Help:      "Total proxy connection errors when reaching external providers.",
	}, []string{"provider", "model"})

	// ProviderConnectionFailuresTotal counts TCP / TLS / DNS failures (non-proxy).
	ProviderConnectionFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus",
		Subsystem: "provider",
		Name:      "connection_failures_total",
		Help:      "Total TCP/TLS/DNS connection failures reaching external providers.",
	}, []string{"provider", "model"})

	// ProviderHealthStatus is a gauge: 1 = healthy, 0 = down.
	// Updated by the runtime watcher on every health-check tick.
	ProviderHealthStatus = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "nexus",
		Subsystem: "provider",
		Name:      "health_status",
		Help:      "Current health status of external provider endpoint (1=healthy, 0=down).",
	}, []string{"provider", "model"})

	// ProviderTTFTSeconds measures time-to-first-token for streaming provider responses.
	ProviderTTFTSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "nexus",
		Subsystem: "provider",
		Name:      "ttft_seconds",
		Help:      "Time to first streaming token from external provider.",
		Buckets:   []float64{0.1, 0.25, 0.5, 1, 2, 5, 10},
	}, []string{"provider", "model"})
)

// RecordProviderRequest records the outcome of a single provider request.
// Call this from the proxy handler after each provider round-trip completes.
//
//	provider — BackendType string (e.g. "openai_provider")
//	model    — NexusLLM model name
//	status   — "success" | "error" | "timeout" | "rate_limited"
//	latency  — seconds elapsed for the full round-trip
//	costUSD  — estimated cost for this request (from model_cost_config)
func RecordProviderRequest(provider, model, status string, latencySeconds, costUSD float64) {
	ProviderRequestsTotal.WithLabelValues(provider, model, status).Inc()
	ProviderLatencySeconds.WithLabelValues(provider, model).Observe(latencySeconds)
	if costUSD > 0 {
		ProviderCostTotal.WithLabelValues(provider, model).Add(costUSD)
	}
}

// RecordProviderTokens records token counts for a completed provider request.
func RecordProviderTokens(provider, model string, input, output, cached, reasoning int) {
	if input > 0 {
		ProviderTokensTotal.WithLabelValues(provider, model, "input").Add(float64(input))
	}
	if output > 0 {
		ProviderTokensTotal.WithLabelValues(provider, model, "output").Add(float64(output))
	}
	if cached > 0 {
		ProviderTokensTotal.WithLabelValues(provider, model, "cached").Add(float64(cached))
	}
	if reasoning > 0 {
		ProviderTokensTotal.WithLabelValues(provider, model, "reasoning").Add(float64(reasoning))
	}
}

// RecordProviderFailure records a provider-side failure.
// failureType: "timeout" | "rate_limited" | "auth_error" | "server_error" |
//
//	"network_error" | "invalid_response"
func RecordProviderFailure(provider, model, failureType string) {
	ProviderFailuresTotal.WithLabelValues(provider, model, failureType).Inc()
}

// RecordProviderConnectionError classifies err using ClassifyProviderError and
// increments the appropriate counter:
//   - "proxy_error"         → ProviderProxyErrorsTotal
//   - "connection_failure"  → ProviderConnectionFailuresTotal
//   - "timeout"             → ProviderTimeoutTotal
//   - anything else         → ProviderFailuresTotal with the classified label
//
// Call this from the proxy handler when backend.Chat/Embeddings returns an error.
func RecordProviderConnectionError(provider, model string, err error) {
	if err == nil {
		return
	}
	label := classifyProviderErrorLabel(err)
	switch label {
	case "proxy_error":
		ProviderProxyErrorsTotal.WithLabelValues(provider, model).Inc()
		ProviderFailuresTotal.WithLabelValues(provider, model, "proxy_error").Inc()
	case "connection_failure":
		ProviderConnectionFailuresTotal.WithLabelValues(provider, model).Inc()
		ProviderFailuresTotal.WithLabelValues(provider, model, "connection_failure").Inc()
	case "timeout":
		ProviderTimeoutTotal.WithLabelValues(provider, model).Inc()
		ProviderFailuresTotal.WithLabelValues(provider, model, "timeout").Inc()
	default:
		ProviderFailuresTotal.WithLabelValues(provider, model, label).Inc()
	}
}

// classifyProviderErrorLabel maps an error to a short Prometheus label.
// Mirrors runtime.ClassifyProviderError without importing the runtime package
// from middleware (which would create an import cycle).
func classifyProviderErrorLabel(err error) string {
	msg := err.Error()
	switch {
	case containsStr(msg, "proxyconnect"), containsStr(msg, "CONNECT"):
		return "proxy_error"
	case containsStr(msg, "connection refused"), containsStr(msg, "no such host"),
		containsStr(msg, "dial "):
		return "connection_failure"
	case containsStr(msg, "i/o timeout"), containsStr(msg, "context deadline exceeded"),
		containsStr(msg, "context canceled"):
		return "timeout"
	case containsStr(msg, "tls "), containsStr(msg, "x509"), containsStr(msg, "certificate"):
		return "tls_error"
	default:
		return "request_error"
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}()
}

// RecordProviderHealth updates the health gauge for a provider model endpoint.
// Called by the watcher on every health-check tick.
func RecordProviderHealth(provider, model string, healthy bool) {
	v := 0.0
	if healthy {
		v = 1.0
	}
	ProviderHealthStatus.WithLabelValues(provider, model).Set(v)
}
