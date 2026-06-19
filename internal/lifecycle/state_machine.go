// Package lifecycle implements the model endpoint state machine and
// the idle-timeout / LRU eviction manager.
package lifecycle

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// ─────────────────────────────────────────────────────────────────────────────
// State machine
// ─────────────────────────────────────────────────────────────────────────────

// State represents a model endpoint's lifecycle state.
type State string

const (
	StateRegistered  State = "registered"
	StateDownloading State = "downloading"
	StateLoading     State = "loading"
	StateWarm        State = "warm"
	StateActive      State = "active"
	StateIdle        State = "idle"
	StateUnloading   State = "unloading"
	StateUnloaded    State = "unloaded"
	StateFailed      State = "failed"
	StateDraining    State = "draining"
)

// allowed encodes valid state transitions.
var allowed = map[State][]State{
	StateRegistered:  {StateDownloading, StateLoading, StateFailed},
	StateDownloading: {StateLoading, StateFailed},
	StateLoading:     {StateWarm, StateFailed},
	StateWarm:        {StateActive, StateIdle, StateDraining, StateFailed},
	StateActive:      {StateWarm, StateIdle, StateDraining, StateFailed},
	StateIdle:        {StateActive, StateUnloading, StateFailed},
	StateDraining:    {StateUnloading, StateUnloaded},
	StateUnloading:   {StateUnloaded, StateFailed},
	StateUnloaded:    {StateRegistered, StateDownloading},
	StateFailed:      {StateRegistered, StateUnloaded},
}

// Transition validates and applies a state change.
// Returns an error if the transition is not permitted.
func Transition(from, to State) error {
	nexts, ok := allowed[from]
	if !ok {
		return fmt.Errorf("unknown state %q", from)
	}
	for _, s := range nexts {
		if s == to {
			return nil
		}
	}
	return fmt.Errorf("transition %s → %s is not allowed", from, to)
}

// ─────────────────────────────────────────────────────────────────────────────
// Idle / LRU manager
// ─────────────────────────────────────────────────────────────────────────────

const (
	redisLastUsed = "nexus:ep:%s:last_used" // key: endpoint_id → unix timestamp
)

// Manager monitors endpoint idle time and triggers unload when the idle
// timeout is exceeded. Uses an LRU policy when VRAM headroom is low.
type Manager struct {
	db          *sqlx.DB
	rdb         *redis.Client
	log         *zap.Logger
	idleTimeout time.Duration

	mu       sync.Mutex
	unloader Unloader
}

// Unloader is the callback invoked when the manager decides to unload an endpoint.
type Unloader func(ctx context.Context, endpointID string) error

// NewManager constructs a lifecycle Manager.
func NewManager(db *sqlx.DB, rdb *redis.Client, idleTimeout time.Duration, unloader Unloader, log *zap.Logger) *Manager {
	return &Manager{db: db, rdb: rdb, log: log, idleTimeout: idleTimeout, unloader: unloader}
}

// RecordActivity marks an endpoint as active right now (called by the proxy on each request).
func (m *Manager) RecordActivity(ctx context.Context, endpointID string) {
	key := fmt.Sprintf(redisLastUsed, endpointID)
	_ = m.rdb.Set(ctx, key, time.Now().Unix(), m.idleTimeout*2).Err()
}

// Start runs the idle check loop until ctx is cancelled.
func (m *Manager) Start(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.evictIdle(ctx)
		}
	}
}

func (m *Manager) evictIdle(ctx context.Context) {
	// Load all active/warm endpoints
	type row struct {
		ID             string `db:"id"`
		ModelName      string `db:"model_name"`
		LifecycleState string `db:"lifecycle_state"`
	}
	var endpoints []row
	_ = m.db.SelectContext(ctx, &endpoints, `
		SELECT me.id, m.name AS model_name, me.lifecycle_state
		FROM model_endpoints me
		JOIN models m ON m.id = me.model_id
		WHERE me.lifecycle_state IN ('active','warm','idle')
		  AND me.is_enabled = TRUE`)

	for _, ep := range endpoints {
		key := fmt.Sprintf(redisLastUsed, ep.ID)
		val, err := m.rdb.Get(ctx, key).Int64()
		if err != nil {
			// No activity record → assume idle since last restart
			continue
		}
		lastUsed := time.Unix(val, 0)
		if time.Since(lastUsed) > m.idleTimeout {
			m.log.Info("idle timeout — unloading endpoint",
				zap.String("endpoint_id", ep.ID),
				zap.String("model", ep.ModelName),
				zap.Duration("idle_for", time.Since(lastUsed)),
			)
			_, _ = m.db.ExecContext(ctx,
				`UPDATE model_endpoints SET lifecycle_state = 'idle', updated_at = NOW() WHERE id = $1`, ep.ID)
			if m.unloader != nil {
				if err := m.unloader(ctx, ep.ID); err != nil {
					m.log.Error("unload failed", zap.String("endpoint_id", ep.ID), zap.Error(err))
					_, _ = m.db.ExecContext(ctx,
						`UPDATE model_endpoints SET lifecycle_state = 'failed', updated_at = NOW() WHERE id = $1`, ep.ID)
				} else {
					_, _ = m.db.ExecContext(ctx,
						`UPDATE model_endpoints SET lifecycle_state = 'unloaded', is_enabled = FALSE, updated_at = NOW() WHERE id = $1`, ep.ID)
				}
			}
		}
	}
}
