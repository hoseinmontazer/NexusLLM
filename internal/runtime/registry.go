package runtime

import (
	"context"
	"fmt"
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

	mu    sync.RWMutex
	pools map[string]*Pool   // model name → pool
	bends map[string]Backend // backend type → backend instance (shared HTTP client)
}

// NewRegistry constructs and populates a Registry from the database.
func NewRegistry(db *sqlx.DB, rdb *redis.Client, factory *Factory, log *zap.Logger) (*Registry, error) {
	r := &Registry{
		db:      db,
		rdb:     rdb,
		factory: factory,
		log:     log,
		pools:   make(map[string]*Pool),
		bends:   make(map[string]Backend),
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
		db:      db,
		rdb:     rdb,
		factory: factory,
		log:     log,
		pools:   make(map[string]*Pool),
		bends:   make(map[string]Backend),
	}, nil
}

// Reload re-reads all enabled endpoints from PostgreSQL and rebuilds every Pool.
// Safe to call concurrently — uses a write lock only at the swap point.
func (r *Registry) Reload(ctx context.Context) error {
	rows, err := r.loadEndpoints(ctx)
	if err != nil {
		return err
	}

	newPools := make(map[string]*Pool, len(rows))
	for _, row := range rows {
		if _, ok := newPools[row.ModelName]; !ok {
			newPools[row.ModelName] = NewPool(row.ModelID, StrategyRoundRobin)
		}
		pool := newPools[row.ModelName]

		ep := &Endpoint{
			ID:          row.ID,
			ModelID:     row.ModelID,
			BackendType: row.BackendType,
			URL:         row.URL(),
			Weight:      row.Weight,
			Priority:    row.Priority,
			Status:      row.HealthStatus,
		}
		pool.Add(ep)

		// Ensure we have a backend instance for this type.
		key := string(row.BackendType)
		if _, ok := r.bends[key]; !ok {
			b, err := r.factory.Build(row.BackendType)
			if err != nil {
				r.log.Warn("unknown backend type, falling back to vllm",
					zap.String("type", key), zap.Error(err))
				b = r.factory.MustBuild(BackendVLLM)
			}
			r.bends[key] = b
		}
	}

	r.mu.Lock()
	r.pools = newPools
	r.mu.Unlock()

	r.log.Info("registry reloaded",
		zap.Int("models", len(newPools)),
		zap.Int("endpoints", len(rows)),
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
		-- Primary endpoints from model_endpoints (backward-compat single-replica path)
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
		    me.consecutive_failures
		FROM model_endpoints me
		JOIN models m ON m.id = me.model_id
		WHERE me.is_enabled = TRUE
		  AND m.enabled = TRUE
		  -- Only include if no active agent_runtime exists for this model (single-replica case)
		  -- OR if the runtime row's endpoint_id matches (runtime registered its endpoint)
		  AND (
		      NOT EXISTS (
		          SELECT 1 FROM agent_runtimes ar
		          WHERE ar.model_id = me.model_id
		            AND ar.state IN ('ready','active','warm','idle','loading_model','waiting_ready')
		            AND ar.bind_port > 0
		      )
		      OR me.id IN (
		          SELECT ar.endpoint_id FROM agent_runtimes ar
		          WHERE ar.model_id = me.model_id
		            AND ar.state IN ('ready','active','warm','idle','loading_model','waiting_ready')
		            AND ar.endpoint_id IS NOT NULL
		      )
		  )

		UNION ALL

		-- Runtime-level endpoints: one per agent_runtime replica.
		-- Includes both healthy (ready/active/warm/idle) and still-starting
		-- (loading_model/waiting_ready) runtimes so the health watcher can
		-- probe them and transition them to healthy as soon as they are ready.
		-- This is the critical path for HA failover: reconciler-created runtimes
		-- always have endpoint_id=NULL and only appear via this branch. They
		-- must enter the pool while starting so the watcher can promote them
		-- to healthy — without this they would be stuck in loading_model forever
		-- because the watcher only checks endpoints already in the pool.
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
		        WHEN 'loading_model' THEN 'unknown'::text
		        WHEN 'waiting_ready' THEN 'unknown'::text
		        ELSE 'unknown'::text
		    END                                      AS health_status,
		    TRUE                                     AS is_enabled,
		    0                                        AS consecutive_failures
		FROM agent_runtimes ar
		JOIN models m ON m.id = ar.model_id
		WHERE ar.state IN ('ready','active','warm','idle','loading_model','waiting_ready')
		  AND ar.bind_host != ''
		  AND ar.bind_port > 0
		  AND m.enabled = TRUE
		  -- Exclude runtimes already covered by model_endpoints UNION above
		  AND (ar.endpoint_id IS NULL OR NOT EXISTS (
		      SELECT 1 FROM model_endpoints me2
		      WHERE me2.id = ar.endpoint_id AND me2.is_enabled = TRUE
		  ))

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
func (r *Registry) BackendForEndpoint(ep *Endpoint) (Backend, error) {
	r.mu.RLock()
	b, ok := r.bends[string(ep.BackendType)]
	r.mu.RUnlock()
	if ok {
		return b, nil
	}
	// BackendType not yet registered — build and cache it
	built, err := r.factory.Build(ep.BackendType)
	if err != nil {
		// Unknown backend type — fall back to openai_compat which is the most
		// permissive (attempts /v1/models for health, accepts any JSON response)
		r.log.Warn("unknown backend type in endpoint, falling back to openai_compat",
			zap.String("backend_type", string(ep.BackendType)),
			zap.String("endpoint_id", ep.ID),
		)
		built = r.factory.MustBuild(BackendOpenAICompat)
	}
	r.mu.Lock()
	r.bends[string(ep.BackendType)] = built
	r.mu.Unlock()
	return built, nil
}

func (r *Registry) backendForEndpoint(modelID string) (Backend, error) {
	// Legacy path used by proxy handler — finds a backend by scanning pools.
	// The proxy resolves an *Endpoint first, so it should call BackendForEndpoint
	// directly. This method is kept for the Resolve/ResolveWithFailover path.
	r.mu.RLock()
	defer r.mu.RUnlock()
	// Scan all pools to find the endpoint's backend type
	for _, pool := range r.pools {
		for _, ep := range pool.Endpoints() {
			if ep.ModelID == modelID {
				if b, ok := r.bends[string(ep.BackendType)]; ok {
					return b, nil
				}
			}
		}
	}
	// Fallback: return any registered backend (should not happen after Reload)
	for _, b := range r.bends {
		return b, nil
	}
	return r.factory.Build(BackendVLLM)
}
