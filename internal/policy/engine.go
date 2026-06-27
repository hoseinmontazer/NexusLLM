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
	projectPrefix    = "nexus:project:" // project-scoped policy keys
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
	Model                string
	EstimatedInputTokens int
	TeamID               string
	// ProjectID — when set, project-level limits are checked first.
	// Falls through to team limits when empty (backward-compat).
	ProjectID string
}

// TeamPolicy holds the runtime policy limits for a team.
type TeamPolicy struct {
	RPMLimit         int
	TPDLimit         int // tokens per day
	MaxConcurrent    int
	MaxContextTokens int
}

// ProjectPolicy holds per-project rate limits. 0 = unlimited for that field.
type ProjectPolicy struct {
	RPMLimit           int
	TPMLimit           int // tokens per minute
	MaxConcurrent      int
	MaxContextTokens   int
	DailyTokenBudget   int64
	MonthlyTokenBudget int64
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
// When req.ProjectID is set, project-level limits are checked first.
// Limits are read from the Redis policy hash (nexus:policy:<teamID> or
// nexus:project:<projectID>) if present, falling back to the in-memory structs.
func (e *Engine) Evaluate(ctx context.Context, req *InferenceRequest, priority int, policy *TeamPolicy) PolicyDecision {
	// 1. Model ACL (team-level — unchanged)
	allowed, err := e.rdb.SIsMember(ctx,
		teamModelsPrefix+req.TeamID+":models", req.Model).Result()
	if err != nil || !allowed {
		return PolicyDecision{Allowed: false, RejectReason: "model_not_allowed"}
	}

	// Load live team limits from Redis
	live := e.loadLivePolicy(ctx, req.TeamID, policy)

	// 2. Context length
	maxCtx := live.MaxContextTokens
	if req.ProjectID != "" {
		pp := e.loadLiveProjectPolicy(ctx, req.ProjectID)
		if pp.MaxContextTokens > 0 {
			maxCtx = pp.MaxContextTokens
		}
	}
	if maxCtx > 0 && req.EstimatedInputTokens > maxCtx {
		return PolicyDecision{Allowed: false, RejectReason: "context_length_exceeded"}
	}

	// ── Project-level checks (when project is scoped) ─────────────────────
	if req.ProjectID != "" {
		pp := e.loadLiveProjectPolicy(ctx, req.ProjectID)

		// 3a. Project RPM
		if pp.RPMLimit > 0 {
			projRPMKey := projectPrefix + req.ProjectID + ":rpm"
			if exceeded, _ := e.checkSlidingWindow(ctx, projRPMKey, pp.RPMLimit, 60*time.Second); exceeded {
				return PolicyDecision{Allowed: false, RejectReason: "project_rate_limit_exceeded"}
			}
		}

		// 3b. Project TPM (tokens per minute — estimated from input)
		if pp.TPMLimit > 0 {
			projTPMKey := projectPrefix + req.ProjectID + ":tpm"
			used, _ := e.rdb.Get(ctx, projTPMKey).Int64()
			if int(used)+req.EstimatedInputTokens > pp.TPMLimit {
				return PolicyDecision{Allowed: false, RejectReason: "project_token_rate_exceeded"}
			}
		}

		// 3c. Project daily token budget
		if pp.DailyTokenBudget > 0 {
			projDailyKey := projectPrefix + req.ProjectID + ":daily:" + time.Now().UTC().Format("2006-01-02")
			dailyUsed, _ := e.rdb.Get(ctx, projDailyKey).Int64()
			if dailyUsed >= pp.DailyTokenBudget {
				return PolicyDecision{Allowed: false, RejectReason: "project_daily_budget_exceeded"}
			}
		}

		// 3d. Project concurrency
		if pp.MaxConcurrent > 0 {
			projInflightKey := projectPrefix + req.ProjectID + ":inflight"
			inflight, _ := e.rdb.Get(ctx, projInflightKey).Int64()
			if int(inflight) >= pp.MaxConcurrent {
				return PolicyDecision{
					Allowed:       false,
					QueueInstead:  true,
					RejectReason:  "project_concurrency_limit_reached",
					QueuePriority: priority,
				}
			}
		}
	}

	// ── Team-level checks (fallback / aggregate ceiling) ──────────────────

	// 4. Team RPM sliding window
	rpmKey := ratelimitPrefix + req.TeamID + ":rpm"
	exceeded, err := e.checkSlidingWindow(ctx, rpmKey, live.RPMLimit, 60*time.Second)
	if err == nil && exceeded {
		return PolicyDecision{Allowed: false, RejectReason: "rate_limit_exceeded"}
	}

	// 5. Team daily token quota
	quotaKey := quotaPrefix + req.TeamID + ":daily:" + time.Now().UTC().Format("2006-01-02")
	dailyUsed, err := e.rdb.Get(ctx, quotaKey).Int64()
	if err == nil && live.TPDLimit > 0 && int(dailyUsed) >= live.TPDLimit {
		return PolicyDecision{Allowed: false, RejectReason: "daily_quota_exceeded"}
	}

	// 6. Team concurrency
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

	// 7. GPU pool capacity
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

// RecordProjectTokenUsage increments per-project token counters for rate limiting.
// Called after every successful inference alongside RecordTokenUsage.
func (e *Engine) RecordProjectTokenUsage(ctx context.Context, projectID string, inputTokens, outputTokens int) error {
	if projectID == "" {
		return nil
	}
	total := int64(inputTokens + outputTokens)
	today := time.Now().UTC().Format("2006-01-02")
	pipe := e.rdb.Pipeline()
	// TPM counter (1-minute rolling window via TTL)
	tpmKey := projectPrefix + projectID + ":tpm"
	pipe.IncrBy(ctx, tpmKey, total)
	pipe.Expire(ctx, tpmKey, 70*time.Second) // slightly longer than 60s to cover clock skew
	// Daily budget counter
	dailyKey := projectPrefix + projectID + ":daily:" + today
	pipe.IncrBy(ctx, dailyKey, total)
	pipe.Expire(ctx, dailyKey, 48*time.Hour)
	_, err := pipe.Exec(ctx)
	return err
}

// IncrementProjectInflight increments the project-level in-flight counter.
func (e *Engine) IncrementProjectInflight(ctx context.Context, projectID string) error {
	if projectID == "" {
		return nil
	}
	key := projectPrefix + projectID + ":inflight"
	pipe := e.rdb.Pipeline()
	pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, 10*time.Minute)
	_, err := pipe.Exec(ctx)
	return err
}

// DecrementProjectInflight decrements the project-level in-flight counter.
func (e *Engine) DecrementProjectInflight(ctx context.Context, projectID string) error {
	if projectID == "" {
		return nil
	}
	key := projectPrefix + projectID + ":inflight"
	val, err := e.rdb.Decr(ctx, key).Result()
	if err != nil {
		return err
	}
	if val < 0 {
		e.rdb.Set(ctx, key, 0, 10*time.Minute)
	}
	return nil
}

// SetProjectPolicy pushes a project policy into Redis so the hot path reads it
// without a DB round-trip. Called by the admin handler on policy update.
func (e *Engine) SetProjectPolicy(ctx context.Context, projectID string, pp ProjectPolicy) error {
	key := projectPrefix + projectID + ":policy"
	pipe := e.rdb.Pipeline()
	pipe.HSet(ctx, key,
		"rpm", pp.RPMLimit,
		"tpm", pp.TPMLimit,
		"max_concurrent", pp.MaxConcurrent,
		"max_context_tokens", pp.MaxContextTokens,
		"daily_tokens", pp.DailyTokenBudget,
		"monthly_tokens", pp.MonthlyTokenBudget,
	)
	pipe.Expire(ctx, key, 48*time.Hour)
	_, err := pipe.Exec(ctx)
	return err
}

// GetProjectQuotaStatus returns current daily/monthly usage counters for a project.
// Used by the admin UI to show remaining budget.
func (e *Engine) GetProjectQuotaStatus(ctx context.Context, projectID string) map[string]int64 {
	today := time.Now().UTC().Format("2006-01-02")
	dailyKey := projectPrefix + projectID + ":daily:" + today
	tpmKey := projectPrefix + projectID + ":tpm"
	inflightKey := projectPrefix + projectID + ":inflight"

	daily, _ := e.rdb.Get(ctx, dailyKey).Int64()
	tpm, _ := e.rdb.Get(ctx, tpmKey).Int64()
	inflight, _ := e.rdb.Get(ctx, inflightKey).Int64()

	return map[string]int64{
		"daily_tokens_used": daily,
		"tpm_current":       tpm,
		"inflight":          inflight,
	}
}

// loadLiveProjectPolicy reads a project's policy from Redis.
// Returns zero-value (unlimited) when not cached yet.
func (e *Engine) loadLiveProjectPolicy(ctx context.Context, projectID string) ProjectPolicy {
	key := projectPrefix + projectID + ":policy"
	vals, err := e.rdb.HGetAll(ctx, key).Result()
	if err != nil || len(vals) == 0 {
		return ProjectPolicy{}
	}
	pp := ProjectPolicy{}
	if v, ok := vals["rpm"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			pp.RPMLimit = n
		}
	}
	if v, ok := vals["tpm"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			pp.TPMLimit = n
		}
	}
	if v, ok := vals["max_concurrent"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			pp.MaxConcurrent = n
		}
	}
	if v, ok := vals["max_context_tokens"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			pp.MaxContextTokens = n
		}
	}
	if v, ok := vals["daily_tokens"]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			pp.DailyTokenBudget = n
		}
	}
	if v, ok := vals["monthly_tokens"]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			pp.MonthlyTokenBudget = n
		}
	}
	return pp
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
