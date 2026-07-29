// Package runtime defines the Backend interface and all backend implementations.
// Every backend (vLLM, TGI, llama.cpp, OpenAI-compatible) implements the same
// interface so the router layer never needs to know which technology is serving
// a model.
package runtime

import (
	"context"
	"net/http"
	"time"

	"github.com/nexusllm/nexusllm/internal/models"
)

// ─────────────────────────────────────────────────────────────────────────────
// Core types
// ─────────────────────────────────────────────────────────────────────────────

// BackendType enumerates the supported runtime backends.
type BackendType string

const (
	// ── Local / self-hosted backends ──────────────────────────────────────
	// Note: BackendLlamaCpp and BackendCPUNative are declared in their own files.
	BackendVLLM         BackendType = "vllm"
	BackendTGI          BackendType = "tgi"
	BackendOpenAICompat BackendType = "openai_compat"

	// ── External / cloud provider backends ────────────────────────────────
	// These implement the same Backend interface.
	// Lifecycle methods (ContainerPort, ContainerPortEnvVars, PrepareStartupArgs)
	// are all no-ops. The gateway never starts, stops, or schedules these.
	// Everything flows through the single model registry and policy engine.

	// BackendOpenAI routes to api.openai.com.
	// Wire format: OpenAI Chat Completions API (native).
	BackendOpenAI BackendType = "openai_provider"

	// BackendAnthropic routes to api.anthropic.com.
	// Wire format: Anthropic Messages API → translated to OpenAI response format.
	BackendAnthropic BackendType = "anthropic_provider"

	// BackendGemini routes to generativelanguage.googleapis.com.
	// Wire format: Gemini API (OpenAI-compat endpoint at /v1beta/openai/).
	BackendGemini BackendType = "google_provider"

	// BackendAzureOpenAI routes to <resource>.openai.azure.com.
	// Wire format: Azure OpenAI REST API with api-version query param.
	BackendAzureOpenAI BackendType = "azure_openai_provider"

	// BackendOpenRouter routes to openrouter.ai.
	// Wire format: OpenAI-compatible (OpenRouter is a proxy/aggregator).
	BackendOpenRouter BackendType = "openrouter_provider"

	// BackendGroq routes to api.groq.com.
	// Wire format: OpenAI-compatible.
	BackendGroq BackendType = "groq_provider"

	// BackendTogether routes to api.together.xyz.
	// Wire format: OpenAI-compatible.
	BackendTogether BackendType = "together_provider"

	// BackendMistral routes to api.mistral.ai.
	// Wire format: OpenAI-compatible.
	BackendMistral BackendType = "mistral_provider"

	// BackendCohere routes to api.cohere.com.
	// Wire format: OpenAI-compatible (/v2/chat → chat/completions alias).
	BackendCohere BackendType = "cohere_provider"

	// BackendDeepSeek routes to api.deepseek.com.
	// Wire format: OpenAI-compatible.
	BackendDeepSeek BackendType = "deepseek_provider"
)

// IsProviderBackend reports whether a BackendType is an external cloud provider.
// Provider backends skip all lifecycle operations (EnsureRunning, scheduler,
// container management). They are always considered "running".
func IsProviderBackend(t BackendType) bool {
	switch t {
	case BackendOpenAI, BackendAnthropic, BackendGemini, BackendAzureOpenAI,
		BackendOpenRouter, BackendGroq, BackendTogether, BackendMistral,
		BackendCohere, BackendDeepSeek:
		return true
	}
	return false
}

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
	Req            *models.InferenceRequest
	EndpointURL    string
	UpstreamAPIKey string       // non-empty for cloud/external endpoints
	Client         *http.Client // per-endpoint dedicated client; nil = backend uses its own
}

// EmbedRequest is the canonical request passed to a backend's Embeddings method.
type EmbedRequest struct {
	Req            *models.EmbeddingRequest
	EndpointURL    string
	UpstreamAPIKey string       // non-empty for cloud/external endpoints
	Client         *http.Client // per-endpoint dedicated client; nil = backend uses its own
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
	// client is the per-endpoint HTTP client to use; local backends may
	// ignore it and use their own shared client.
	// The returned EndpointHealth is fully populated even on failure.
	Health(ctx context.Context, url string, client *http.Client) EndpointHealth

	// Models lists models currently loaded on the backend endpoint.
	// client is the per-endpoint HTTP client to use.
	Models(ctx context.Context, url string, client *http.Client) ([]BackendModel, error)

	// Chat sends a chat completion request.
	// r.Client carries the per-endpoint HTTP client when set; backends
	// fall back to their own shared client when r.Client is nil.
	// For streaming requests the response body must be an SSE stream;
	// for non-streaming it must be a JSON ChatCompletionResponse.
	// The caller is responsible for closing the response body.
	Chat(ctx context.Context, r ChatRequest) (*BackendResponse, error)

	// Embeddings sends an embeddings request and returns the parsed response.
	// r.Client carries the per-endpoint HTTP client when set.
	Embeddings(ctx context.Context, r EmbedRequest) (*models.EmbeddingResponse, error)

	// PrepareStartupArgs allows a backend adapter to inject or modify the
	// extra_args that are sent in a START_MODEL task payload.
	PrepareStartupArgs(caps ModelStartupCaps, extraArgs []string) []string

	// ContainerPort returns the TCP port the backend process listens on
	// inside the container by default, before any env-var override.
	ContainerPort() int

	// ContainerPortEnvVars returns the environment variables that configure
	// the backend process to listen on the given port inside the container.
	ContainerPortEnvVars(port int) map[string]string
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
