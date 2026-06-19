// Package runtime defines the Backend interface and all backend implementations.
// Every backend (vLLM, Ollama, TGI, OpenAI-compatible) implements the same
// interface so the router layer never needs to know which technology is serving
// a model.
package runtime

import (
	"context"
	"time"

	"github.com/nexusllm/nexusllm/internal/models"
)

// ─────────────────────────────────────────────────────────────────────────────
// Core types
// ─────────────────────────────────────────────────────────────────────────────

// BackendType enumerates the supported runtime backends.
type BackendType string

const (
	BackendVLLM        BackendType = "vllm"
	BackendOllama      BackendType = "ollama"
	BackendTGI         BackendType = "tgi"
	BackendOpenAICompat BackendType = "openai_compat"
)

// EndpointHealth represents the health state of a single backend endpoint.
type EndpointHealth struct {
	EndpointID   string
	URL          string
	Status       HealthStatus
	LatencyMs    int
	Error        string
	CheckedAt    time.Time
	ActiveReqs   int
	RunningReqs  int  // vLLM-specific
	WaitingReqs  int  // vLLM-specific
	GPUCacheUtil float64
}

// HealthStatus is the observed health of a backend endpoint.
type HealthStatus string

const (
	StatusHealthy  HealthStatus = "healthy"
	StatusDegraded HealthStatus = "degraded"
	StatusDown     HealthStatus = "down"
	StatusUnknown  HealthStatus = "unknown"
	StatusDraining HealthStatus = "draining"
)

// BackendModel describes a model as reported by the backend's /v1/models endpoint.
type BackendModel struct {
	ID      string
	Object  string
	Created int64
	OwnedBy string
}

// ChatRequest is the canonical request passed to a backend's Chat method.
// It carries the original InferenceRequest plus routing metadata.
type ChatRequest struct {
	Req        *models.InferenceRequest
	EndpointURL string
}

// EmbedRequest is the canonical request passed to a backend's Embeddings method.
type EmbedRequest struct {
	Req         *models.EmbeddingRequest
	EndpointURL string
}

// ─────────────────────────────────────────────────────────────────────────────
// Backend interface
// ─────────────────────────────────────────────────────────────────────────────

// Backend is the abstraction over a model runtime.
// All methods are context-aware so callers can set deadlines and cancel.
type Backend interface {
	// Type returns the backend type identifier.
	Type() BackendType

	// Health checks liveness and readiness of the endpoint at url.
	// The returned EndpointHealth is fully populated even on failure.
	Health(ctx context.Context, url string) EndpointHealth

	// Models lists models currently loaded on the backend endpoint.
	Models(ctx context.Context, url string) ([]BackendModel, error)

	// Chat sends a chat completion request.
	// For streaming requests the response body must be an SSE stream;
	// for non-streaming it must be a JSON ChatCompletionResponse.
	// The caller is responsible for closing the response body.
	Chat(ctx context.Context, r ChatRequest) (*BackendResponse, error)

	// Embeddings sends an embeddings request and returns the parsed response.
	Embeddings(ctx context.Context, r EmbedRequest) (*models.EmbeddingResponse, error)
}

// BackendResponse wraps the raw HTTP response so callers can proxy it directly.
type BackendResponse struct {
	StatusCode int
	Body       []byte       // populated for non-streaming
	Stream     StreamReader // populated for streaming (nil otherwise)
	Headers    map[string]string
}

// StreamReader is the interface for consuming an SSE stream.
type StreamReader interface {
	// ReadLine returns the next SSE line. Returns ("", io.EOF) when done.
	ReadLine() (string, error)
	Close() error
}
