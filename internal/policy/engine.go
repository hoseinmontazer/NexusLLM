// Package policy implements the two-layer policy architecture for NexusLLM.
//
// Architecture:
//
//	Layer 1 — Project Policy (execution layer)
//	  Applied to every inference request. The sole source of truth for:
//	    • RPM (requests per minute)
//	    • TPM (tokens per minute)
//	    • Concurrency / inflight limit
//	    • Context length cap
//	    • Daily / monthly token budgets
//	    • Queue priority weight
//	    • Allowed model ACL (per-project)
//	  The scheduler, rate limiter, queue manager, and autoscaler all operate
//	  exclusively on Project Policy.
//
//	Layer 2 — Org/Team Governance (guardrail layer)
//	  Applied as a final, org-wide safety net AFTER project policy passes.
//	  Only blocks requests when:
//	    • The organization is disabled (billing, compliance)
//	    • The org-level monthly token budget is exhausted
//	    • The GPU pool for the requested model is physically exhausted
//	  Team-level RPM / TPD / concurrency limits are intentionally NOT enforced
//	  here — they existed in the old team-centric design and are now removed
//	  from the hot-path. Team objects represent ownership and billing grouping
//	  only; they do not participate in scheduling or rate limiting.
//
// Request flow (project-scoped key or X-Nexus-Project header):
//
//  1. Model ACL check (project or team)
//  2. Project policy evaluation (RPM, TPM, daily budget, concurrency)
//  3. Org governance check (org disabled, org budget, GPU pool)
//
// Request flow (legacy team-only key, no project context):
//
//  1. Model ACL check (team)
//  2. Team-level soft limits (best-effort; only blocks when explicitly configured)
//  3. Org governance check
package policy

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// ─── Redis key namespaces ─────────────────────────────────────────────────────

const (
	// Project-scoped keys (Layer 1 — execution)
	projectPrefix = "nexus:project:" // nexus:project:<id>:<counter>

	// Org-scoped keys (Layer 2 — governance + model ACL)
	// Organization is the root entity. Model permissions are granted at org
	// level (via org_model_permissions). Governance (billing, compliance,
	// budget caps) is always org-scoped.
	orgPrefix  = "nexus:org:"  // nexus:org:<id>:<counter> and nexus:org:<id>:models
	poolPrefix = "nexus:pool:" // nexus:pool:<model>:at_capacity

	// Team-scoped keys — RBAC/membership only (legacy).
	// NOT used in the execution hot-path for project-scoped requests.
	// Kept for backward compat with team-only API keys.
	teamModelsPrefix = "nexus:team:"      // nexus:team:<id>:models  (legacy ACL set)
	teamPolicyPrefix = "nexus:policy:"    // nexus:policy:<teamID>   (legacy team policy hash)
	quotaPrefix      = "nexus:quota:"     // nexus:quota:<teamID>:daily:<date> (legacy)
	ratelimitPrefix  = "nexus:ratelimit:" // nexus:ratelimit:<teamID>:rpm (legacy)
	inflightPrefix   = "nexus:inflight:"  // nexus:inflight:<teamID> (legacy)
)

// ─── Decision ────────────────────────────────────────────────────────────────

// PolicyDecision is the result of evaluating a request.
type PolicyDecision struct {
	Allowed       bool
	RejectReason  string
	QueueInstead  bool
	QueuePriority int
}

// ─── Request context ─────────────────────────────────────────────────────────

// InferenceRequest carries the identity and sizing context needed for policy
// evaluation. All fields are stable identifiers — no mutable display strings.
type InferenceRequest struct {
	Model                string
	EstimatedInputTokens int

	// Stable identifiers only. TeamName must never appear here.
	OrgID     string
	TeamID    string
	ProjectID string
}

// ─── Policy structs ───────────────────────────────────────────────────────────

// ProjectPolicy holds Layer-1 execution limits for a single project.
// A value of 0 means "unlimited" for that field.
type ProjectPolicy struct {
	RPMLimit           int   // requests per minute
	TPMLimit           int   // tokens per minute (estimated input)
	MaxConcurrent      int   // max simultaneous in-flight requests
	MaxContextTokens   int   // max input tokens per request
	DailyTokenBudget   int64 // max total tokens (input+output) per day
	MonthlyTokenBudget int64 // max total tokens per calendar month
}

// TeamPolicy is kept for backward compatibility with legacy team-only API keys
// and for loading team data at startup. It is NOT used when a ProjectID is set.
type TeamPolicy struct {
	RPMLimit         int
	TPDLimit         int // tokens per day
	MaxConcurrent    int
	MaxContextTokens int
}

// OrgGovernance holds Layer-2 org-wide guardrails.
// These are only evaluated after project policy passes.
type OrgGovernance struct {
	Enabled            bool  // false = org is disabled (billing/compliance)
	MonthlyTokenBudget int64 // 0 = unlimited
}

// ─── Engine ───────────────────────────────────────────────────────────────────

// Engine evaluates two-layer policy decisions using Redis for the hot path.
// No database calls are made during evaluation.
type Engine struct {
	rdb *redis.Client
}

// NewEngine constructs a policy Engine.
func NewEngine(rdb *redis.Client) *Engine {
	return &Engine{rdb: rdb}
}

// ─── Evaluate ────────────────────────────────────────────────────────────────

// Evaluate runs the two-layer policy check for an inference request.
//
// When req.ProjectID is non-empty (project-scoped key or X-Nexus-Project header):
//
//	Layer 1: project policy (RPM, TPM, daily budget, concurrency, context length)
//	Layer 2: org governance (org enabled, org monthly budget, GPU pool)
//
// When req.ProjectID is empty (legacy team-only key):
//
//	Soft team-level limits (backward compat) + org governance.
//
// The team's RPM/TPD/concurrency values are intentionally NOT applied to
// project-scoped requests. Team policy is governance-only.
func (e *Engine) Evaluate(
	ctx context.Context,
	req *InferenceRequest,
	priority int,
	fallback *TeamPolicy,
) PolicyDecision {

	// ── Step 0: Model ACL ────────────────────────────────────────────────────
	// Organization is the root for model permissions.
	// The canonical ACL set is nexus:org:<OrgID>:models.
	// For legacy team-only keys (OrgID resolved via team), we also accept
	// the team-level set (nexus:team:<TeamID>:models) as a fallback so that
	// existing integrations continue to work before migration 031 is run.
	modelAllowed := false
	if req.OrgID != "" {
		ok, _ := e.rdb.SIsMember(ctx, orgPrefix+req.OrgID+":models", req.Model).Result()
		modelAllowed = ok
	}
	if !modelAllowed && req.TeamID != "" {
		// Legacy fallback — team-level ACL set (pre-031 schema)
		ok, _ := e.rdb.SIsMember(ctx, teamModelsPrefix+req.TeamID+":models", req.Model).Result()
		modelAllowed = ok
	}
	if !modelAllowed {
		return PolicyDecision{Allowed: false, RejectReason: "model_not_allowed"}
	}

	// ── Layer 1: Project Policy ───────────────────────────────────────────────
	if req.ProjectID != "" {
		pp := e.loadProjectPolicy(ctx, req.ProjectID)

		// 1a. Context length cap
		if pp.MaxContextTokens > 0 && req.EstimatedInputTokens > pp.MaxContextTokens {
			return PolicyDecision{Allowed: false, RejectReason: "context_length_exceeded"}
		}

		// 1b. RPM — sliding window per project
		if pp.RPMLimit > 0 {
			key := projectPrefix + req.ProjectID + ":rpm"
			if exceeded, _ := e.checkSlidingWindow(ctx, key, pp.RPMLimit, 60*time.Second); exceeded {
				return PolicyDecision{Allowed: false, RejectReason: "project_rate_limit_exceeded"}
			}
		}

		// 1c. TPM — token-per-minute counter (estimated input only; full count added post-response)
		if pp.TPMLimit > 0 {
			key := projectPrefix + req.ProjectID + ":tpm"
			used, _ := e.rdb.Get(ctx, key).Int64()
			if int(used)+req.EstimatedInputTokens > pp.TPMLimit {
				return PolicyDecision{Allowed: false, RejectReason: "project_token_rate_exceeded"}
			}
		}

		// 1d. Daily token budget
		if pp.DailyTokenBudget > 0 {
			key := projectPrefix + req.ProjectID + ":daily:" + today()
			used, _ := e.rdb.Get(ctx, key).Int64()
			if used >= pp.DailyTokenBudget {
				return PolicyDecision{Allowed: false, RejectReason: "project_daily_budget_exceeded"}
			}
		}

		// 1e. Monthly token budget
		if pp.MonthlyTokenBudget > 0 {
			key := projectPrefix + req.ProjectID + ":monthly:" + month()
			used, _ := e.rdb.Get(ctx, key).Int64()
			if used >= pp.MonthlyTokenBudget {
				return PolicyDecision{Allowed: false, RejectReason: "project_monthly_budget_exceeded"}
			}
		}

		// 1f. Concurrency / inflight limit
		if pp.MaxConcurrent > 0 {
			key := projectPrefix + req.ProjectID + ":inflight"
			inflight, _ := e.rdb.Get(ctx, key).Int64()
			if int(inflight) >= pp.MaxConcurrent {
				return PolicyDecision{
					Allowed:       false,
					QueueInstead:  true,
					RejectReason:  "project_concurrency_limit_reached",
					QueuePriority: priority,
				}
			}
		}

		// ── Layer 2: Org Governance (project path) ────────────────────────────
		return e.orgGovernanceCheck(ctx, req, priority)
	}

	// ── Legacy path: team-only API key (no project context) ──────────────────
	// Applied only for backward compatibility. Team-level limits are soft: they
	// only block when the team has an explicit configured limit (> 0).
	live := e.loadTeamPolicy(ctx, req.TeamID, fallback)

	// Context length
	if live.MaxContextTokens > 0 && req.EstimatedInputTokens > live.MaxContextTokens {
		return PolicyDecision{Allowed: false, RejectReason: "context_length_exceeded"}
	}

	// RPM (team)
	if live.RPMLimit > 0 {
		key := ratelimitPrefix + req.TeamID + ":rpm"
		if exceeded, _ := e.checkSlidingWindow(ctx, key, live.RPMLimit, 60*time.Second); exceeded {
			return PolicyDecision{Allowed: false, RejectReason: "rate_limit_exceeded"}
		}
	}

	// Daily token quota (team)
	if live.TPDLimit > 0 {
		key := quotaPrefix + req.TeamID + ":daily:" + today()
		used, _ := e.rdb.Get(ctx, key).Int64()
		if int(used) >= live.TPDLimit {
			return PolicyDecision{Allowed: false, RejectReason: "daily_quota_exceeded"}
		}
	}

	// Concurrency (team)
	if live.MaxConcurrent > 0 {
		key := inflightPrefix + req.TeamID
		inflight, _ := e.rdb.Get(ctx, key).Int64()
		if int(inflight) >= live.MaxConcurrent {
			return PolicyDecision{
				Allowed:       false,
				QueueInstead:  true,
				RejectReason:  "concurrency_limit_reached",
				QueuePriority: priority,
			}
		}
	}

	// ── Layer 2: Org Governance (legacy path) ─────────────────────────────────
	return e.orgGovernanceCheck(ctx, req, priority)
}

// orgGovernanceCheck applies Layer-2 org-wide guardrails.
// It runs after all Layer-1 project checks pass (or on the legacy path).
// Returns PolicyDecision{Allowed: true} when all governance checks pass.
func (e *Engine) orgGovernanceCheck(ctx context.Context, req *InferenceRequest, priority int) PolicyDecision {
	// G1. Org enabled/disabled flag
	if req.OrgID != "" {
		disabledKey := orgPrefix + req.OrgID + ":disabled"
		disabled, _ := e.rdb.Get(ctx, disabledKey).Result()
		if disabled == "1" {
			return PolicyDecision{Allowed: false, RejectReason: "org_disabled"}
		}
	}

	// G2. Org monthly token budget
	if req.OrgID != "" {
		budgetKey := orgPrefix + req.OrgID + ":budget:monthly"
		limitStr, _ := e.rdb.Get(ctx, budgetKey).Result()
		if limitStr != "" {
			if limit, err := strconv.ParseInt(limitStr, 10, 64); err == nil && limit > 0 {
				usedKey := orgPrefix + req.OrgID + ":monthly:" + month()
				used, _ := e.rdb.Get(ctx, usedKey).Int64()
				if used >= limit {
					return PolicyDecision{Allowed: false, RejectReason: "org_monthly_budget_exceeded"}
				}
			}
		}
	}

	// G3. GPU pool physical capacity for the requested model
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

// ─── Inflight counters ────────────────────────────────────────────────────────

// IncrementProjectInflight atomically increments the project-level in-flight counter.
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

// DecrementProjectInflight atomically decrements the project-level in-flight counter.
// The counter is clamped to 0 to guard against counter drift from missed decrements
// (e.g. client disconnect before the defer fires).
func (e *Engine) DecrementProjectInflight(ctx context.Context, projectID string) error {
	if projectID == "" {
		return nil
	}
	key := projectPrefix + projectID + ":inflight"
	// Decrement and clamp atomically so a concurrent Increment doesn't get
	// clobbered by a subsequent Set(0).
	clampScript := redis.NewScript(`
		local v = redis.call('DECR', KEYS[1])
		if v < 0 then
			redis.call('SET', KEYS[1], 0, 'KEEPTTL')
		end
		return v
	`)
	_, err := clampScript.Run(ctx, e.rdb, []string{key}).Int64()
	return err
}

// IncrementInflight increments the legacy team-level in-flight counter.
// Only used for team-only (non-project-scoped) requests.
func (e *Engine) IncrementInflight(ctx context.Context, teamID string) error {
	key := inflightPrefix + teamID
	pipe := e.rdb.Pipeline()
	pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, 10*time.Minute)
	_, err := pipe.Exec(ctx)
	return err
}

// DecrementInflight decrements the legacy team-level in-flight counter.
func (e *Engine) DecrementInflight(ctx context.Context, teamID string) error {
	key := inflightPrefix + teamID
	clampScript := redis.NewScript(`
		local v = redis.call('DECR', KEYS[1])
		if v < 0 then
			redis.call('SET', KEYS[1], 0, 'KEEPTTL')
		end
		return v
	`)
	_, err := clampScript.Run(ctx, e.rdb, []string{key}).Int64()
	return err
}

// ─── Token usage recording ────────────────────────────────────────────────────

// RecordProjectTokenUsage increments per-project token counters (TPM, daily, monthly).
// Called after every successful inference for project-scoped requests.
func (e *Engine) RecordProjectTokenUsage(ctx context.Context, projectID string, inputTokens, outputTokens int) error {
	if projectID == "" {
		return nil
	}
	total := int64(inputTokens + outputTokens)
	pipe := e.rdb.Pipeline()

	// TPM (sliding TTL — tracks last 60s of token usage)
	tpmKey := projectPrefix + projectID + ":tpm"
	pipe.IncrBy(ctx, tpmKey, total)
	pipe.Expire(ctx, tpmKey, 70*time.Second)

	// Daily budget counter
	dailyKey := projectPrefix + projectID + ":daily:" + today()
	pipe.IncrBy(ctx, dailyKey, total)
	pipe.Expire(ctx, dailyKey, 48*time.Hour)

	// Monthly budget counter
	monthlyKey := projectPrefix + projectID + ":monthly:" + month()
	pipe.IncrBy(ctx, monthlyKey, total)
	pipe.Expire(ctx, monthlyKey, 33*24*time.Hour)

	_, err := pipe.Exec(ctx)
	return err
}

// RecordTokenUsage increments legacy team-level daily token usage.
// Only called for team-only (non-project-scoped) requests.
func (e *Engine) RecordTokenUsage(ctx context.Context, teamID string, inputTokens, outputTokens int) error {
	key := quotaPrefix + teamID + ":daily:" + today()
	pipe := e.rdb.Pipeline()
	pipe.IncrBy(ctx, key, int64(inputTokens+outputTokens))
	pipe.Expire(ctx, key, 48*time.Hour)
	_, err := pipe.Exec(ctx)
	return err
}

// RecordOrgTokenUsage increments org-level monthly token usage for governance checks.
// Called after every successful inference regardless of project scope.
func (e *Engine) RecordOrgTokenUsage(ctx context.Context, orgID string, inputTokens, outputTokens int) error {
	if orgID == "" {
		return nil
	}
	key := orgPrefix + orgID + ":monthly:" + month()
	pipe := e.rdb.Pipeline()
	pipe.IncrBy(ctx, key, int64(inputTokens+outputTokens))
	pipe.Expire(ctx, key, 33*24*time.Hour)
	_, err := pipe.Exec(ctx)
	return err
}

// ─── Project policy cache management ─────────────────────────────────────────

// SetProjectPolicy pushes a project's Layer-1 policy into Redis so the hot
// path reads it without a DB round-trip. Called by the admin handler on update.
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

// GetProjectQuotaStatus returns live usage counters for a project (for admin UI).
func (e *Engine) GetProjectQuotaStatus(ctx context.Context, projectID string) map[string]int64 {
	daily, _ := e.rdb.Get(ctx, projectPrefix+projectID+":daily:"+today()).Int64()
	monthly, _ := e.rdb.Get(ctx, projectPrefix+projectID+":monthly:"+month()).Int64()
	tpm, _ := e.rdb.Get(ctx, projectPrefix+projectID+":tpm").Int64()
	inflight, _ := e.rdb.Get(ctx, projectPrefix+projectID+":inflight").Int64()
	return map[string]int64{
		"daily_tokens_used":   daily,
		"monthly_tokens_used": monthly,
		"tpm_current":         tpm,
		"inflight":            inflight,
	}
}

// ─── Org governance management ────────────────────────────────────────────────

// SetOrgDisabled sets or clears the org-disabled governance flag.
// When disabled=true, all requests from this org are rejected with org_disabled.
func (e *Engine) SetOrgDisabled(ctx context.Context, orgID string, disabled bool) error {
	key := orgPrefix + orgID + ":disabled"
	if disabled {
		return e.rdb.Set(ctx, key, "1", 0).Err()
	}
	return e.rdb.Del(ctx, key).Err()
}

// SetOrgMonthlyBudget configures the org-level monthly token governance cap.
// A value of 0 removes the limit.
func (e *Engine) SetOrgMonthlyBudget(ctx context.Context, orgID string, limit int64) error {
	key := orgPrefix + orgID + ":budget:monthly"
	if limit <= 0 {
		return e.rdb.Del(ctx, key).Err()
	}
	return e.rdb.Set(ctx, key, limit, 0).Err()
}

// GetOrgGovernanceStatus returns current governance state for an org.
func (e *Engine) GetOrgGovernanceStatus(ctx context.Context, orgID string) map[string]interface{} {
	disabled, _ := e.rdb.Get(ctx, orgPrefix+orgID+":disabled").Result()
	budget, _ := e.rdb.Get(ctx, orgPrefix+orgID+":budget:monthly").Int64()
	used, _ := e.rdb.Get(ctx, orgPrefix+orgID+":monthly:"+month()).Int64()
	return map[string]interface{}{
		"org_id":               orgID,
		"disabled":             disabled == "1",
		"monthly_token_budget": budget,
		"monthly_tokens_used":  used,
	}
}

// ─── Model ACL helpers ────────────────────────────────────────────────────────

// SetOrgModelAllowed adds a model to an org's allowed-models set in Redis.
// This is the canonical model ACL check used by the policy engine.
func (e *Engine) SetOrgModelAllowed(ctx context.Context, orgID, model string) error {
	return e.rdb.SAdd(ctx, orgPrefix+orgID+":models", model).Err()
}

// RemoveOrgModelAllowed removes a model from an org's allowed-models set.
func (e *Engine) RemoveOrgModelAllowed(ctx context.Context, orgID, model string) error {
	return e.rdb.SRem(ctx, orgPrefix+orgID+":models", model).Err()
}

// SetModelAllowed adds a model to a team's allowed-models set (legacy).
// Prefer SetOrgModelAllowed for new code.
func (e *Engine) SetModelAllowed(ctx context.Context, teamID, model string) error {
	return e.rdb.SAdd(ctx, teamModelsPrefix+teamID+":models", model).Err()
}

// RemoveModelAllowed removes a model from a team's allowed-models set (legacy).
func (e *Engine) RemoveModelAllowed(ctx context.Context, teamID, model string) error {
	return e.rdb.SRem(ctx, teamModelsPrefix+teamID+":models", model).Err()
}

// ─── Infrastructure capacity ──────────────────────────────────────────────────

// SetPoolCapacity marks a model pool as at-capacity or available.
// Used by the autoscaler / capacity monitor to signal GPU exhaustion.
func (e *Engine) SetPoolCapacity(ctx context.Context, model string, atCapacity bool) error {
	key := poolPrefix + model + ":at_capacity"
	val := "0"
	if atCapacity {
		val = "1"
	}
	return e.rdb.Set(ctx, key, val, 30*time.Second).Err()
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

// loadProjectPolicy reads a project's Layer-1 policy from the Redis cache.
// Returns zero-value (unlimited) when not yet cached — the admin handler
// must call SetProjectPolicy after any DB update.
func (e *Engine) loadProjectPolicy(ctx context.Context, projectID string) ProjectPolicy {
	key := projectPrefix + projectID + ":policy"
	vals, err := e.rdb.HGetAll(ctx, key).Result()
	if err != nil || len(vals) == 0 {
		return ProjectPolicy{}
	}
	pp := ProjectPolicy{}
	if v, ok := vals["rpm"]; ok {
		if n, _ := strconv.Atoi(v); n > 0 {
			pp.RPMLimit = n
		}
	}
	if v, ok := vals["tpm"]; ok {
		if n, _ := strconv.Atoi(v); n > 0 {
			pp.TPMLimit = n
		}
	}
	if v, ok := vals["max_concurrent"]; ok {
		if n, _ := strconv.Atoi(v); n > 0 {
			pp.MaxConcurrent = n
		}
	}
	if v, ok := vals["max_context_tokens"]; ok {
		if n, _ := strconv.Atoi(v); n > 0 {
			pp.MaxContextTokens = n
		}
	}
	if v, ok := vals["daily_tokens"]; ok {
		if n, _ := strconv.ParseInt(v, 10, 64); n > 0 {
			pp.DailyTokenBudget = n
		}
	}
	if v, ok := vals["monthly_tokens"]; ok {
		if n, _ := strconv.ParseInt(v, 10, 64); n > 0 {
			pp.MonthlyTokenBudget = n
		}
	}
	return pp
}

// loadTeamPolicy returns live team limits from Redis, falling back to the
// in-memory struct loaded at startup. Only used for legacy team-only keys.
func (e *Engine) loadTeamPolicy(ctx context.Context, teamID string, fallback *TeamPolicy) *TeamPolicy {
	policyKey := teamPolicyPrefix + teamID
	vals, err := e.rdb.HGetAll(ctx, policyKey).Result()
	if err != nil || len(vals) == 0 {
		if fallback != nil {
			return fallback
		}
		return &TeamPolicy{}
	}
	live := &TeamPolicy{}
	if fallback != nil {
		*live = *fallback
	}
	if v, ok := vals["rpm"]; ok {
		if n, _ := strconv.Atoi(v); n >= 0 {
			live.RPMLimit = n
		}
	}
	if v, ok := vals["tpd"]; ok {
		if n, _ := strconv.Atoi(v); n >= 0 {
			live.TPDLimit = n
		}
	}
	if v, ok := vals["max_concurrent"]; ok {
		if n, _ := strconv.Atoi(v); n >= 0 {
			live.MaxConcurrent = n
		}
	}
	if v, ok := vals["max_context_tokens"]; ok {
		if n, _ := strconv.Atoi(v); n >= 0 {
			live.MaxContextTokens = n
		}
	}
	return live
}

// checkSlidingWindow implements a Redis sorted-set sliding window rate limiter.
// Returns true if the limit is exceeded (request should be rejected/queued).
// Uses a Lua script for atomicity — no race between check and increment.
func (e *Engine) checkSlidingWindow(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	now := time.Now().UnixMilli()
	windowMS := window.Milliseconds()

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

	result, err := script.Run(ctx, e.rdb, []string{key}, now, windowMS, limit).Int()
	if err != nil {
		return false, fmt.Errorf("sliding window script: %w", err)
	}
	return result == 1, nil
}

// today returns the current UTC date as YYYY-MM-DD for use in daily counter keys.
func today() string {
	return time.Now().UTC().Format("2006-01-02")
}

// month returns the current UTC year-month as YYYY-MM for use in monthly counter keys.
func month() string {
	return time.Now().UTC().Format("2006-01")
}
