package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nexusllm/nexusllm/internal/models"
)

// vllmBackend implements Backend for vLLM servers.
// vLLM exposes the full OpenAI-compatible API at /v1/*.
type vllmBackend struct {
	client *http.Client
}

// NewVLLMBackend constructs a vLLM backend with a shared HTTP client.
func NewVLLMBackend(client *http.Client) Backend {
	if client == nil {
		client = &http.Client{
			Timeout: 5 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}
	return &vllmBackend{client: client}
}

func (b *vllmBackend) Type() BackendType { return BackendVLLM }

// Health polls /health (liveness) and /metrics (capacity) to produce a
// complete EndpointHealth snapshot.
func (b *vllmBackend) Health(ctx context.Context, url string) EndpointHealth {
	h := EndpointHealth{
		URL:       url,
		Status:    StatusDown,
		CheckedAt: time.Now(),
	}

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/health", nil)
	if err != nil {
		h.Error = err.Error()
		return h
	}

	resp, err := b.client.Do(req)
	h.LatencyMs = int(time.Since(start).Milliseconds())
	if err != nil {
		h.Error = err.Error()
		return h
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		h.Status = StatusHealthy
	} else {
		h.Status = StatusDegraded
		h.Error = fmt.Sprintf("health returned HTTP %d", resp.StatusCode)
	}

	// Best-effort metrics scrape for capacity info.
	b.enrichFromMetrics(ctx, url, &h)
	return h
}

// enrichFromMetrics scrapes vLLM's Prometheus /metrics endpoint for queue depth
// and GPU cache utilisation. Failures are silently ignored — health data is
// already populated from /health.
func (b *vllmBackend) enrichFromMetrics(ctx context.Context, baseURL string, h *EndpointHealth) {
	mCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(mCtx, http.MethodGet, baseURL+"/metrics", nil)
	if err != nil {
		return
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "vllm:num_requests_running"):
			fmt.Sscanf(extractMetricValue(line), "%d", &h.RunningReqs)
		case strings.HasPrefix(line, "vllm:num_requests_waiting"):
			fmt.Sscanf(extractMetricValue(line), "%d", &h.WaitingReqs)
		case strings.HasPrefix(line, "vllm:gpu_cache_usage_perc"):
			fmt.Sscanf(extractMetricValue(line), "%f", &h.GPUCacheUtil)
		}
	}
	h.ActiveReqs = h.RunningReqs + h.WaitingReqs

	// Promote to degraded if GPU cache is nearly full.
	if h.Status == StatusHealthy && h.GPUCacheUtil > 0.95 {
		h.Status = StatusDegraded
	}
}

func (b *vllmBackend) Models(ctx context.Context, url string) ([]BackendModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s/v1/models: %w", url, err)
	}
	defer resp.Body.Close()

	var list models.ModelListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	out := make([]BackendModel, len(list.Data))
	for i, m := range list.Data {
		out[i] = BackendModel{ID: m.ID, Object: m.Object, Created: m.Created, OwnedBy: m.OwnedBy}
	}
	return out, nil
}

func (b *vllmBackend) Chat(ctx context.Context, r ChatRequest) (*BackendResponse, error) {
	body, err := json.Marshal(r.Req)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.EndpointURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.Req.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chat completion: %w", err)
	}

	br := &BackendResponse{
		StatusCode: resp.StatusCode,
		Headers:    map[string]string{"Content-Type": resp.Header.Get("Content-Type")},
	}

	if r.Req.Stream {
		br.Stream = &sseStream{reader: bufio.NewReader(resp.Body), closer: resp.Body}
	} else {
		br.Body, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read chat response: %w", err)
		}
	}
	return br, nil
}

func (b *vllmBackend) Embeddings(ctx context.Context, r EmbedRequest) (*models.EmbeddingResponse, error) {
	body, err := json.Marshal(r.Req)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.EndpointURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings: %w", err)
	}
	defer resp.Body.Close()

	var out models.EmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode embeddings: %w", err)
	}
	return &out, nil
}

// ─── SSE stream reader ────────────────────────────────────────────────────────

type sseStream struct {
	reader *bufio.Reader
	closer io.Closer
}

func (s *sseStream) ReadLine() (string, error) {
	line, err := s.reader.ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

func (s *sseStream) Close() error { return s.closer.Close() }

// ─── helpers ──────────────────────────────────────────────────────────────────

func extractMetricValue(line string) string {
	parts := strings.Fields(line)
	if len(parts) >= 2 {
		return parts[len(parts)-1]
	}
	return "0"
}
