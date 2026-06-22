package runtime

import (
	"fmt"
	"net/http"
)

// Factory creates Backend instances keyed by BackendType.
// Register custom backends by calling Register before the application starts.
type Factory struct {
	constructors map[BackendType]func(*http.Client) Backend
	client       *http.Client
}

// NewFactory returns a Factory pre-registered with all built-in backends.
func NewFactory(client *http.Client) *Factory {
	f := &Factory{
		constructors: make(map[BackendType]func(*http.Client) Backend),
		client:       client,
	}
	f.Register(BackendVLLM,         NewVLLMBackend)
	f.Register(BackendOllama,        NewOllamaBackend)
	f.Register(BackendTGI,           NewTGIBackend)
	f.Register(BackendOpenAICompat,  NewOpenAICompatBackend)
	// CPU-native services (embeddings, rerankers, STT, TTS, OCR, MCP, agents)
	f.Register(BackendCPUNative,     NewCPUNativeBackend)
	// llama.cpp server — OpenAI-compatible, CPU inference
	f.Register(BackendType("llamacpp"), NewOpenAICompatBackend) // wire format is identical to openai_compat
	return f
}

// Register adds a constructor for a BackendType.
func (f *Factory) Register(t BackendType, constructor func(*http.Client) Backend) {
	f.constructors[t] = constructor
}

// Build returns a Backend for the given type, sharing the factory's HTTP client.
func (f *Factory) Build(t BackendType) (Backend, error) {
	ctor, ok := f.constructors[t]
	if !ok {
		return nil, fmt.Errorf("unknown backend type: %q", t)
	}
	return ctor(f.client), nil
}

// MustBuild panics if the backend type is unknown.
func (f *Factory) MustBuild(t BackendType) Backend {
	b, err := f.Build(t)
	if err != nil {
		panic(err)
	}
	return b
}
