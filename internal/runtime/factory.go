package runtime

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Factory creates Backend instances keyed by BackendType.
// Register custom backends by calling Register before the application starts.
type Factory struct {
	constructors map[BackendType]func(*http.Client) Backend
	client       *http.Client // shared client for local/direct endpoints
	globalProxy  string       // NEXUS_UPSTREAM_PROXY fallback; "" = direct

	// proxyClients caches one *http.Client per proxy URL so we don't allocate
	// a new transport per request. Access is guarded by proxyMu.
	proxyMu      sync.RWMutex
	proxyClients map[string]*http.Client
}

// NewFactory returns a Factory pre-registered with all built-in backends.
func NewFactory(client *http.Client) *Factory {
	f := &Factory{
		constructors: make(map[BackendType]func(*http.Client) Backend),
		client:       client,
		proxyClients: make(map[string]*http.Client),
	}
	f.Register(BackendVLLM, NewVLLMBackend)
	f.Register(BackendTGI, NewTGIBackend)
	f.Register(BackendOpenAICompat, NewOpenAICompatBackend)
	// CPU-native services (embeddings, rerankers, STT, TTS, OCR, MCP, agents)
	f.Register(BackendCPUNative, NewCPUNativeBackend)
	// llama.cpp server — has its own adapter for startup arg preparation.
	// HTTP wire format is OpenAI-compatible, but PrepareStartupArgs handles
	// llamacpp-specific flags (e.g. --reasoning off for thinking models).
	f.Register(BackendLlamaCpp, NewLlamaCppBackend)
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

// ClientFor returns an *http.Client suitable for calling the given proxy URL.
//
//   - proxyURL == "" → returns the factory's direct client (no proxy).
//   - proxyURL != "" → returns a cached client whose transport routes through
//     that proxy. One transport is allocated per unique proxy URL and reused
//     across all requests to endpoints that share the same proxy.
//
// This is used by the gateway proxy handler to make upstream calls with the
// correct transport for each cloud endpoint.
func (f *Factory) ClientFor(proxyURL string) *http.Client {
	if proxyURL == "" {
		return f.client
	}

	// Fast path: already cached.
	f.proxyMu.RLock()
	if c, ok := f.proxyClients[proxyURL]; ok {
		f.proxyMu.RUnlock()
		return c
	}
	f.proxyMu.RUnlock()

	// Parse the proxy URL — fall back to direct client on parse error.
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return f.client
	}

	// Slow path: build and cache a new proxy-aware client.
	f.proxyMu.Lock()
	defer f.proxyMu.Unlock()
	// Double-check after acquiring write lock.
	if c, ok := f.proxyClients[proxyURL]; ok {
		return c
	}

	c := &http.Client{
		Timeout: 5 * time.Minute,
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(parsed),
			MaxIdleConnsPerHost: 32,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	f.proxyClients[proxyURL] = c
	return c
}

// DirectClient returns the factory's direct (no-proxy) HTTP client.
// Use this for local model endpoints.
func (f *Factory) DirectClient() *http.Client {
	return f.client
}

// SetGlobalProxy sets the default proxy URL applied to all endpoints that
// do not set their own upstream_proxy. Call this once at startup with the
// value of NEXUS_UPSTREAM_PROXY. An empty string disables the global proxy.
func (f *Factory) SetGlobalProxy(proxyURL string) {
	f.proxyMu.Lock()
	f.globalProxy = proxyURL
	f.proxyMu.Unlock()
}

// GlobalProxy returns the current global proxy URL (may be "").
func (f *Factory) GlobalProxy() string {
	f.proxyMu.RLock()
	defer f.proxyMu.RUnlock()
	return f.globalProxy
}
