// Package runtime defines the Backend interface and all backend implementations.
// Every backend (vLLM, TGI, llama.cpp, OpenAI-compatible) implements the same
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
	// ── Local / self-hosted backends ──────────────────────────────────────
	// Note: BackendLlamaCpp and BackendCPUNative are declared in their own files.
	BackendVLLM         BackendType = "vllm"
	BackendTGI          BackendType = "tgi"
	BackendOpenAICompat BackendType = "openai_compat"
	// BackendTEI is declared in tei.go.

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
	UpstreamAPIKey string // non-empty for cloud/external endpoints
}

// EmbedRequest is the canonical request passed to a backend's Embeddings method.
type EmbedRequest struct {
	Req            *models.EmbeddingRequest
	EndpointURL    string
	UpstreamAPIKey string // non-empty for cloud/external endpoints
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

	// ContainerPort returns the TCP port the backend process listens on
	// inside the container by default, before any env-var override.
	//
	// This is the container-internal port — it is NOT the host port.
	// The executor maps host_port → container_port via docker -p or by
	// injecting the env vars returned by ContainerPortEnvVars so the
	// process binds to the allocated host port instead.
	//
	// Backends that bind to a fixed well-known port that use
	// docker -p host:container mapping return 0 — no fixed default.
	ContainerPort() int

	// ContainerPortEnvVars returns the environment variables that configure
	// the backend process to listen on the given port inside the container.
	//
	// The RuntimeManager merges these into the START_MODEL task payload's
	// Env map so the node agent passes them to `docker run -e`. When
	// BindPort is pre-allocated at control-plane time these are injected
	// before dispatch. When the agent allocates the port at runtime it
	// re-applies the same env vars after port selection using the mirror
	// table in executor.go (backendPortEnvVars).
	//
	// Rules for implementors:
	//   - Return every env var the backend server reads to set its listen
	//     port. Never rely on callers knowing which vars to set.
	//   - Do NOT include generic vars like PORT unless the backend actually
	//     reads PORT at startup.
	//   - Backends that use CMD-line flags for port (llamacpp, vllm, tgi)
	//     may return nil here; their port is passed via ExtraArgs instead.
	//   - Backends with a fixed internal port that use
	//     docker -p host:container mapping return nil.
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
