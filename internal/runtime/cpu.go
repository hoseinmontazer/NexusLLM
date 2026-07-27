package runtime

// cpuNativeBackend implements Backend for CPU-native AI services.
// These services expose an OpenAI-compatible HTTP API but run on CPU,
// examples: faster-whisper-server, Infinity embedding server, llama.cpp,
// Kokoro TTS, EasyOCR REST, MCP HTTP bridges.
//
// Health check probes /health first (FastAPI/uvicorn standard endpoint used
// by faster-whisper-server, Kokoro, and most Python AI servers) then falls
// back to GET /v1/models for services that don't have /health.

import (
	"context"
	"net/http"
	"time"

	"github.com/nexusllm/nexusllm/internal/models"
)

// BackendCPUNative identifies a CPU-native runtime.
const BackendCPUNative BackendType = "cpu_native"

// cpuNativeBackend wraps the OpenAI-compat backend but identifies itself
// as CPU_NATIVE so the placement engine, scheduler, and metrics all track
// it separately from GPU workloads.
type cpuNativeBackend struct {
	inner  Backend // openAICompatBackend
	client *http.Client
}

// NewCPUNativeBackend constructs a CPU-native backend.
func NewCPUNativeBackend(client *http.Client) Backend {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &cpuNativeBackend{
		inner:  NewOpenAICompatBackend(client),
		client: client,
	}
}

func (b *cpuNativeBackend) Type() BackendType { return BackendCPUNative }

// PrepareStartupArgs — CPU-native services have no server-level reasoning
// flags; args returned unchanged.
func (b *cpuNativeBackend) PrepareStartupArgs(caps ModelStartupCaps, extraArgs []string) []string {
	return extraArgs
}

// Health probes /health first (used by faster-whisper-server, Kokoro, Infinity,
// and most FastAPI-based servers), then falls back to GET /v1/models.
// Any 2xx or 4xx response from /health counts as healthy — the service is up.
func (b *cpuNativeBackend) Health(ctx context.Context, url string) EndpointHealth {
	h := EndpointHealth{URL: url, Status: StatusDown, CheckedAt: time.Now()}
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/health", nil)
	if err == nil {
		resp, doErr := b.client.Do(req)
		h.LatencyMs = int(time.Since(start).Milliseconds())
		if doErr == nil {
			resp.Body.Close()
			// Any non-5xx response means the server is alive.
			// faster-whisper-server returns 200 {"status":"ok"} on /health.
			if resp.StatusCode < 500 {
				h.Status = StatusHealthy
				return h
			}
		}
	}

	// Fall back to GET /v1/models for OpenAI-compat services that expose it
	// (e.g. Infinity embedding server, some vLLM-compatible CPU backends).
	return b.inner.Health(ctx, url)
}

func (b *cpuNativeBackend) Models(ctx context.Context, url string) ([]BackendModel, error) {
	return b.inner.Models(ctx, url)
}

func (b *cpuNativeBackend) Chat(ctx context.Context, r ChatRequest) (*BackendResponse, error) {
	return b.inner.Chat(ctx, r)
}

func (b *cpuNativeBackend) Embeddings(ctx context.Context, r EmbedRequest) (*models.EmbeddingResponse, error) {
	return b.inner.Embeddings(ctx, r)
}
