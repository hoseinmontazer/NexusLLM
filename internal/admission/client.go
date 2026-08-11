// Package admission provides the atomic Redis-based admission gate for
// NexusLLM inference requests.
//
// All admission state lives on a dedicated standalone Redis instance
// (redis-admission) separate from the general caching Redis. This avoids
// Redis Cluster cross-slot violations and allows independent tuning.
//
// Memory policy requirement: maxmemory-policy noeviction
package admission

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Config holds connection parameters for the admission Redis instance.
type Config struct {
	// Addr is the Redis address, e.g. "localhost:6380"
	Addr string
	// Password is the Redis auth password (empty = no auth)
	Password string
	// DB is the Redis database number (typically 0)
	DB int
	// DialTimeout is the connection timeout (default: 5s)
	DialTimeout time.Duration
	// ReadTimeout is the read timeout (default: 3s)
	ReadTimeout time.Duration
	// WriteTimeout is the write timeout (default: 3s)
	WriteTimeout time.Duration
	// PoolSize is the number of connections in the pool (default: 10)
	PoolSize int
}

// NewClient constructs a dedicated Redis client for the admission subsystem.
// This MUST connect to a standalone Redis instance, not a Redis Cluster.
// The admission Lua scripts touch keys with different hash slots and are
// physically incompatible with Redis Cluster topology.
func NewClient(cfg Config) (*redis.Client, error) {
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 3 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 3 * time.Second
	}
	if cfg.PoolSize == 0 {
		cfg.PoolSize = 10
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  cfg.DialTimeout,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		PoolSize:     cfg.PoolSize,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("admission redis unreachable at %s: %w", cfg.Addr, err)
	}
	return rdb, nil
}
