package policy

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	ratelimitPrefix  = "nexus:ratelimit:"
	quotaPrefix      = "nexus:quota:"
	inflightPrefix   = "nexus:inflight:"
	poolPrefix       = "nexus:pool:"
	teamModelsPrefix = "nexus:team:"
)

// PolicyDecision is the result of evaluating a request against team policy.
type PolicyDecision struct {
	Allowed       bool
	RejectReason  string
	QueueInstead  bool
	QueuePriority int
}

// InferenceRequest carries the fields needed for policy evaluation.
type InferenceRequest struct {
	Model               string
	EstimatedInputTokens int
	TeamID              string
}

// TeamPolicy holds the runtime policy limits for a team.
type TeamPolicy struct {
	RPMLimit           int
	TPDLimit           int // tokens per day
	MaxConcurrent      int
	MaxContextTokens   int
}

// Engine evaluates policy decisions using Redis for hot-path checks.
type Engine struct {
	rdb *redis.Client
}

// NewEngine constructs a policy Engine.
func NewEngine(rdb *redis.Client) *Engine {
	return &Engine{rdb: rdb}
}

// Evaluate runs all policy checks for the given request and team claims.
// Policy checks run entirely in Redis — no DB calls on the hot path.
// Limits are read from the Redis policy hash (nexus:policy:<teamID>) if present,
// falling back to the in-memory TeamPolicy struct loaded at startup.
func (e *Engine) Evaluate(ctx context.Context, req *InferenceRequest, priority int, policy *TeamPolicy) PolicyDecision {
	// 1. Model ACL
	allowed, err := e.rdb.SIsMember(ctx,
		teamModelsPrefix+req.TeamID+":models", req.Model).Result()
	if err != nil || !allowed {
		return PolicyDecision{Allowed: false, RejectReason: "model_not_allowed"}
	}

	// Load live limits from Redis (updated by admin on policy change)
	live := e.loadLivePolicy(ctx, req.TeamID, policy)

	// 2. Context length
	if live.MaxContextTokens > 0 && req.EstimatedInputTokens > live.MaxContextTokens {
		return PolicyDecision{Allowed: false, RejectReason: "context_length_exceeded"}
	}

	// 3. Rate limit — Redis sliding window (requests per 60s window)
	rpmKey := ratelimitPrefix + req.TeamID + ":rpm"
	exceeded, err := e.checkSlidingWindow(ctx, rpmKey, live.RPMLimit, 60*time.Second)
	if err == nil && exceeded {
		return PolicyDecision{Allowed: false, RejectReason: "rate_limit_exceeded"}
	}

	// 4. Daily token quota — Redis counter reset at midnight
	quotaKey := quotaPrefix + req.TeamID + ":daily:" + time.Now().UTC().Format("2006-01-02")
	dailyUsed, err := e.rdb.Get(ctx, quotaKey).Int64()
	if err == nil && live.TPDLimit > 0 && int(dailyUsed) >= live.TPDLimit {
		return PolicyDecision{Allowed: false, RejectReason: "daily_quota_exceeded"}
	}

	// 5. Concurrency
	inflightKey := inflightPrefix + req.TeamID
	inflight, err := e.rdb.Get(ctx, inflightKey).Int64()
	if err == nil && live.MaxConcurrent > 0 && int(inflight) >= live.MaxConcurrent {
		return PolicyDecision{
			Allowed:       false,
			QueueInstead:  true,
			RejectReason:  "concurrency_limit_reached",
			QueuePriority: priority,
		}
	}

	// 6. GPU pool capacity
	poolKey := poolPrefix + req.Model + ":at_capacity"
	atCap, _ := e.rdb.Get(ctx, poolKey).Result()
	if atCap == "1" {
		return PolicyDecision{
			Allowed:       false,
			QueueInstead:  true,
			RejectReason:  "gpu_capacity_exhausted",
			QueuePriority: priority,
		}
	}

	return PolicyDecision{Allowed: true}
}

// loadLivePolicy returns policy limits from Redis if the admin has pushed an update,
// otherwise falls back to the in-memory struct loaded at gateway startup.
func (e *Engine) loadLivePolicy(ctx context.Context, teamID string, fallback *TeamPolicy) *TeamPolicy {
	policyKey := "nexus:policy:" + teamID
	vals, err := e.rdb.HGetAll(ctx, policyKey).Result()
	if err != nil || len(vals) == 0 {
		return fallback
	}
	live := &TeamPolicy{
		RPMLimit:         fallback.RPMLimit,
		TPDLimit:         fallback.TPDLimit,
		MaxConcurrent:    fallback.MaxConcurrent,
		MaxContextTokens: fallback.MaxContextTokens,
	}
	if v, ok := vals["rpm"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			live.RPMLimit = n
		}
	}
	if v, ok := vals["tpd"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			live.TPDLimit = n
		}
	}
	if v, ok := vals["max_concurrent"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			live.MaxConcurrent = n
		}
	}
	if v, ok := vals["max_context_tokens"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			live.MaxContextTokens = n
		}
	}
	return live
}

// IncrementInflight atomically increments the in-flight request counter for a team.
func (e *Engine) IncrementInflight(ctx context.Context, teamID string) error {
	key := inflightPrefix + teamID
	pipe := e.rdb.Pipeline()
	pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, 10*time.Minute) // safety TTL
	_, err := pipe.Exec(ctx)
	return err
}

// DecrementInflight atomically decrements the in-flight request counter for a team.
func (e *Engine) DecrementInflight(ctx context.Context, teamID string) error {
	key := inflightPrefix + teamID
	val, err := e.rdb.Decr(ctx, key).Result()
	if err != nil {
		return err
	}
	if val < 0 {
		e.rdb.Set(ctx, key, 0, 10*time.Minute)
	}
	return nil
}

// RecordTokenUsage increments daily token usage counter for the team.
func (e *Engine) RecordTokenUsage(ctx context.Context, teamID string, inputTokens, outputTokens int) error {
	key := quotaPrefix + teamID + ":daily:" + time.Now().UTC().Format("2006-01-02")
	pipe := e.rdb.Pipeline()
	pipe.IncrBy(ctx, key, int64(inputTokens+outputTokens))
	// TTL of 2 days ensures cleanup
	pipe.Expire(ctx, key, 48*time.Hour)
	_, err := pipe.Exec(ctx)
	return err
}

// SetModelAllowed adds a model to a team's allowed-models Redis set.
func (e *Engine) SetModelAllowed(ctx context.Context, teamID, model string) error {
	key := teamModelsPrefix + teamID + ":models"
	return e.rdb.SAdd(ctx, key, model).Err()
}

// RemoveModelAllowed removes a model from a team's allowed-models Redis set.
func (e *Engine) RemoveModelAllowed(ctx context.Context, teamID, model string) error {
	key := teamModelsPrefix + teamID + ":models"
	return e.rdb.SRem(ctx, key, model).Err()
}

// SetPoolCapacity marks a model pool as at-capacity (val=true) or available (val=false).
func (e *Engine) SetPoolCapacity(ctx context.Context, model string, atCapacity bool) error {
	key := poolPrefix + model + ":at_capacity"
	val := "0"
	if atCapacity {
		val = "1"
	}
	return e.rdb.Set(ctx, key, val, 30*time.Second).Err()
}

// checkSlidingWindow implements a Redis-based sliding window rate limiter.
// Returns true if the limit has been exceeded.
func (e *Engine) checkSlidingWindow(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	now := time.Now().UnixMilli()
	windowMS := window.Milliseconds()

	// Lua script: remove old entries, count, add new entry
	script := redis.NewScript(`
		local key    = KEYS[1]
		local now    = tonumber(ARGV[1])
		local window = tonumber(ARGV[2])
		local limit  = tonumber(ARGV[3])

		redis.call('ZREMRANGEBYSCORE', key, '-inf', now - window)
		local count = redis.call('ZCARD', key)
		if count >= limit then
			return 1
		end
		redis.call('ZADD', key, now, now)
		redis.call('PEXPIRE', key, window)
		return 0
	`)

	result, err := script.Run(ctx, e.rdb,
		[]string{key},
		now, windowMS, limit,
	).Int()
	if err != nil {
		return false, fmt.Errorf("sliding window script: %w", err)
	}
	return result == 1, nil
}
