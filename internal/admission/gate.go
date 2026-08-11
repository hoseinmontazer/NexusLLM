package admission

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

//go:embed lua/admission_gate.lua
var luaAdmissionGate string

//go:embed lua/admission_rollback.lua
var luaAdmissionRollback string

//go:embed lua/tpm_correction.lua
var luaTPMCorrection string

// Limits holds all rate-limit and quota parameters for a single admission call.
// Zero values mean "unlimited" for that dimension.
type Limits struct {
	ModelRPM      int64
	ModelITPM     int64
	ProjectRPM    int64
	ProjectITPM   int64
	ProjectInflight int64
	TeamEnforceRPM  bool
	TeamRPM         int64
	TeamEnforceTMP  bool
	TeamITPM        int64
	DailyTokens   int64
	MonthlyTokens int64
	OrgMonthly    int64
}

// AdmitParams contains everything needed to call the admission gate.
type AdmitParams struct {
	RequestID      string
	ModelName      string
	ProjectID      string
	TeamID         string
	OrgID          string
	EstInput       int64  // estimated input tokens
	MaxReservation int64  // est_input + max_output_tokens
	AdmissionDate  string // YYYY-MM-DD UTC — for daily key
	AdmissionMonth string // YYYY-MM UTC — for monthly key
	AuthExpiresAt  time.Time
	Limits         Limits
}

// AdmitResult is returned by Gate.Admit.
type AdmitResult struct {
	// Admitted is true when the request was admitted (new or idempotent).
	Admitted bool
	// Idempotent is true when request_id was already admitted previously.
	Idempotent bool
	// Token is the admission token UUID (used for rollback ownership check).
	Token string
	// RejectedDimension is set when Admitted=false.
	RejectedDimension string
}

// RollbackParams identifies which admission to roll back.
type RollbackParams struct {
	RequestID      string
	AdmissionToken string
	ModelName      string
	ProjectID      string
	TeamID         string
	AdmissionDate  string
	AdmissionMonth string
	EstInput       int64
	MaxReservation int64
}

// CorrectionParams contains post-response token counts for counter correction.
type CorrectionParams struct {
	RequestID      string
	AdmissionToken string
	ModelName      string
	ProjectID      string
	TeamID         string
	OrgID          string
	AdmissionDate  string
	AdmissionMonth string
	EstInput       int64
	ActualInput    int64
	ActualOutput   int64
	MaxReservation int64
}

// Gate is the atomic admission gate.
// It wraps the three Lua scripts and provides PostgreSQL fallback
// for daily/monthly quota checks when Redis is unavailable.
type Gate struct {
	rdb *redis.Client
	db  *sqlx.DB
	log *zap.Logger

	scriptAdmit    *redis.Script
	scriptRollback *redis.Script
	scriptCorrect  *redis.Script

	// Configuration constants
	rpmWindowMs      int64 // sliding window size in ms (default: 60000)
	tpmTTLSeconds    int64 // TPM counter TTL in seconds (default: 70)
	heartbeatStaleMs int64 // stale cutoff for inflight ZSET (default: 600000)
}

// NewGate constructs an admission Gate.
// rdb must be a standalone Redis client (not cluster).
// db is used for PostgreSQL fallback when Redis is unavailable.
func NewGate(rdb *redis.Client, db *sqlx.DB, log *zap.Logger) *Gate {
	return &Gate{
		rdb:              rdb,
		db:               db,
		log:              log,
		scriptAdmit:      redis.NewScript(luaAdmissionGate),
		scriptRollback:   redis.NewScript(luaAdmissionRollback),
		scriptCorrect:    redis.NewScript(luaTPMCorrection),
		rpmWindowMs:      60_000,
		tpmTTLSeconds:    70,
		heartbeatStaleMs: 600_000,
	}
}

// Admit runs the atomic admission gate for a single request.
// On Redis unavailability it falls back to PostgreSQL quota checks.
// Returns AdmitResult — check Admitted before proceeding.
func (g *Gate) Admit(ctx context.Context, p AdmitParams) (AdmitResult, error) {
	token := uuid.New().String()

	endOfDay := endOfDayUnix(p.AdmissionDate)
	endOfMonth := endOfMonthUnix(p.AdmissionMonth)
	tokenTTL := int64(time.Until(p.AuthExpiresAt).Seconds())
	if tokenTTL <= 0 {
		tokenTTL = 900 // 15 minutes default
	}

	keys := []string{
		keyModelRPM(p.ModelName),
		keyModelITPM(p.ModelName),
		keyProjectRPM(p.ProjectID),
		keyProjectITPM(p.ProjectID),
		keyProjectInflight(p.ProjectID),
		keyTeamRPM(p.TeamID),
		keyTeamITPM(p.TeamID),
		keyProjectDaily(p.ProjectID, p.AdmissionDate),
		keyProjectMonthly(p.ProjectID, p.AdmissionMonth),
		keyOrgMonthly(p.OrgID),
		keyToken(p.RequestID),
	}

	args := []interface{}{
		p.RequestID,
		token,
		p.EstInput,
		p.MaxReservation,
		g.rpmWindowMs,
		g.tpmTTLSeconds,
		tokenTTL,
		endOfDay,
		endOfMonth,
		g.heartbeatStaleMs,
		p.Limits.ModelRPM,
		p.Limits.ModelITPM,
		p.Limits.ProjectRPM,
		p.Limits.ProjectITPM,
		p.Limits.ProjectInflight,
		boolArg(p.Limits.TeamEnforceRPM),
		p.Limits.TeamRPM,
		boolArg(p.Limits.TeamEnforceTMP),
		p.Limits.TeamITPM,
		p.Limits.DailyTokens,
		p.Limits.MonthlyTokens,
		p.Limits.OrgMonthly,
	}

	result, err := g.scriptAdmit.Run(ctx, g.rdb, keys, args...).StringSlice()
	if err != nil {
		if isRedisUnavailable(err) {
			g.log.Warn("admission redis unavailable, using PostgreSQL fallback",
				zap.String("request_id", p.RequestID),
				zap.Error(err),
			)
			return g.admitPGFallback(ctx, p, token)
		}
		return AdmitResult{}, fmt.Errorf("admission gate: %w", err)
	}

	return parseAdmitResult(result, token), nil
}

// Rollback reverses admission counters when billing authorization fails.
// This is best-effort: if Redis is unavailable, counters self-correct via TTL.
func (g *Gate) Rollback(ctx context.Context, p RollbackParams) {
	keys := []string{
		keyModelRPM(p.ModelName),
		keyModelITPM(p.ModelName),
		keyProjectRPM(p.ProjectID),
		keyProjectITPM(p.ProjectID),
		keyProjectInflight(p.ProjectID),
		keyTeamRPM(p.TeamID),
		keyTeamITPM(p.TeamID),
		keyProjectDaily(p.ProjectID, p.AdmissionDate),
		keyProjectMonthly(p.ProjectID, p.AdmissionMonth),
		keyToken(p.RequestID),
	}
	args := []interface{}{
		p.RequestID,
		p.AdmissionToken,
		p.EstInput,
		p.MaxReservation,
	}
	result, err := g.scriptRollback.Run(ctx, g.rdb, keys, args...).StringSlice()
	if err != nil {
		g.log.Warn("admission rollback failed — counters will self-correct via TTL",
			zap.String("request_id", p.RequestID),
			zap.Error(err),
		)
		return
	}
	if len(result) > 0 && result[0] == "noop" {
		g.log.Debug("admission rollback: token mismatch or expired",
			zap.String("request_id", p.RequestID),
			zap.String("reason", safeGet(result, 1)),
		)
	}
}

// Correct runs the post-response TPM correction script.
// It adjusts input TPM for actual vs estimated, records output TPM,
// releases unused quota reservation, and removes the request from inflight.
// This is best-effort — call it in a goroutine or defer.
func (g *Gate) Correct(ctx context.Context, p CorrectionParams) {
	keys := []string{
		keyModelITPM(p.ModelName),
		keyProjectITPM(p.ProjectID),
		keyTeamITPM(p.TeamID),
		keyModelOTPM(p.ModelName),
		keyProjectOTPM(p.ProjectID),
		keyTeamOTPM(p.TeamID),
		keyProjectDaily(p.ProjectID, p.AdmissionDate),
		keyProjectMonthly(p.ProjectID, p.AdmissionMonth),
		keyOrgMonthly(p.OrgID),
		keyProjectInflight(p.ProjectID),
		keyToken(p.RequestID),
	}
	args := []interface{}{
		p.RequestID,
		p.EstInput,
		p.ActualInput,
		p.ActualOutput,
		p.MaxReservation,
	}
	if _, err := g.scriptCorrect.Run(ctx, g.rdb, keys, args...).StringSlice(); err != nil {
		g.log.Warn("tpm correction failed — counters may drift until TTL",
			zap.String("request_id", p.RequestID),
			zap.Error(err),
		)
	}
}

// Heartbeat updates the inflight ZSET score for a request so it is not
// pruned as stale during long-running inferences.
// Call every 30 seconds from a context-aware goroutine.
func (g *Gate) Heartbeat(ctx context.Context, projectID, requestID string) {
	key := keyProjectInflight(projectID)
	nowMs := float64(time.Now().UnixMilli())
	if err := g.rdb.ZAdd(ctx, key, redis.Z{Score: nowMs, Member: requestID}).Err(); err != nil {
		// Heartbeat failure is non-fatal. Log only.
		g.log.Debug("heartbeat update failed",
			zap.String("project_id", projectID),
			zap.String("request_id", requestID),
			zap.Error(err),
		)
	}
}

// keyProjectOTPM is the output-TPM advisory key.
func keyProjectOTPM(projectID string) string {
	return fmt.Sprintf("nexus:{admission}:project:%s:otpm", projectID)
}

// ── PostgreSQL fallback ───────────────────────────────────────────────────────

// admitPGFallback is used when Redis is unavailable.
// Enforces project concurrency and daily/monthly quota via PostgreSQL.
// Model and project RPM/TPM are not enforced (fail open for throughput limits;
// fail closed only for financial quotas and concurrency).
func (g *Gate) admitPGFallback(ctx context.Context, p AdmitParams, token string) (AdmitResult, error) {
	// Check concurrency via inference_usage
	if p.Limits.ProjectInflight > 0 {
		var running int
		err := g.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM inference_usage
			 WHERE project_id = $1::uuid AND execution_status = 'running'`,
			p.ProjectID,
		).Scan(&running)
		if err == nil && int64(running) >= p.Limits.ProjectInflight {
			return AdmitResult{Admitted: false, RejectedDimension: "project_concurrency_exceeded"}, nil
		}
	}

	// Check daily quota via quota_ledger
	if p.Limits.DailyTokens > 0 {
		var used int64
		err := g.db.QueryRowContext(ctx,
			`SELECT COALESCE(tokens_used, 0) FROM quota_ledger
			 WHERE scope_type='project' AND scope_id=$1::uuid
			   AND period_type='daily' AND period_key=$2`,
			p.ProjectID, p.AdmissionDate,
		).Scan(&used)
		if err == nil && used+p.MaxReservation > p.Limits.DailyTokens {
			return AdmitResult{Admitted: false, RejectedDimension: "daily_quota_exceeded"}, nil
		}
	}

	// Check monthly quota via quota_ledger
	if p.Limits.MonthlyTokens > 0 {
		var used int64
		err := g.db.QueryRowContext(ctx,
			`SELECT COALESCE(tokens_used, 0) FROM quota_ledger
			 WHERE scope_type='project' AND scope_id=$1::uuid
			   AND period_type='monthly' AND period_key=$2`,
			p.ProjectID, p.AdmissionMonth,
		).Scan(&used)
		if err == nil && used+p.MaxReservation > p.Limits.MonthlyTokens {
			return AdmitResult{Admitted: false, RejectedDimension: "monthly_quota_exceeded"}, nil
		}
	}

	// Model RPM — fail closed when Redis unavailable
	return AdmitResult{Admitted: false, RejectedDimension: "admission_redis_unavailable"}, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func parseAdmitResult(result []string, generatedToken string) AdmitResult {
	if len(result) == 0 {
		return AdmitResult{}
	}
	switch result[0] {
	case "admitted":
		token := generatedToken
		if len(result) > 1 && result[1] != "" {
			token = result[1]
		}
		return AdmitResult{Admitted: true, Token: token}
	case "admitted_idempotent":
		token := ""
		if len(result) > 1 {
			token = result[1]
		}
		return AdmitResult{Admitted: true, Idempotent: true, Token: token}
	case "rejected":
		dim := ""
		if len(result) > 1 {
			dim = result[1]
		}
		return AdmitResult{Admitted: false, RejectedDimension: dim}
	}
	return AdmitResult{}
}

func isRedisUnavailable(err error) bool {
	if err == nil {
		return false
	}
	// redis.Nil means "key not found", not unavailability
	return !errors.Is(err, redis.Nil)
}

func boolArg(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func safeGet(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return ""
}

// endOfDayUnix returns the Unix timestamp of the end of the day for a given
// date string "YYYY-MM-DD" in UTC.
func endOfDayUnix(date string) int64 {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		// Fallback: 24 hours from now
		return time.Now().Add(24 * time.Hour).Unix()
	}
	// End of day = start of next day in UTC
	return t.UTC().Add(24 * time.Hour).Unix()
}

// endOfMonthUnix returns the Unix timestamp of the end of the month for
// a given month string "YYYY-MM" in UTC.
func endOfMonthUnix(month string) int64 {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		return time.Now().Add(33 * 24 * time.Hour).Unix()
	}
	// End of month = start of next month in UTC
	nextMonth := t.UTC().AddDate(0, 1, 0)
	return nextMonth.Unix()
}
