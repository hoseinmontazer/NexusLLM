package runtime

// cpuNativeBackend implements Backend for CPU-native AI services.
// These services expose an OpenAI-compatible HTTP API but run on CPU,
// examples: faster-whisper-server, Infinity embedding server, llama.cpp,
// Kokoro TTS, EasyOCR REST, MCP HTTP bridges.
//
// Because the wire format is identical to openai_compat, cpuNativeBackend
// delegates to openAICompatBackend for all HTTP calls and only overrides
// Type() to allow distinct routing in the gateway and placement logic.

import (
	"context"
	"net/http"

	"github.com/nexusllm/nexusllm/internal/models"
)

// BackendCPUNative identifies a CPU-native runtime.
const BackendCPUNative BackendType = "cpu_native"

// cpuNativeBackend wraps the OpenAI-compat backend but identifies itself
// as CPU_NATIVE so the placement engine, scheduler, and metrics all track
// it separately from GPU workloads.
type cpuNativeBackend struct {
	inner Backend // openAICompatBackend
}

// NewCPUNativeBackend constructs a CPU-native backend.
func NewCPUNativeBackend(client *http.Client) Backend {
	return &cpuNativeBackend{
		inner: NewOpenAICompatBackend(client),
	}
}

func (b *cpuNativeBackend) Type() BackendType { return BackendCPUNative }

func (b *cpuNativeBackend) Health(ctx context.Context, url string) EndpointHealth {
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
