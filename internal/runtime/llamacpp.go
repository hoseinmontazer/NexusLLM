package runtime

// llamacppBackend implements Backend for llama.cpp server
// (ghcr.io/ggml-org/llama.cpp:server).
//
// Wire format: llama.cpp exposes a full OpenAI-compatible /v1 API, so all
// HTTP calls delegate to openAICompatBackend. This adapter exists to isolate
// llamacpp-specific startup behaviour — primarily the --reasoning flag for
// thinking-capable models — from the shared infrastructure.
//
// This is the only place in the codebase that is permitted to know about
// llama.cpp-specific server flags. No other package should branch on
// backend == "llamacpp".

import (
	"context"
	"net/http"

	"github.com/nexusllm/nexusllm/internal/models"
)

// BackendLlamaCpp identifies the llama.cpp server backend.
const BackendLlamaCpp BackendType = "llamacpp"

// llamacppBackend wraps openAICompatBackend for HTTP calls and overrides
// PrepareStartupArgs to inject llamacpp-specific server flags.
type llamacppBackend struct {
	inner Backend // openAICompatBackend
}

// NewLlamaCppBackend constructs a llamacpp backend.
func NewLlamaCppBackend(client *http.Client) Backend {
	return &llamacppBackend{
		inner: NewOpenAICompatBackend(client),
	}
}

func (b *llamacppBackend) Type() BackendType { return BackendLlamaCpp }

func (b *llamacppBackend) Health(ctx context.Context, url string) EndpointHealth {
	// llama.cpp exposes /health (same path as openai_compat's /v1/models check
	// but returns HTTP 200 when the model is loaded, 503 while loading).
	// openAICompatBackend uses GET /v1/models which also works, but /health is
	// more semantically correct and is what the watcher already uses for all
	// other backends.
	return b.inner.Health(ctx, url)
}

func (b *llamacppBackend) Models(ctx context.Context, url string) ([]BackendModel, error) {
	return b.inner.Models(ctx, url)
}

func (b *llamacppBackend) Chat(ctx context.Context, r ChatRequest) (*BackendResponse, error) {
	return b.inner.Chat(ctx, r)
}

func (b *llamacppBackend) Embeddings(ctx context.Context, r EmbedRequest) (*models.EmbeddingResponse, error) {
	return b.inner.Embeddings(ctx, r)
}

// PrepareStartupArgs injects llamacpp-specific server flags into extra_args
// before a START_MODEL task is enqueued.
//
// Reasoning flag logic (the only llamacpp-specific startup behaviour):
//   - When a model supports thinking (SupportsThinking=true) AND thinking is
//     disabled by operator config (ThinkingEnabled=false), prepend
//     "--reasoning off" so the server enforces it at the binary level.
//   - If "--reasoning" is already present (operator override), leave it alone.
//   - All other models: return extraArgs unchanged.
//
// This function is the single canonical location for this logic.
// It was previously duplicated in:
//   - runtimemgr/activator.go (injectReasoningFlag)
//   - ha/reconciler.go (inline copy)
//
// Both of those locations now call backend.PrepareStartupArgs instead.
func (b *llamacppBackend) PrepareStartupArgs(caps ModelStartupCaps, extraArgs []string) []string {
	if !caps.SupportsThinking || caps.ThinkingEnabled {
		// Model doesn't support thinking, or thinking is already enabled by
		// default — no flag needed.
		return extraArgs
	}

	// Thinking is supported but operator has disabled it. Check whether the
	// operator already set --reasoning explicitly (respect their override).
	for _, a := range extraArgs {
		if a == "--reasoning" || a == "-rea" {
			return extraArgs // operator controls this — don't touch it
		}
	}

	// Prepend --reasoning off so it takes effect before any other args.
	return append([]string{"--reasoning", "off"}, extraArgs...)
}
