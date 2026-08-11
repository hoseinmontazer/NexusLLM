package sweep

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// RedisRebuilder rebuilds admission Redis counters from PostgreSQL state.
// Called on Redis reconnect and periodically (every 6 hours) as validation.
//
// Rebuild strategy per dimension:
//   - Concurrency ZSET: rebuilt from running inference_usage rows.
//   - Daily quota:      rebuilt from quota_ledger (actual usage, not estimates).
//   - Monthly quota:    rebuilt from quota_ledger.
//   - RPM/TPM:          NOT rebuilt — 60-70s windows start fresh after restart.
//     A brief burst window is acceptable. Logging the restart event is required.
type RedisRebuilder struct {
	db         *sqlx.DB
	admissionRDB *redis.Client
	log        *zap.Logger
}

// NewRedisRebuilder constructs a RedisRebuilder.
func NewRedisRebuilder(db *sqlx.DB, admissionRDB *redis.Client, log *zap.Logger) *RedisRebuilder {
	return &RedisRebuilder{db: db, admissionRDB: admissionRDB, log: log}
}

// Run executes a full rebuild pass.
func (r *RedisRebuilder) Run(ctx context.Context) error {
	r.log.Info("redis rebuild started")
	start := time.Now()

	var errs []error

	if err := r.rebuildConcurrency(ctx); err != nil {
		errs = append(errs, fmt.Errorf("concurrency rebuild: %w", err))
	}
	if err := r.rebuildDailyQuotas(ctx); err != nil {
		errs = append(errs, fmt.Errorf("daily quota rebuild: %w", err))
	}
	if err := r.rebuildMonthlyQuotas(ctx); err != nil {
		errs = append(errs, fmt.Errorf("monthly quota rebuild: %w", err))
	}

	r.log.Info("redis rebuild completed",
		zap.Duration("duration", time.Since(start)),
		zap.Int("errors", len(errs)),
	)

	if len(errs) > 0 {
		return errs[0] // Return first error; others were logged individually.
	}
	return nil
}

// Start runs the rebuilder on a ticker. Blocks until ctx is cancelled.
func (r *RedisRebuilder) Start(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.Run(ctx); err != nil {
				r.log.Error("redis rebuild error", zap.Error(err))
			}
		}
	}
}

// rebuildConcurrency rebuilds project inflight ZSETs from running inference_usage rows.
// Only projects with active running inferences get a ZSET entry.
func (r *RedisRebuilder) rebuildConcurrency(ctx context.Context) error {
	type runningRow struct {
		ProjectID string `db:"project_id"`
		RequestID string `db:"request_id"`
		StartedAt time.Time `db:"started_at"`
	}

	var rows []runningRow
	err := r.db.SelectContext(ctx, &rows, `
		SELECT project_id::text, request_id, started_at
		FROM inference_usage
		WHERE execution_status = 'running'
		  AND project_id IS NOT NULL
		  AND started_at > NOW() - INTERVAL '2 hours'`,
	)
	if err != nil {
		return fmt.Errorf("query running inferences: %w", err)
	}

	if len(rows) == 0 {
		return nil
	}

	pipe := r.admissionRDB.Pipeline()
	for _, row := range rows {
		key := fmt.Sprintf("nexus:{admission}:project:%s:inflight", row.ProjectID)
		nowMs := float64(time.Now().UnixMilli())
		pipe.ZAdd(ctx, key, redis.Z{Score: nowMs, Member: row.RequestID})
	}
	_, err = pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("rebuild concurrency ZADD: %w", err)
	}

	r.log.Info("concurrency ZSET rebuilt",
		zap.Int("active_requests", len(rows)),
	)
	return nil
}

// rebuildDailyQuotas restores project daily quota counters from quota_ledger.
func (r *RedisRebuilder) rebuildDailyQuotas(ctx context.Context) error {
	today := time.Now().UTC().Format("2006-01-02")
	endOfDay := endOfDayUnixRebuild(today)

	type quotaRow struct {
		ScopeID    string `db:"scope_id"`
		TokensUsed int64  `db:"tokens_used"`
	}

	var rows []quotaRow
	err := r.db.SelectContext(ctx, &rows, `
		SELECT scope_id::text, tokens_used
		FROM quota_ledger
		WHERE scope_type = 'project'
		  AND period_type = 'daily'
		  AND period_key = $1`, today,
	)
	if err != nil {
		return fmt.Errorf("query daily quotas: %w", err)
	}

	pipe := r.admissionRDB.Pipeline()
	for _, row := range rows {
		key := fmt.Sprintf("nexus:{admission}:project:%s:daily:%s", row.ScopeID, today)
		pipe.Set(ctx, key, row.TokensUsed, 0)
		pipe.ExpireAt(ctx, key, time.Unix(endOfDay, 0))
	}
	if len(rows) > 0 {
		if _, err := pipe.Exec(ctx); err != nil {
			return fmt.Errorf("rebuild daily quota SET: %w", err)
		}
	}

	r.log.Info("daily quota counters rebuilt", zap.Int("projects", len(rows)))
	return nil
}

// rebuildMonthlyQuotas restores project monthly quota counters from quota_ledger.
func (r *RedisRebuilder) rebuildMonthlyQuotas(ctx context.Context) error {
	month := time.Now().UTC().Format("2006-01")
	endOfMonth := endOfMonthUnixRebuild(month)

	type quotaRow struct {
		ScopeID    string `db:"scope_id"`
		TokensUsed int64  `db:"tokens_used"`
	}

	var rows []quotaRow
	err := r.db.SelectContext(ctx, &rows, `
		SELECT scope_id::text, tokens_used
		FROM quota_ledger
		WHERE scope_type = 'project'
		  AND period_type = 'monthly'
		  AND period_key = $1`, month,
	)
	if err != nil {
		return fmt.Errorf("query monthly quotas: %w", err)
	}

	pipe := r.admissionRDB.Pipeline()
	for _, row := range rows {
		key := fmt.Sprintf("nexus:{admission}:project:%s:monthly:%s", row.ScopeID, month)
		pipe.Set(ctx, key, row.TokensUsed, 0)
		pipe.ExpireAt(ctx, key, time.Unix(endOfMonth, 0))
	}
	if len(rows) > 0 {
		if _, err := pipe.Exec(ctx); err != nil {
			return fmt.Errorf("rebuild monthly quota SET: %w", err)
		}
	}

	r.log.Info("monthly quota counters rebuilt", zap.Int("projects", len(rows)))
	return nil
}

// QuotaLedgerSync updates quota_ledger from recent inference_usage completions.
// This keeps PostgreSQL quota state current so Redis rebuilds are accurate.
// Run every 15 minutes.
type QuotaLedgerSync struct {
	db  *sqlx.DB
	log *zap.Logger
}

// NewQuotaLedgerSync constructs a QuotaLedgerSync.
func NewQuotaLedgerSync(db *sqlx.DB, log *zap.Logger) *QuotaLedgerSync {
	return &QuotaLedgerSync{db: db, log: log}
}

// Run syncs recent completions into quota_ledger.
func (q *QuotaLedgerSync) Run(ctx context.Context) error {
	// Upsert daily project quota usage from inference_usage.
	// Uses started_at::date (UTC) for period_key — not completed_at.
	// This ensures corrections apply to the admission day's quota bucket.
	_, err := q.db.ExecContext(ctx, `
		INSERT INTO quota_ledger (scope_type, scope_id, period_type, period_key, tokens_used, tokens_limit)
		SELECT
		    'project'                              AS scope_type,
		    project_id                             AS scope_id,
		    'daily'                                AS period_type,
		    (started_at AT TIME ZONE 'UTC')::date::text AS period_key,
		    COALESCE(SUM(total_tokens), 0)         AS tokens_used,
		    0                                      AS tokens_limit
		FROM inference_usage
		WHERE project_id IS NOT NULL
		  AND execution_status IN ('completed','partial','unknown')
		  AND started_at > NOW() - INTERVAL '2 days'
		GROUP BY project_id, (started_at AT TIME ZONE 'UTC')::date
		ON CONFLICT (scope_type, scope_id, period_type, period_key)
		DO UPDATE SET
		    tokens_used = EXCLUDED.tokens_used,
		    updated_at  = NOW()`,
	)
	if err != nil {
		return fmt.Errorf("quota_ledger daily sync: %w", err)
	}

	// Upsert monthly project quota usage.
	_, err = q.db.ExecContext(ctx, `
		INSERT INTO quota_ledger (scope_type, scope_id, period_type, period_key, tokens_used, tokens_limit)
		SELECT
		    'project'                                          AS scope_type,
		    project_id                                        AS scope_id,
		    'monthly'                                         AS period_type,
		    to_char((started_at AT TIME ZONE 'UTC'), 'YYYY-MM') AS period_key,
		    COALESCE(SUM(total_tokens), 0)                    AS tokens_used,
		    0                                                 AS tokens_limit
		FROM inference_usage
		WHERE project_id IS NOT NULL
		  AND execution_status IN ('completed','partial','unknown')
		  AND started_at > NOW() - INTERVAL '35 days'
		GROUP BY project_id, to_char((started_at AT TIME ZONE 'UTC'), 'YYYY-MM')
		ON CONFLICT (scope_type, scope_id, period_type, period_key)
		DO UPDATE SET
		    tokens_used = EXCLUDED.tokens_used,
		    updated_at  = NOW()`,
	)
	if err != nil {
		return fmt.Errorf("quota_ledger monthly sync: %w", err)
	}

	q.log.Debug("quota_ledger sync completed")
	return nil
}

// Start runs quota ledger sync on a ticker.
func (q *QuotaLedgerSync) Start(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := q.Run(ctx); err != nil {
				q.log.Error("quota ledger sync error", zap.Error(err))
			}
		}
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func endOfDayUnixRebuild(date string) int64 {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return time.Now().Add(24 * time.Hour).Unix()
	}
	return t.UTC().Add(24 * time.Hour).Unix()
}

func endOfMonthUnixRebuild(month string) int64 {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		return time.Now().Add(33 * 24 * time.Hour).Unix()
	}
	return t.UTC().AddDate(0, 1, 0).Unix()
}
