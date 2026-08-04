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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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

// ContainerPort returns 0 — cpu_native services (faster-whisper-server,
// Kokoro, Infinity, custom Python AI servers) do not share a single fixed
// internal port.  The listen port is driven entirely by ContainerPortEnvVars.
func (b *cpuNativeBackend) ContainerPort() int { return 0 }

// ContainerPortEnvVars injects every environment variable that cpu_native
// backend processes may read to set their HTTP listen port.
//
// Variables injected (all set to the same value so any framework is covered):
//   - PORT         — generic convention (PaaS, gunicorn, hypercorn)
//   - HTTP_PORT    — used by some custom server wrappers
//   - UVICORN_PORT — read by faster-whisper-server and most uvicorn-based
//     Python AI servers (the most common cpu_native runtime)
//
// All three are set to the same allocated host port so whichever variable the
// server image reads, it binds to the correct port.
//
// If a future backend image reads a different variable, add it here.
// Never add these variables outside this function — backend-specific port
// knowledge belongs only in the backend driver.
func (b *cpuNativeBackend) ContainerPortEnvVars(port int) map[string]string {
	s := strconv.Itoa(port)
	return map[string]string{
		"PORT":         s,
		"HTTP_PORT":    s,
		"UVICORN_PORT": s,
	}
}

// PrepareStartupArgs — CPU-native services have no server-level reasoning
// flags; args returned unchanged.
func (b *cpuNativeBackend) PrepareStartupArgs(caps ModelStartupCaps, extraArgs []string) []string {
	return extraArgs
}

// Health probes /health first (used by faster-whisper-server, Kokoro, Infinity,
// and most FastAPI-based servers), then falls back to GET /v1/models.
// Any 2xx or 4xx response from /health counts as healthy — the service is up.
func (b *cpuNativeBackend) Health(ctx context.Context, url string, client *http.Client) EndpointHealth {
	c := b.client
	if client != nil {
		c = client
	}
	h := EndpointHealth{URL: url, Status: StatusDown, CheckedAt: time.Now()}
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/health", nil)
	if err == nil {
		resp, doErr := c.Do(req)
		h.LatencyMs = int(time.Since(start).Milliseconds())
		if doErr == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				h.Status = StatusHealthy
				return h
			}
		}
	}

	// Fall back to GET /v1/models.
	return b.inner.Health(ctx, url, client)
}

func (b *cpuNativeBackend) Models(ctx context.Context, url string, client *http.Client) ([]BackendModel, error) {
	return b.inner.Models(ctx, url, client)
}

func (b *cpuNativeBackend) Chat(ctx context.Context, r ChatRequest) (*BackendResponse, error) {
	return b.inner.Chat(ctx, r)
}

func (b *cpuNativeBackend) Embeddings(ctx context.Context, r EmbedRequest) (*models.EmbeddingResponse, error) {
	c := b.client
	if r.Client != nil {
		c = r.Client
	}
	body, err := json.Marshal(r.Req)
	if err != nil {
		return nil, err
	}

	for _, path := range []string{"/v1/embeddings", "/embeddings"} {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost,
			r.EndpointURL+path, bytes.NewReader(body))
		if reqErr != nil {
			return nil, reqErr
		}
		req.Header.Set("Content-Type", "application/json")
		if r.UpstreamAPIKey != "" {
			req.Header.Set("Authorization", "Bearer "+r.UpstreamAPIKey)
		}
		resp, doErr := c.Do(req)
		if doErr != nil {
			return nil, doErr
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			continue
		}
		defer resp.Body.Close()
		var out models.EmbeddingResponse
		if decErr := json.NewDecoder(resp.Body).Decode(&out); decErr != nil {
			return nil, decErr
		}
		return &out, nil
	}
	return nil, fmt.Errorf("embeddings endpoint not found at %s — tried /v1/embeddings and /embeddings", r.EndpointURL)
}
