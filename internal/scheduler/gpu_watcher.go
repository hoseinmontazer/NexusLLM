package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

const (
	poolActiveKey   = "nexus:pool:%s:active"
	poolCapKey      = "nexus:pool:%s:at_capacity"
	poolMetricsKey  = "nexus:pool:%s:metrics"
	capacityTTL     = 30 * time.Second
	maxActiveRatio  = 0.90 // mark at-capacity when 90% utilised
)

// vLLMMetrics is a minimal struct to parse vLLM's /metrics JSON or Prometheus text.
type vLLMMetrics struct {
	RunningRequests int     `json:"running"`
	WaitingRequests int     `json:"waiting"`
	GPUCacheUsage   float64 `json:"gpu_cache_usage_perc"`
}

// GPUWatcher polls each vLLM pool's /metrics endpoint and updates Redis with
// capacity information so the policy engine and scheduler can make fast decisions.
type GPUWatcher struct {
	rdb       *redis.Client
	endpoints map[string]string // model → base URL
	capacity  map[string]int    // model → max concurrent requests (configurable)
	interval  time.Duration
	client    *http.Client
	log       *zap.Logger
}

// NewGPUWatcher constructs a GPUWatcher.
func NewGPUWatcher(
	rdb *redis.Client,
	endpoints map[string]string,
	capacity map[string]int,
	interval time.Duration,
	log *zap.Logger,
) *GPUWatcher {
	return &GPUWatcher{
		rdb:       rdb,
		endpoints: endpoints,
		capacity:  capacity,
		interval:  interval,
		client:    &http.Client{Timeout: 3 * time.Second},
		log:       log,
	}
}

// Start begins the polling loop. It runs until ctx is cancelled.
func (w *GPUWatcher) Start(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.pollAll(ctx) // immediate first poll
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pollAll(ctx)
		}
	}
}

func (w *GPUWatcher) pollAll(ctx context.Context) {
	for model, baseURL := range w.endpoints {
		metrics, err := w.fetchMetrics(baseURL)
		if err != nil {
			w.log.Warn("failed to fetch vLLM metrics",
				zap.String("model", model),
				zap.Error(err),
			)
			// Do NOT mark at-capacity on poll failure — avoid false rejects.
			continue
		}
		w.updateRedis(ctx, model, metrics)
	}
}

func (w *GPUWatcher) fetchMetrics(baseURL string) (*vLLMMetrics, error) {
	// Try /metrics/stats (JSON) first, then fall back to Prometheus /metrics text.
	url := strings.TrimRight(baseURL, "/") + "/metrics"
	resp, err := w.client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	// Attempt JSON decode (vLLM ≥ 0.4 /stats endpoint)
	var m vLLMMetrics
	if json.Unmarshal(body, &m) == nil && m.RunningRequests >= 0 {
		return &m, nil
	}

	// Fall back: parse Prometheus text format for key metrics
	m = parsePrometheusText(string(body))
	return &m, nil
}

func (w *GPUWatcher) updateRedis(ctx context.Context, model string, m *vLLMMetrics) {
	active := m.RunningRequests + m.WaitingRequests
	cap := w.maxCapacity(model)
	atCap := cap > 0 && float64(active)/float64(cap) >= maxActiveRatio

	pipe := w.rdb.Pipeline()

	activeKey := fmt.Sprintf(poolActiveKey, model)
	pipe.Set(ctx, activeKey, active, capacityTTL)

	capKey := fmt.Sprintf(poolCapKey, model)
	capVal := "0"
	if atCap {
		capVal = "1"
	}
	pipe.Set(ctx, capKey, capVal, capacityTTL)

	// Store full metrics JSON for observability
	metricsJSON, _ := json.Marshal(m)
	pipe.Set(ctx, fmt.Sprintf(poolMetricsKey, model), string(metricsJSON), capacityTTL)

	if _, err := pipe.Exec(ctx); err != nil {
		w.log.Error("failed to update pool metrics in Redis",
			zap.String("model", model), zap.Error(err))
	}

	w.log.Debug("pool metrics updated",
		zap.String("model", model),
		zap.Int("active", active),
		zap.Bool("at_capacity", atCap),
	)
}

// IsPoolAvailable returns true when the model pool is not at capacity.
func (w *GPUWatcher) IsPoolAvailable(ctx context.Context, model string) bool {
	key := fmt.Sprintf(poolCapKey, model)
	val, err := w.rdb.Get(ctx, key).Result()
	if err != nil {
		// Unknown state → assume available (fail open for availability)
		return true
	}
	return val != "1"
}

// GetPoolMetrics returns the latest cached metrics for a model pool.
func (w *GPUWatcher) GetPoolMetrics(ctx context.Context, model string) PoolMetrics {
	key := fmt.Sprintf(poolMetricsKey, model)
	data, err := w.rdb.Get(ctx, key).Bytes()
	if err != nil {
		return PoolMetrics{Model: model}
	}
	var m vLLMMetrics
	_ = json.Unmarshal(data, &m)

	capKey := fmt.Sprintf(poolCapKey, model)
	atCap, _ := w.rdb.Get(ctx, capKey).Result()

	return PoolMetrics{
		Model:          model,
		ActiveRequests: m.RunningRequests,
		QueueSize:      m.WaitingRequests,
		GPUUtilPct:     m.GPUCacheUsage * 100,
		AtCapacity:     atCap == "1",
	}
}

func (w *GPUWatcher) maxCapacity(model string) int {
	if cap, ok := w.capacity[model]; ok {
		return cap
	}
	return 100 // safe default
}

// parsePrometheusText extracts running/waiting request counts from Prometheus text format.
func parsePrometheusText(text string) vLLMMetrics {
	var m vLLMMetrics
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "vllm:num_requests_running") {
			fmt.Sscanf(extractValue(line), "%d", &m.RunningRequests)
		}
		if strings.HasPrefix(line, "vllm:num_requests_waiting") {
			fmt.Sscanf(extractValue(line), "%d", &m.WaitingRequests)
		}
		if strings.HasPrefix(line, "vllm:gpu_cache_usage_perc") {
			fmt.Sscanf(extractValue(line), "%f", &m.GPUCacheUsage)
		}
	}
	return m
}

func extractValue(line string) string {
	parts := strings.Fields(line)
	if len(parts) >= 2 {
		return parts[len(parts)-1]
	}
	return "0"
}
