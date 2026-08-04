package catalog

import (
	"context"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/runtime"
	"go.uber.org/zap"
)

// exposedRow is the DB row returned by the exposed-catalog query.
type exposedRow struct {
	RemoteModelID                string `db:"remote_model_id"`
	ProviderID                   string `db:"provider_id"`
	ProviderModelID              string `db:"provider_model_id"`
	DisplayName                  string `db:"display_name"`
	ContextLength                *int   `db:"context_length"`
	SupportsStreaming            bool   `db:"supports_streaming"`
	SupportsTools                bool   `db:"supports_tools"`
	SupportsVision               bool   `db:"supports_vision"`
	SupportsEmbedding            bool   `db:"supports_embeddings"`
	SupportsReasoning            bool   `db:"supports_reasoning"`
	ProviderName                 string `db:"provider_name"`
	BackendType                  string `db:"backend_type"`
	BaseURL                      string `db:"base_url"`
	APIKey                       string `db:"api_key"`
	ProxyURL                     string `db:"proxy_url"`
	TLSInsecureSkipVerify        bool   `db:"tls_insecure_skip_verify"`
	TLSRootCAPEM                 string `db:"tls_root_ca_pem"`
	ConnectTimeoutSeconds        int    `db:"connect_timeout_seconds"`
	ReadTimeoutSeconds           int    `db:"read_timeout_seconds"`
	IdleConnTimeoutSeconds       int    `db:"idle_conn_timeout_seconds"`
	ResponseHeaderTimeoutSeconds int    `db:"response_header_timeout_seconds"`
	MaxIdleConnsPerHost          int    `db:"max_idle_conns_per_host"`
	MaxConnsPerHost              int    `db:"max_conns_per_host"`
	DisableHTTP2                 bool   `db:"disable_http2"`
	VirtualModelName             string `db:"virtual_model_name"`
	TagsRaw                      string `db:"tags_raw"`
}

// virtualCache is the in-memory resolved catalog used by hot-path resolution.
type virtualCache struct {
	byName  map[string]*VirtualEndpoint // virtual_model_name → endpoint
	list    []string                    // ordered list of virtual model names
	builtAt time.Time
}

// VirtualModelResolver resolves Mode-B virtual model names to VirtualEndpoints.
// It caches the exposed catalog in memory and refreshes on Invalidate().
type VirtualModelResolver struct {
	db      *sqlx.DB
	store   *ProviderStore
	rules   *RuleStore
	engine  *RuleEngine
	clients *ProviderClientCache
	log     *zap.Logger

	mu    sync.RWMutex
	cache *virtualCache
}

// NewVirtualModelResolver constructs a resolver.
func NewVirtualModelResolver(db *sqlx.DB, log *zap.Logger) *VirtualModelResolver {
	return &VirtualModelResolver{
		db:      db,
		store:   NewProviderStore(db),
		rules:   NewRuleStore(db),
		engine:  NewRuleEngine(),
		clients: NewProviderClientCache(),
		log:     log,
	}
}

// Resolve returns a VirtualEndpoint for a Mode-B model name.
// Returns (nil, nil) when the name is not in the exposed catalog.
func (r *VirtualModelResolver) Resolve(ctx context.Context, modelName string) (*VirtualEndpoint, error) {
	cache, err := r.getCache(ctx)
	if err != nil {
		return nil, err
	}
	ep, ok := cache.byName[modelName]
	if !ok {
		return nil, nil
	}
	return ep, nil
}

// ListExposed returns all virtual model names currently exposed.
// Used by GET /v1/models.
func (r *VirtualModelResolver) ListExposed(ctx context.Context) ([]string, error) {
	cache, err := r.getCache(ctx)
	if err != nil {
		return nil, err
	}
	return cache.list, nil
}

// ListExposedForProject returns the virtual model names the given project is
// allowed to call, based on its project_provider_access grants.
//
// It reads the full exposed catalog from the in-memory cache (fast) and
// filters each name through the project's provider ACL grants (also fast —
// the ACL is a small in-memory list loaded once per call from the DB).
//
// Returns an empty slice when the project has no provider grants or when
// no virtual models are exposed.  Never returns an error for missing grants
// (just an empty list), to avoid blocking the Models endpoint on a DB error.
func (r *VirtualModelResolver) ListExposedForProject(ctx context.Context, projectID string) ([]string, error) {
	cache, err := r.getCache(ctx)
	if err != nil {
		return nil, err
	}
	if len(cache.list) == 0 {
		return nil, nil
	}

	// Load project's provider access grants from DB.
	store := NewProjectProviderAccessStore(r.db)
	grants, err := store.ListForProject(ctx, projectID)
	if err != nil || len(grants) == 0 {
		return nil, nil
	}

	out := make([]string, 0, len(cache.list))
	for _, name := range cache.list {
		for i := range grants {
			if grants[i].IsAllowed(name) {
				out = append(out, name)
				break
			}
		}
	}
	return out, nil
}

// ListExposedForProjectWithMeta is like ListExposedForProject but also returns
// the VirtualEndpoint for each name so the proxy handler can enrich the
// ModelObject response with catalog metadata.
// Returns a slice of (virtualName, *VirtualEndpoint) pairs in the same order
// as ListExposedForProject.
func (r *VirtualModelResolver) ListExposedForProjectWithEndpoints(ctx context.Context, projectID string) ([]ExposedEntry, error) {
	names, err := r.ListExposedForProject(ctx, projectID)
	if err != nil || len(names) == 0 {
		return nil, err
	}
	cache, cerr := r.getCache(ctx)
	if cerr != nil {
		return nil, cerr
	}
	out := make([]ExposedEntry, 0, len(names))
	for _, name := range names {
		if vep, ok := cache.byName[name]; ok {
			out = append(out, ExposedEntry{Name: name, VEP: vep})
		}
	}
	return out, nil
}

// ExposedEntry pairs a resolved virtual model name with its VirtualEndpoint.
type ExposedEntry struct {
	Name string
	VEP  *VirtualEndpoint
}

// SplitVirtID splits "virt:<providerID>:<providerModelID>" into three parts.
// Returns nil when the format is unexpected.
// Exported so the proxy handler can extract provider + model IDs for metadata lookups.
func SplitVirtID(id string) []string {
	return splitVirtID(id)
}

func splitVirtID(id string) []string {
	if len(id) < 6 || id[:5] != "virt:" {
		return nil
	}
	rest := id[5:] // "<providerID>:<providerModelID>"
	idx := indexOf(rest, ':')
	if idx < 0 {
		return nil
	}
	return []string{"virt", rest[:idx], rest[idx+1:]}
}

func indexOf(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// Capabilities returns the capability list for a virtual model.
// Returns (nil, false) if the model is not in the exposed catalog.
func (r *VirtualModelResolver) Capabilities(ctx context.Context, modelName string) ([]runtime.Capability, bool) {
	cache, err := r.getCache(ctx)
	if err != nil || cache == nil {
		return nil, false
	}
	ep, ok := cache.byName[modelName]
	if !ok {
		return nil, false
	}
	return capabilitiesFromVirtualEndpoint(ep), true
}

// Invalidate clears the in-memory cache so the next request rebuilds it.
// Call after every catalog sync or rule change.
func (r *VirtualModelResolver) Invalidate() {
	r.mu.Lock()
	r.cache = nil
	r.mu.Unlock()
}

// getCache returns the in-memory cache, building it if stale or absent.
func (r *VirtualModelResolver) getCache(ctx context.Context) (*virtualCache, error) {
	r.mu.RLock()
	c := r.cache
	r.mu.RUnlock()
	if c != nil {
		return c, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Double-check after write lock.
	if r.cache != nil {
		return r.cache, nil
	}

	fresh, err := r.buildCache(ctx)
	if err != nil {
		return nil, err
	}
	r.cache = fresh
	return fresh, nil
}

// buildCache queries providers + catalog + rules and produces the in-memory map.
//
// After migration 050 the authoritative filter is exposure_mode IN ('catalog','hybrid').
// The legacy catalog_direct_expose boolean is kept in sync by a DB trigger so
// existing installations that have not yet run migration 050 still work via the
// fallback WHERE clause.
func (r *VirtualModelResolver) buildCache(ctx context.Context) (*virtualCache, error) {
	// Load all catalog/hybrid-mode providers.
	// We query on exposure_mode first; COALESCE keeps the fallback safe on
	// pre-050 installations where the column does not exist yet.
	type provRow struct {
		ID                           string `db:"id"`
		Name                         string `db:"name"`
		BackendType                  string `db:"backend_type"`
		BaseURL                      string `db:"base_url"`
		APIKey                       string `db:"api_key"`
		ExposureMode                 string `db:"exposure_mode"`
		CatalogExposePrefix          string `db:"catalog_expose_prefix"`
		ProxyURL                     string `db:"proxy_url"`
		TLSInsecureSkipVerify        bool   `db:"tls_insecure_skip_verify"`
		TLSRootCAPEM                 string `db:"tls_root_ca_pem"`
		ConnectTimeoutSeconds        int    `db:"connect_timeout_seconds"`
		ReadTimeoutSeconds           int    `db:"read_timeout_seconds"`
		IdleConnTimeoutSeconds       int    `db:"idle_conn_timeout_seconds"`
		ResponseHeaderTimeoutSeconds int    `db:"response_header_timeout_seconds"`
		MaxIdleConnsPerHost          int    `db:"max_idle_conns_per_host"`
		MaxConnsPerHost              int    `db:"max_conns_per_host"`
		DisableHTTP2                 bool   `db:"disable_http2"`
	}
	var providers []provRow
	if err := r.db.SelectContext(ctx, &providers, `
		SELECT id::text, name, backend_type, base_url, api_key,
		       COALESCE(exposure_mode,'managed') AS exposure_mode,
		       catalog_expose_prefix,
		       COALESCE(proxy_url,'') AS proxy_url,
		       tls_insecure_skip_verify,
		       COALESCE(tls_root_ca_pem,'') AS tls_root_ca_pem,
		       connect_timeout_seconds, read_timeout_seconds,
		       idle_conn_timeout_seconds, response_header_timeout_seconds,
		       max_idle_conns_per_host, max_conns_per_host, disable_http2
		FROM providers
		WHERE enabled=TRUE AND catalog_direct_expose=TRUE`); err != nil {
		// Table may not exist yet (pre-migration 047).
		r.log.Debug("virtual resolver: providers query returned error (pre-migration?)", zap.Error(err))
		return &virtualCache{byName: map[string]*VirtualEndpoint{}, list: nil, builtAt: time.Now()}, nil
	}

	cache := &virtualCache{
		byName:  make(map[string]*VirtualEndpoint),
		list:    nil,
		builtAt: time.Now(),
	}

	for _, prov := range providers {
		prefix := prov.CatalogExposePrefix
		if prefix == "" {
			// Fall back to provider name — ensures virtual model names are always
			// namespaced (e.g. "openrouter/openai/gpt-5", not "/openai/gpt-5").
			prefix = prov.Name
		}

		transport := runtime.ProviderTransportConfig{
			ProxyURL:                     prov.ProxyURL,
			TLSInsecureSkipVerify:        prov.TLSInsecureSkipVerify,
			TLSRootCAPEM:                 prov.TLSRootCAPEM,
			ConnectTimeoutSeconds:        prov.ConnectTimeoutSeconds,
			ReadTimeoutSeconds:           prov.ReadTimeoutSeconds,
			IdleConnTimeoutSeconds:       prov.IdleConnTimeoutSeconds,
			ResponseHeaderTimeoutSeconds: prov.ResponseHeaderTimeoutSeconds,
			MaxIdleConnsPerHost:          prov.MaxIdleConnsPerHost,
			MaxConnsPerHost:              prov.MaxConnsPerHost,
			DisableHTTP2:                 prov.DisableHTTP2,
		}

		// Load catalog entries for this provider.
		type catalogRow struct {
			ProviderModelID   string `db:"provider_model_id"`
			TagsRaw           string `db:"tags_raw"`
			SupportsStreaming bool   `db:"supports_streaming"`
			SupportsTools     bool   `db:"supports_tools"`
			SupportsVision    bool   `db:"supports_vision"`
			SupportsAudio     bool   `db:"supports_audio"`
			SupportsEmbedding bool   `db:"supports_embeddings"`
			SupportsReasoning bool   `db:"supports_reasoning"`
		}
		var entries []catalogRow
		if err := r.db.SelectContext(ctx, &entries, `
			SELECT provider_model_id,
			       COALESCE(array_to_string(tags,','),'') AS tags_raw,
			       supports_streaming, supports_tools, supports_vision,
			       supports_audio, supports_embeddings, supports_reasoning
			FROM provider_remote_models
			WHERE provider_id::text=$1 AND enabled=TRUE`, prov.ID); err != nil {
			r.log.Warn("virtual resolver: failed to load catalog", zap.String("provider", prov.Name), zap.Error(err))
			continue
		}

		rules, err := r.rules.ListForProvider(ctx, prov.ID)
		if err != nil {
			r.log.Warn("virtual resolver: failed to load rules", zap.String("provider", prov.Name), zap.Error(err))
			continue
		}

		for _, e := range entries {
			entry := CatalogEntry{
				ProviderModelID:   e.ProviderModelID,
				Tags:              splitTags(e.TagsRaw),
				SupportsStreaming: e.SupportsStreaming,
				SupportsTools:     e.SupportsTools,
				SupportsVision:    e.SupportsVision,
				SupportsAudio:     e.SupportsAudio,
				SupportsEmbedding: e.SupportsEmbedding,
				SupportsReasoning: e.SupportsReasoning,
			}
			if !r.engine.IsExposed(entry, rules) {
				continue
			}

			virtualName := prefix + "/" + e.ProviderModelID
			vep := &VirtualEndpoint{
				// Include provider ID in the virtual endpoint ID to prevent
				// collisions when two providers expose a model with the same ID.
				ID:                "virt:" + prov.ID + ":" + e.ProviderModelID,
				BackendType:       runtime.BackendType(prov.BackendType),
				UpstreamBaseURL:   prov.BaseURL,
				UpstreamAPIKey:    prov.APIKey,
				UpstreamModelName: e.ProviderModelID,
				Transport:         transport,
				ExposureMode:      ExposureMode(prov.ExposureMode),
				// Capability flags come directly from provider_remote_models.
				// Never inferred from the model name string.
				SupportsStreaming: e.SupportsStreaming,
				SupportsTools:     e.SupportsTools,
				SupportsVision:    e.SupportsVision,
				SupportsAudio:     e.SupportsAudio,
				SupportsEmbedding: e.SupportsEmbedding,
				SupportsReasoning: e.SupportsReasoning,
			}
			cache.byName[virtualName] = vep
			cache.list = append(cache.list, virtualName)
		}
	}

	r.log.Debug("virtual resolver cache built",
		zap.Int("virtual_models", len(cache.list)))
	return cache, nil
}

// capabilitiesFromVirtualEndpoint derives the capability list for a virtual
// (Mode-B catalog) endpoint from the stored DB flags on the VirtualEndpoint.
//
// Architecture rule: capabilities MUST come from metadata stored at sync time,
// never from pattern-matching the model name string. The flags were set when
// the catalog entry was created (via upsertCatalog) and can be updated by
// operators via PUT /admin/v1/providers/:id/models/:model_id.
//
// Mapping:
//
//	SupportsEmbedding=true  → [embedding]           (not a chat model)
//	SupportsAudio=true      → [transcription]        (STT; not a chat model)
//	default (chat model)    → [chat, completion]
//	SupportsVision=true     → append vision to chat caps
//	SupportsReasoning=true  → append reasoning to chat caps
func capabilitiesFromVirtualEndpoint(vep *VirtualEndpoint) []runtime.Capability {
	// Non-chat modalities take exclusive priority — these models do not also
	// serve chat requests through the same endpoint.
	if vep.SupportsEmbedding {
		return []runtime.Capability{runtime.CapabilityEmbedding}
	}
	if vep.SupportsAudio && !vep.SupportsTools {
		// Heuristic: pure audio models have SupportsAudio but not SupportsTools.
		// This distinguishes Whisper-style STT (audio-only) from GPT-4o Audio
		// (chat model that also handles audio, which has SupportsTools=true).
		return []runtime.Capability{runtime.CapabilityTranscription}
	}

	// Chat / completion model — the common case.
	caps := []runtime.Capability{runtime.CapabilityChat, runtime.CapabilityCompletion}
	if vep.SupportsVision {
		caps = append(caps, runtime.CapabilityVision)
	}
	return caps
}
