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
	BackendVLLM         BackendType = "vllm"
	BackendOllama       BackendType = "ollama"
	BackendTGI          BackendType = "tgi"
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
	RunningReqs  int // vLLM-specific
	WaitingReqs  int // vLLM-specific
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
	Req         *models.InferenceRequest
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
//
// Design: every model type (LLM, STT, TTS, OCR, Embedding, Rerank, Vision,
// ImageGen, ...) uses the same Backend interface. Type-specific endpoints
// (Transcriptions, Speech, OCR, etc.) call ForwardRaw() which proxies the
// request verbatim to the backend at the given path. Only Chat and Embeddings
// have typed methods because they require request/response transformation.
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

	// PrepareStartupArgs allows a backend adapter to inject or modify the
	// extra_args that are sent in a START_MODEL task payload.
	//
	// The RuntimeManager calls this once per startup event, passing the
	// current extra_args and the model's capability flags. The adapter may
	// prepend, append, or leave the args unchanged based purely on its own
	// knowledge — no backend-specific logic belongs in the caller.
	//
	// caps holds model-level flags declared in the models table. Adapters
	// that have no startup customisation should return extraArgs unchanged.
	PrepareStartupArgs(caps ModelStartupCaps, extraArgs []string) []string
}

// ModelStartupCaps carries model-level capability flags that backend adapters
// may consult in PrepareStartupArgs. This is the only context the adapter
// receives about the model — it must not receive workload-type strings.
type ModelStartupCaps struct {
	// SupportsThinking indicates the model has an internal reasoning chain
	// (e.g. Qwen3-thinking, DeepSeek-R1). Backend adapters that have a
	// server-level flag to disable reasoning should check this.
	SupportsThinking bool
	// ThinkingEnabled is the operator-configured deployment default.
	// When SupportsThinking=true and ThinkingEnabled=false, the adapter
	// should emit whatever flag disables reasoning at the server level.
	ThinkingEnabled bool
}

// ─────────────────────────────────────────────────────────────────────────────
// Capability identifiers
// ─────────────────────────────────────────────────────────────────────────────

// Capability is an API endpoint that a model supports.
// These match the values stored in models.capabilities JSONB column.
type Capability string

const (
	CapabilityChat            Capability = "chat"
	CapabilityCompletion      Capability = "completion"
	CapabilityResponses       Capability = "responses"
	CapabilityEmbedding       Capability = "embedding"
	CapabilityRerank          Capability = "rerank"
	CapabilityTranscription   Capability = "transcription"
	CapabilitySpeech          Capability = "speech"
	CapabilityOCR             Capability = "ocr"
	CapabilityVision          Capability = "vision"
	CapabilityImageGeneration Capability = "image_generation"
	CapabilityModeration      Capability = "moderation"
)

// DefaultCapabilities returns the capabilities implied by a service_type.
// Used to seed the capabilities column and for fallback validation when a
// model's capabilities JSONB column is empty.
func DefaultCapabilities(serviceType string) []Capability {
	switch serviceType {
	case "CHAT":
		return []Capability{CapabilityChat, CapabilityCompletion}
	case "EMBEDDING":
		return []Capability{CapabilityEmbedding}
	case "RERANK":
		return []Capability{CapabilityRerank}
	case "STT":
		return []Capability{CapabilityTranscription}
	case "TTS":
		return []Capability{CapabilitySpeech}
	case "OCR":
		return []Capability{CapabilityOCR}
	case "VISION":
		return []Capability{CapabilityChat, CapabilityVision}
	case "IMAGE_GENERATION":
		return []Capability{CapabilityImageGeneration}
	case "MODERATION":
		return []Capability{CapabilityModeration}
	case "AGENT", "MCP":
		return []Capability{CapabilityChat, CapabilityCompletion}
	default:
		return []Capability{}
	}
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
