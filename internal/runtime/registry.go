package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

const (
	redisEndpointHealth = "nexus:ep:%s:health"  // key: endpoint_id
	redisModelPool      = "nexus:model:%s:pool" // key: model_name → JSON pool snapshot
	poolCacheTTL        = 30 * time.Second
)

// RegistryEndpoint is the DB-backed representation of a model endpoint row.
type RegistryEndpoint struct {
	ID                  string       `db:"id"`
	ModelID             string       `db:"model_id"`
	ModelName           string       `db:"model_name"`
	BackendType         BackendType  `db:"backend_type"`
	Host                string       `db:"host"`
	Port                int          `db:"port"`
	BasePath            string       `db:"base_path"`
	Weight              int          `db:"weight"`
	Priority            int          `db:"priority"`
	HealthStatus        HealthStatus `db:"health_status"`
	IsEnabled           bool         `db:"is_enabled"`
	ConsecutiveFailures int          `db:"consecutive_failures"`
	// Cloud / external model credentials. NULL for local models.
	UpstreamAPIKey    string `db:"upstream_api_key"`
	UpstreamBaseURL   string `db:"upstream_base_url"`
	UpstreamProxy     string `db:"upstream_proxy"`
	UpstreamModelName string `db:"upstream_model_name"`
	// Per-endpoint provider transport configuration (migration 046).
	// NULL / zero = use production defaults (see DefaultProviderTransportConfig).
	// Only populated for provider backends; zero for local backends.
	ProviderProxyURL                 string `db:"provider_proxy_url"`
	ProviderTLSInsecureSkipVerify    bool   `db:"provider_tls_insecure_skip_verify"`
	ProviderTLSRootCAPEM             string `db:"provider_tls_root_ca_pem"`
	ProviderConnectTimeoutSeconds    int    `db:"provider_connect_timeout_seconds"`
	ProviderReadTimeoutSeconds       int    `db:"provider_read_timeout_seconds"`
	ProviderIdleConnTimeoutSeconds   int    `db:"provider_idle_conn_timeout_seconds"`
	ProviderResponseHeaderTimeout    int    `db:"provider_response_header_timeout_seconds"`
	ProviderMaxIdleConnsPerHost      int    `db:"provider_max_idle_conns_per_host"`
	ProviderMaxConnsPerHost          int    `db:"provider_max_conns_per_host"`
	ProviderDisableHTTP2             bool   `db:"provider_disable_http2"`
}

// transportConfig builds a ProviderTransportConfig from the DB-loaded fields.
// For local models all DB fields are zero, which makes withDefaults() apply
// production-grade defaults — the resulting client is used only by provider
// backends. For local backends the registry uses factory.ClientFor() instead.
func (r *RegistryEndpoint) transportConfig() ProviderTransportConfig {
	return ProviderTransportConfig{
		// Prefer the dedicated provider_proxy_url column; fall back to the
		// legacy upstream_proxy field for backward compatibility.
		ProxyURL: func() string {
			if r.ProviderProxyURL != "" {
				return r.ProviderProxyURL
			}
			return r.UpstreamProxy
		}(),
		TLSInsecureSkipVerify:        r.ProviderTLSInsecureSkipVerify,
		TLSRootCAPEM:                 r.ProviderTLSRootCAPEM,
		ConnectTimeoutSeconds:        r.ProviderConnectTimeoutSeconds,
		ReadTimeoutSeconds:           r.ProviderReadTimeoutSeconds,
		IdleConnTimeoutSeconds:       r.ProviderIdleConnTimeoutSeconds,
		ResponseHeaderTimeoutSeconds: r.ProviderResponseHeaderTimeout,
		MaxIdleConnsPerHost:          r.ProviderMaxIdleConnsPerHost,
		MaxConnsPerHost:              r.ProviderMaxConnsPerHost,
		DisableHTTP2:                 r.ProviderDisableHTTP2,
	}
}

// URL builds the base URL from host and port (no base_path).
// Backends append their own paths (e.g. /v1/chat/completions).
func (r *RegistryEndpoint) URL() string {
	return fmt.Sprintf("http://%s:%d", r.Host, r.Port)
}

// Registry is the in-process runtime catalogue.
// It holds a Pool per model, keeps pools in sync with PostgreSQL,
// and caches health state in Redis for the gateway's hot path.
type Registry struct {
	db      *sqlx.DB
	rdb     *redis.Client
	factory *Factory
	log     *zap.Logger

	mu        sync.RWMutex
	pools     map[string]*Pool   // model name → pool
	bends     map[string]Backend // backend type → backend instance (shared HTTP client)
	epClients map[string]*http.Client // endpoint ID → dedicated per-endpoint HTTP client
	// epClients is only populated for provider backends (IsProviderBackend).
	// Each client is built once from the endpoint's ProviderTransportConfig
	// and reused for every request. Local backends use factory.ClientFor().
}

// NewRegistry constructs and populates a Registry from the database.
func NewRegistry(db *sqlx.DB, rdb *redis.Client, factory *Factory, log *zap.Logger) (*Registry, error) {
	r := &Registry{
		db:        db,
		rdb:       rdb,
		factory:   factory,
		log:       log,
		pools:     make(map[string]*Pool),
		bends:     make(map[string]Backend),
		epClients: make(map[string]*http.Client),
	}
	if err := r.Reload(context.Background()); err != nil {
		return nil, fmt.Errorf("initial registry load: %w", err)
	}
	return r, nil
}

// NewEmptyRegistry constructs a Registry with no endpoints loaded.
// Used when the DB schema is not yet initialised; the registry will
// populate itself once Reload is called successfully.
func NewEmptyRegistry(db *sqlx.DB, rdb *redis.Client, factory *Factory, log *zap.Logger) (*Registry, error) {
	return &Registry{
		db:        db,
		rdb:       rdb,
		factory:   factory,
		log:       log,
		pools:     make(map[string]*Pool),
		bends:     make(map[string]Backend),
		epClients: make(map[string]*http.Client),
	}, nil
}

// Reload re-reads all enabled endpoints from PostgreSQL and rebuilds every Pool.
// Safe to call concurrently — uses a write lock only at the swap point.
func (r *Registry) Reload(ctx context.Context) error {
	rows, err := r.loadEndpoints(ctx)
	if err != nil {
		return err
	}

	newPools     := make(map[string]*Pool, len(rows))
	newBends     := make(map[string]Backend)
	newEpClients := make(map[string]*http.Client, len(rows))

	// Carry over existing backends — avoids rebuilding shared HTTP clients
	// for local backends on every reload.
	r.mu.RLock()
	for k, v := range r.bends {
		newBends[k] = v
	}
	r.mu.RUnlock()

	for _, row := range rows {
		if _, ok := newPools[row.ModelName]; !ok {
			newPools[row.ModelName] = NewPool(row.ModelID, StrategyRoundRobin)
		}
		pool := newPools[row.ModelName]

		transportCfg := row.transportConfig()

		// Provider backends never load as StatusDown from the DB — a stale
		// "down" status from previous health probe failures would make the
		// endpoint unavailable and trigger the activator, which cannot start
		// a remote API. Remote APIs are always considered routable; the request
		// itself will fail if the API is truly unreachable.
		loadedStatus := row.HealthStatus
		if IsProviderBackend(row.BackendType) && loadedStatus == StatusDown {
			loadedStatus = StatusDegraded
		}

		ep := &Endpoint{
			ID:                row.ID,
			ModelID:           row.ModelID,
			BackendType:       row.BackendType,
			URL:               row.URL(),
			Weight:            row.Weight,
			Priority:          row.Priority,
			Status:            loadedStatus,
			UpstreamAPIKey:    row.UpstreamAPIKey,
			UpstreamBaseURL:   row.UpstreamBaseURL,
			UpstreamProxy:     row.UpstreamProxy,
			UpstreamModelName: row.UpstreamModelName,
			Transport:         transportCfg,
		}
		pool.Add(ep)

		// Build a dedicated HTTP client for every provider endpoint.
		// Local backends share the factory's single HTTP client.
		if IsProviderBackend(row.BackendType) {
			client, buildErr := BuildProviderClient(transportCfg)
			if buildErr != nil {
				// Log and fall back to the factory's direct client.
				// The endpoint is still routable; only the transport config is degraded.
				r.log.Warn("failed to build provider transport — using default client",
					zap.String("endpoint_id", row.ID),
					zap.String("model", row.ModelName),
					zap.Error(buildErr),
				)
				client = r.factory.DirectClient()
			}
			newEpClients[row.ID] = client
		}

		// Ensure we have a backend instance for this type.
		key := string(row.BackendType)
		if _, ok := newBends[key]; !ok {
			b, buildErr := r.factory.Build(row.BackendType)
			if buildErr != nil {
				r.log.Warn("unknown backend type, falling back to openai_compat",
					zap.String("type", key), zap.Error(buildErr))
				b = r.factory.MustBuild(BackendOpenAICompat)
			}
			newBends[key] = b
		}
	}

	r.mu.Lock()
	r.pools     = newPools
	r.bends     = newBends
	r.epClients = newEpClients
	r.mu.Unlock()

	r.log.Info("registry reloaded",
		zap.Int("models", len(newPools)),
		zap.Int("endpoints", len(rows)),
		zap.Int("provider_clients", len(newEpClients)),
	)
	return nil
}

// Resolve picks a healthy endpoint for the given model name and returns it
// together with the Backend implementation that can serve it.
// It performs automatic failover: if the first pick is unhealthy when the
// request arrives, it retries up to maxRetries times.
func (r *Registry) Resolve(modelName string) (*Endpoint, Backend, error) {
	r.mu.RLock()
	pool, ok := r.pools[modelName]
	r.mu.RUnlock()

	if !ok {
		return nil, nil, fmt.Errorf("model %q not found in registry", modelName)
	}

	ep, err := pool.Pick()
	if err != nil {
		return nil, nil, err
	}

	backend, err := r.BackendForEndpoint(ep)
	if err != nil {
		return nil, nil, err
	}
	return ep, backend, nil
}

// ResolveWithFailover tries up to maxAttempts different endpoints, skipping
// any that were reported unhealthy by the watcher.
func (r *Registry) ResolveWithFailover(modelName string, maxAttempts int) (*Endpoint, Backend, error) {
	r.mu.RLock()
	pool, ok := r.pools[modelName]
	r.mu.RUnlock()
	if !ok {
		return nil, nil, fmt.Errorf("model %q not found in registry", modelName)
	}

	tried := make(map[string]bool)
	for i := 0; i < maxAttempts; i++ {
		ep, err := pool.Pick()
		if err != nil {
			break
		}
		if tried[ep.ID] {
			continue
		}
		tried[ep.ID] = true
		if ep.IsAvailable() {
			b, err := r.BackendForEndpoint(ep)
			if err != nil {
				continue
			}
			return ep, b, nil
		}
	}
	return nil, nil, fmt.Errorf("model %q: no healthy endpoint after %d attempts", modelName, maxAttempts)
}

// UpdateEndpointHealth updates both the in-memory pool and Redis cache.
func (r *Registry) UpdateEndpointHealth(ctx context.Context, epID, modelName string, h EndpointHealth) {
	r.mu.RLock()
	pool, ok := r.pools[modelName]
	r.mu.RUnlock()
	if !ok {
		return
	}
	for _, ep := range pool.Endpoints() {
		if ep.ID == epID {
			ep.SetStatus(h.Status)
			break
		}
	}

	// Write to Redis for the gateway hot path.
	key := fmt.Sprintf(redisEndpointHealth, epID)
	_ = r.rdb.Set(ctx, key, string(h.Status), poolCacheTTL).Err()
}

// RemoveEndpoint removes an endpoint from the pool by ID.
// Called when the watcher permanently disables an endpoint in DB.
func (r *Registry) RemoveEndpoint(modelName, epID string) {
	r.mu.RLock()
	pool, ok := r.pools[modelName]
	r.mu.RUnlock()
	if !ok {
		return
	}
	pool.Remove(epID)
	r.log.Info("endpoint removed from pool",
		zap.String("model", modelName),
		zap.String("endpoint_id", epID),
	)
}
func (r *Registry) SetPoolStrategy(modelName string, strategy RoutingStrategy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	pool, ok := r.pools[modelName]
	if !ok {
		return fmt.Errorf("model %q not found", modelName)
	}
	pool.Strategy = strategy
	return nil
}

// BackendForType returns a Backend instance for the given type string.
func (r *Registry) BackendForType(backendType string) Backend {
	r.mu.RLock()
	b, ok := r.bends[backendType]
	r.mu.RUnlock()
	if ok {
		return b
	}
	built, err := r.factory.Build(BackendType(backendType))
	if err != nil {
		built = r.factory.MustBuild(BackendOpenAICompat)
	}
	r.mu.Lock()
	// Re-check under write lock to avoid double-write from concurrent callers.
	if existing, ok := r.bends[backendType]; ok {
		r.mu.Unlock()
		return existing
	}
	r.bends[backendType] = built
	r.mu.Unlock()
	return built
}

// ListModels returns all model names currently in the registry.
func (r *Registry) ListModels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.pools))
	for name := range r.pools {
		names = append(names, name)
	}
	return names
}

// ─── private ──────────────────────────────────────────────────────────────────

func (r *Registry) loadEndpoints(ctx context.Context) ([]RegistryEndpoint, error) {
	var rows []RegistryEndpoint
	// Load endpoints from both sources:
	//   1. model_endpoints (the original/primary row — always included when enabled)
	//   2. agent_runtimes (one row per active replica — may differ in host:port)
	//
	// Rule: if an agent_runtimes row has a non-empty bind_host and bind_port,
	// it overrides the model_endpoints host:port for that specific runtime.
	// This supports N replicas on different hosts/ports while keeping the
	// same model_endpoints row for backward compat.
	err := r.db.SelectContext(ctx, &rows, `
		-- Primary endpoints from model_endpoints (backward-compat single-replica path).
		-- Only included when no agent_runtime has taken over this endpoint.
		SELECT
		    me.id,
		    me.model_id,
		    m.name          AS model_name,
		    m.backend_type,
		    me.host,
		    me.port,
		    me.base_path,
		    me.weight,
		    me.priority,
		    me.health_status,
		    me.is_enabled,
		    me.consecutive_failures,
		    COALESCE(me.upstream_api_key, '')    AS upstream_api_key,
		    COALESCE(me.upstream_base_url, '')   AS upstream_base_url,
		    COALESCE(me.upstream_proxy, '')      AS upstream_proxy,
		    COALESCE(me.upstream_model_name, '') AS upstream_model_name,
		    -- Provider transport config (migration 046). COALESCE to zero values
		    -- for pre-046 installs — BuildProviderClient() applies safe defaults.
		    COALESCE(me.provider_proxy_url, '')                    AS provider_proxy_url,
		    COALESCE(me.provider_tls_insecure_skip_verify, FALSE)  AS provider_tls_insecure_skip_verify,
		    COALESCE(me.provider_tls_root_ca_pem, '')              AS provider_tls_root_ca_pem,
		    COALESCE(me.provider_connect_timeout_seconds, 0)       AS provider_connect_timeout_seconds,
		    COALESCE(me.provider_read_timeout_seconds, 0)          AS provider_read_timeout_seconds,
		    COALESCE(me.provider_idle_conn_timeout_seconds, 0)     AS provider_idle_conn_timeout_seconds,
		    COALESCE(me.provider_response_header_timeout_seconds, 0) AS provider_response_header_timeout_seconds,
		    COALESCE(me.provider_max_idle_conns_per_host, 0)       AS provider_max_idle_conns_per_host,
		    COALESCE(me.provider_max_conns_per_host, 0)            AS provider_max_conns_per_host,
		    COALESCE(me.provider_disable_http2, FALSE)             AS provider_disable_http2
		FROM model_endpoints me
		JOIN models m ON m.id = me.model_id
		WHERE me.is_enabled = TRUE
		  AND m.enabled = TRUE
		  AND (
		      NOT EXISTS (
		          SELECT 1 FROM agent_runtimes ar
		          WHERE ar.endpoint_id = me.id
		            AND ar.state IN ('ready','active','warm','idle','loading_model','waiting_ready')
		            AND ar.bind_port > 0
		      )
		  )

		UNION ALL

		-- Runtime-level endpoints: one row per agent_runtime replica.
		-- Local backends only — provider backends never create agent_runtime rows.
		SELECT
		    ar.id                                    AS id,
		    ar.model_id,
		    m.name                                   AS model_name,
		    m.backend_type,
		    ar.bind_host                             AS host,
		    ar.bind_port                             AS port,
		    '/v1'                                    AS base_path,
		    100                                      AS weight,
		    COALESCE(ar.replica_index, 0) + 1        AS priority,
		    CASE ar.state
		        WHEN 'ready'         THEN 'healthy'::text
		        WHEN 'active'        THEN 'healthy'::text
		        WHEN 'warm'          THEN 'healthy'::text
		        WHEN 'idle'          THEN 'healthy'::text
		        WHEN 'loading_model' THEN 'down'::text
		        WHEN 'waiting_ready' THEN 'down'::text
		        ELSE                      'down'::text
		    END                                      AS health_status,
		    TRUE                                     AS is_enabled,
		    0                                        AS consecutive_failures,
		    ''                                       AS upstream_api_key,
		    ''                                       AS upstream_base_url,
		    ''                                       AS upstream_proxy,
		    ''                                       AS upstream_model_name,
		    -- Transport config: local runtimes never use proxy/TLS settings
		    '' AS provider_proxy_url,
		    FALSE AS provider_tls_insecure_skip_verify,
		    '' AS provider_tls_root_ca_pem,
		    0 AS provider_connect_timeout_seconds,
		    0 AS provider_read_timeout_seconds,
		    0 AS provider_idle_conn_timeout_seconds,
		    0 AS provider_response_header_timeout_seconds,
		    0 AS provider_max_idle_conns_per_host,
		    0 AS provider_max_conns_per_host,
		    FALSE AS provider_disable_http2
		FROM agent_runtimes ar
		JOIN models m ON m.id = ar.model_id
		WHERE ar.state IN ('ready','active','warm','idle','loading_model','waiting_ready')
		  AND ar.bind_host != ''
		  AND ar.bind_port > 0
		  AND m.enabled = TRUE

		ORDER BY model_name, priority, weight DESC
	`)
	return rows, err
}

// StartPeriodicReload starts a background goroutine that reloads the registry
// every interval. This ensures HA replicas started by the reconciler are
// picked up even without an explicit enableEndpoint() call from the activator.
// Blocks until ctx is cancelled.
func (r *Registry) StartPeriodicReload(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.Reload(ctx); err != nil {
				r.log.Warn("periodic registry reload failed", zap.Error(err))
			}
		}
	}
}

// GetModelCapabilities returns the capabilities declared for a model.
// It queries the models.capabilities JSONB column and falls back to
// DefaultCapabilities(service_type) for models that pre-date migration 033
// (capabilities column is an empty array).
//
// Returns (nil, false) when the model is not found in the database at all.
// A model that exists but has no capabilities returns ([]Capability{}, true)
// so callers can distinguish "model unknown" from "model has no capabilities".
func (r *Registry) GetModelCapabilities(ctx context.Context, modelName string) ([]Capability, bool) {
	var row struct {
		Capabilities string `db:"capabilities"`
		ServiceType  string `db:"service_type"`
	}
	err := r.db.GetContext(ctx, &row, `
		SELECT COALESCE(capabilities::text, '[]') AS capabilities,
		       COALESCE(service_type, '')          AS service_type
		FROM models
		WHERE name = $1 AND enabled = TRUE
		LIMIT 1`, modelName)
	if err != nil {
		// Model not found or DB error — return not-found so the caller lets
		// the downstream pipeline handle the missing-model case.
		return nil, false
	}

	// Decode the JSONB capability list.
	if row.Capabilities != "" && row.Capabilities != "null" && row.Capabilities != "[]" {
		var capStrs []string
		if jsonErr := jsonDecodeStrings([]byte(row.Capabilities), &capStrs); jsonErr == nil && len(capStrs) > 0 {
			caps := make([]Capability, len(capStrs))
			for i, s := range capStrs {
				caps[i] = Capability(s)
			}
			return caps, true
		}
	}

	// Empty capabilities column — fall back to service_type derivation.
	// This keeps existing models working without requiring an explicit
	// capabilities backfill beyond what migration 033 already ran.
	if row.ServiceType != "" {
		return DefaultCapabilities(row.ServiceType), true
	}

	return []Capability{}, true
}

// IsRemoteModel reports whether a model name maps to an external/cloud provider
// backend by querying the models table. This is the authoritative gate that
// the proxy handler uses to decide whether to skip the runtime activator.
//
// Decision rules (all based on BackendType metadata — never on the model name):
//   - BackendType is one of the provider backends → true  (skip activator)
//   - BackendType is a local runtime (vllm, tgi, llamacpp, …) → false
//   - Model not found in DB → false (let the activator / 503 path handle it)
//
// The method uses the in-memory pool as a fast path: if the model is already
// loaded in the registry we can inspect the endpoint's BackendType without a DB
// round-trip. Only on a registry miss do we fall back to a direct DB query so
// that newly created models (not yet reloaded) are handled correctly.
func (r *Registry) IsRemoteModel(ctx context.Context, modelName string) bool {
	// Fast path: check the in-memory pool first.
	r.mu.RLock()
	pool, ok := r.pools[modelName]
	r.mu.RUnlock()
	if ok {
		// Pool exists — peek at the first endpoint's BackendType.
		if ep, err := pool.Pick(); err == nil {
			return IsProviderBackend(ep.BackendType)
		}
	}

	// Slow path: model not in pool (might be in DB but not yet reloaded, or
	// simply does not exist). Query the models table directly.
	var backendType BackendType
	err := r.db.GetContext(ctx, &backendType,
		`SELECT backend_type FROM models WHERE name=$1 AND enabled=TRUE LIMIT 1`,
		modelName)
	if err != nil {
		// Not found or DB error — treat as local so the activator path runs.
		return false
	}
	return IsProviderBackend(backendType)
}

// jsonDecodeStrings decodes a JSON byte slice into a string slice.
func jsonDecodeStrings(data []byte, out *[]string) error {
	return json.Unmarshal(data, out)
}

// ClientForEndpoint returns the dedicated *http.Client for a provider endpoint.
//
// For provider backends (IsProviderBackend) this returns the per-endpoint client
// built from the endpoint's ProviderTransportConfig at Reload() time. The client
// has a dedicated transport configured with the endpoint's proxy, TLS settings,
// timeouts, and connection pool — completely isolated from all other endpoints.
//
// For local backends this falls back to the factory's shared direct client,
// which is correct because local endpoints never use proxy or custom TLS.
//
// The returned client is safe for concurrent use and must never be closed by
// the caller. Its lifetime is tied to the Registry.
func (r *Registry) ClientForEndpoint(ep *Endpoint) *http.Client {
	if IsProviderBackend(ep.BackendType) {
		r.mu.RLock()
		c, ok := r.epClients[ep.ID]
		r.mu.RUnlock()
		if ok {
			return c
		}
		// Endpoint was added after the last Reload (rare). Build a client on
		// demand from the transport config stored in the endpoint struct.
		client, err := BuildProviderClient(ep.Transport)
		if err != nil {
			return r.factory.DirectClient()
		}
		r.mu.Lock()
		// Double-check: another goroutine may have inserted it concurrently.
		if existing, ok2 := r.epClients[ep.ID]; ok2 {
			r.mu.Unlock()
			return existing
		}
		r.epClients[ep.ID] = client
		r.mu.Unlock()
		return client
	}
	// Local backend: use the factory's proxy-aware client (supports
	// NEXUS_UPSTREAM_PROXY and per-endpoint upstream_proxy for legacy compat).
	return r.factory.ClientFor(ep.UpstreamProxy)
}

// CacheVirtualClient stores an *http.Client for a virtual endpoint ID so that
// ClientForEndpoint can return it without rebuilding. Called by the proxy
// handler after resolving a Mode-B virtual model.
func (r *Registry) CacheVirtualClient(endpointID string, client *http.Client) {
	r.mu.Lock()
	r.epClients[endpointID] = client
	r.mu.Unlock()
}

// BackendForEndpoint returns a Backend instance for the given endpoint.
func (r *Registry) BackendForEndpoint(ep *Endpoint) (Backend, error) {
	r.mu.RLock()
	b, ok := r.bends[string(ep.BackendType)]
	r.mu.RUnlock()
	if ok {
		return b, nil
	}
	built, err := r.factory.Build(ep.BackendType)
	if err != nil {
		r.log.Warn("unknown backend type in endpoint, falling back to openai_compat",
			zap.String("backend_type", string(ep.BackendType)),
			zap.String("endpoint_id", ep.ID),
		)
		built = r.factory.MustBuild(BackendOpenAICompat)
	}
	r.mu.Lock()
	// Re-check under write lock.
	if existing, ok := r.bends[string(ep.BackendType)]; ok {
		r.mu.Unlock()
		return existing, nil
	}
	r.bends[string(ep.BackendType)] = built
	r.mu.Unlock()
	return built, nil
}
