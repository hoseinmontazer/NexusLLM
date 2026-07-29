package catalog

import (
	"net/http"
	"sync"

	"github.com/nexusllm/nexusllm/internal/runtime"
)

// VirtualEndpoint is a synthetic endpoint constructed at request time from a
// catalog entry. It is never stored in a registry Pool or model_endpoints row.
// The gateway pipeline uses it identically to a real *runtime.Endpoint.
type VirtualEndpoint struct {
	// ID is "virt:<provider_id>:<provider_model_id>" — unique per catalog entry.
	ID                string
	BackendType       runtime.BackendType
	UpstreamBaseURL   string
	UpstreamAPIKey    string
	UpstreamModelName string
	Transport         runtime.ProviderTransportConfig

	// Capability flags — loaded directly from provider_remote_models.supports_*
	// columns at cache-build time. These are the sole source of truth for
	// capability validation of virtual (Mode-B) models. They are NEVER inferred
	// from the model name string.
	SupportsStreaming  bool
	SupportsTools     bool
	SupportsVision    bool
	SupportsAudio     bool
	SupportsEmbedding bool
	SupportsReasoning bool
}

// AsEndpoint converts a VirtualEndpoint into a *runtime.Endpoint so that
// the existing proxy.Handler can use it without any type changes.
func (v *VirtualEndpoint) AsEndpoint() *runtime.Endpoint {
	return &runtime.Endpoint{
		ID:                v.ID,
		BackendType:       v.BackendType,
		UpstreamBaseURL:   v.UpstreamBaseURL,
		UpstreamAPIKey:    v.UpstreamAPIKey,
		UpstreamModelName: v.UpstreamModelName,
		Transport:         v.Transport,
		Status:            runtime.StatusHealthy,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ProviderClientCache
// ─────────────────────────────────────────────────────────────────────────────

// ProviderClientCache holds one *http.Client per provider, keyed by provider_id.
// All virtual models sharing a provider reuse the same client — correct because
// they share the same proxy, TLS, and pool configuration.
//
// This is the catalog equivalent of runtime.Registry.epClients.
// Transport isolation is guaranteed: one client per provider, never shared
// across providers, never reads HTTP_PROXY / HTTPS_PROXY env vars.
type ProviderClientCache struct {
	mu      sync.RWMutex
	clients map[string]*http.Client // provider_id → client
}

// NewProviderClientCache constructs an empty cache.
func NewProviderClientCache() *ProviderClientCache {
	return &ProviderClientCache{clients: make(map[string]*http.Client)}
}

// GetOrBuild returns the cached client for the provider, building it if needed.
func (c *ProviderClientCache) GetOrBuild(p *Provider) (*http.Client, error) {
	c.mu.RLock()
	if cl, ok := c.clients[p.ID]; ok {
		c.mu.RUnlock()
		return cl, nil
	}
	c.mu.RUnlock()

	cl, err := runtime.BuildProviderClient(p.Transport())
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	// Double-check after acquiring write lock.
	if existing, ok := c.clients[p.ID]; ok {
		c.mu.Unlock()
		return existing, nil
	}
	c.clients[p.ID] = cl
	c.mu.Unlock()
	return cl, nil
}

// Invalidate removes the cached client for a provider, forcing a rebuild on
// the next request. Call after updating a provider's transport config.
func (c *ProviderClientCache) Invalidate(providerID string) {
	c.mu.Lock()
	delete(c.clients, providerID)
	c.mu.Unlock()
}

// InvalidateAll clears the entire cache.
func (c *ProviderClientCache) InvalidateAll() {
	c.mu.Lock()
	c.clients = make(map[string]*http.Client)
	c.mu.Unlock()
}
