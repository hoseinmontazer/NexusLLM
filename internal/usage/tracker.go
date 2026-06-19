// Package usage provides async usage event ingestion, aggregation,
// and query APIs for billing and observability.
package usage

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// ─────────────────────────────────────────────────────────────────────────────
// Event model
// ─────────────────────────────────────────────────────────────────────────────

// Event represents a single completed inference request.
type Event struct {
	ID               string    `json:"id"                db:"id"`
	OrgID            string    `json:"org_id"            db:"org_id"`
	TeamID           string    `json:"team_id"           db:"team_id"`
	APIKeyID         string    `json:"api_key_id"        db:"api_key_id"`
	ModelID          string    `json:"model_id"          db:"model_id"`
	ModelName        string    `json:"model_name"        db:"model_name"`
	EndpointID       string    `json:"endpoint_id"       db:"endpoint_id"`
	RequestID        string    `json:"request_id"        db:"request_id"`
	PromptTokens     int       `json:"prompt_tokens"     db:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens" db:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"      db:"total_tokens"`
	LatencyMs        int       `json:"latency_ms"        db:"latency_ms"`
	TTFTMs           int       `json:"ttft_ms"           db:"ttft_ms"`
	QueueWaitMs      int       `json:"queue_wait_ms"     db:"queue_wait_ms"`
	Status           string    `json:"status"            db:"status"`
	ErrorCode        string    `json:"error_code"        db:"error_code"`
	CostUSD          float64   `json:"cost_usd"          db:"cost_usd"`
	GPUTimeMs        int       `json:"gpu_time_ms"       db:"gpu_time_ms"`
	CreatedAt        time.Time `json:"created_at"        db:"created_at"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Tracker
// ─────────────────────────────────────────────────────────────────────────────

const (
	usageStreamKey = "nexus:usage:events" // Redis Stream for async ingestion
	streamMaxLen   = 100_000
)

// Tracker accepts usage events from the proxy, enqueues them to a Redis
// Stream for async processing, and provides query methods for dashboards.
type Tracker struct {
	db  *sqlx.DB
	rdb *redis.Client
	log *zap.Logger

	// Cost per 1M tokens (configurable per model in production)
	inputCostPer1M  float64
	outputCostPer1M float64
}

// NewTracker constructs a Tracker.
func NewTracker(db *sqlx.DB, rdb *redis.Client, log *zap.Logger) *Tracker {
	return &Tracker{
		db:              db,
		rdb:             rdb,
		log:             log,
		inputCostPer1M:  0.50,  // $0.50 / 1M input tokens (example)
		outputCostPer1M: 1.50,  // $1.50 / 1M output tokens
	}
}

// Record enqueues a usage event to Redis Stream (non-blocking, < 1ms).
func (t *Tracker) Record(ctx context.Context, e Event) {
	if e.ID == "" {
		e.ID = uuid.New().String()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	e.TotalTokens = e.PromptTokens + e.CompletionTokens
	e.CostUSD = t.computeCost(e.PromptTokens, e.CompletionTokens)

	data, err := json.Marshal(e)
	if err != nil {
		t.log.Error("usage marshal error", zap.Error(err))
		return
	}

	_ = t.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: usageStreamKey,
		MaxLen: streamMaxLen,
		Values: map[string]interface{}{"event": string(data)},
	}).Err()
}

// StartConsumer reads events from the Redis Stream and writes them to
// PostgreSQL. Runs until ctx is cancelled.
func (t *Tracker) StartConsumer(ctx context.Context) {
	const group = "usage-writer"
	const consumer = "writer-1"

	// Create consumer group (idempotent)
	_ = t.rdb.XGroupCreateMkStream(ctx, usageStreamKey, group, "0").Err()

	t.log.Info("usage consumer started")
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msgs, err := t.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    group,
			Consumer: consumer,
			Streams:  []string{usageStreamKey, ">"},
			Count:    200,
			Block:    500 * time.Millisecond,
		}).Result()
		if err != nil {
			continue
		}

		for _, stream := range msgs {
			for _, msg := range stream.Messages {
				t.processMessage(ctx, stream.Stream, msg)
			}
		}
	}
}

func (t *Tracker) processMessage(ctx context.Context, stream string, msg redis.XMessage) {
	defer func() {
		_ = t.rdb.XAck(ctx, stream, "usage-writer", msg.ID)
	}()

	raw, ok := msg.Values["event"].(string)
	if !ok {
		return
	}
	var e Event
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return
	}
	t.persist(ctx, e)
}

func (t *Tracker) persist(ctx context.Context, e Event) {
	_, err := t.db.ExecContext(ctx, `
		INSERT INTO usage_events
		  (id, org_id, team_id, api_key_id, model_id, model_name, endpoint_id,
		   request_id, prompt_tokens, completion_tokens, total_tokens,
		   latency_ms, ttft_ms, queue_wait_ms, status, error_code, cost_usd, gpu_time_ms, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		ON CONFLICT (id) DO NOTHING`,
		e.ID, e.OrgID, e.TeamID, e.APIKeyID, e.ModelID, e.ModelName, e.EndpointID,
		e.RequestID, e.PromptTokens, e.CompletionTokens, e.TotalTokens,
		e.LatencyMs, e.TTFTMs, e.QueueWaitMs, e.Status, e.ErrorCode, e.CostUSD, e.GPUTimeMs,
		e.CreatedAt,
	)
	if err != nil {
		t.log.Error("usage persist error", zap.Error(err), zap.String("event_id", e.ID))
	}
}

// Aggregate runs hourly + daily rollups. Call from a cron goroutine.
func (t *Tracker) Aggregate(ctx context.Context) {
	t.aggregateHourly(ctx)
	t.aggregateDaily(ctx)
}

func (t *Tracker) aggregateHourly(ctx context.Context) {
	_, _ = t.db.ExecContext(ctx, `
		INSERT INTO usage_hourly
		  (team_id, model_name, hour,
		   request_count, error_count,
		   prompt_tokens, completion_tokens, total_tokens, cost_usd)
		SELECT
		  team_id, model_name,
		  date_trunc('hour', created_at) AS hour,
		  COUNT(*),
		  COUNT(*) FILTER (WHERE status != 'success'),
		  SUM(prompt_tokens), SUM(completion_tokens), SUM(total_tokens),
		  SUM(cost_usd)
		FROM usage_events
		WHERE created_at >= date_trunc('hour', NOW()) - INTERVAL '2 hours'
		GROUP BY team_id, model_name, date_trunc('hour', created_at)
		ON CONFLICT (team_id, model_name, hour) DO UPDATE SET
		  request_count     = EXCLUDED.request_count,
		  error_count       = EXCLUDED.error_count,
		  prompt_tokens     = EXCLUDED.prompt_tokens,
		  completion_tokens = EXCLUDED.completion_tokens,
		  total_tokens      = EXCLUDED.total_tokens,
		  cost_usd          = EXCLUDED.cost_usd`)
}

func (t *Tracker) aggregateDaily(ctx context.Context) {
	_, _ = t.db.ExecContext(ctx, `
		INSERT INTO usage_daily
		  (team_id, model_name, day,
		   request_count, error_count,
		   prompt_tokens, completion_tokens, total_tokens, cost_usd)
		SELECT
		  team_id, model_name,
		  created_at::date AS day,
		  COUNT(*),
		  COUNT(*) FILTER (WHERE status != 'success'),
		  SUM(prompt_tokens), SUM(completion_tokens), SUM(total_tokens),
		  SUM(cost_usd)
		FROM usage_events
		WHERE created_at::date = CURRENT_DATE
		GROUP BY team_id, model_name, created_at::date
		ON CONFLICT (team_id, model_name, day) DO UPDATE SET
		  request_count     = EXCLUDED.request_count,
		  error_count       = EXCLUDED.error_count,
		  prompt_tokens     = EXCLUDED.prompt_tokens,
		  completion_tokens = EXCLUDED.completion_tokens,
		  total_tokens      = EXCLUDED.total_tokens,
		  cost_usd          = EXCLUDED.cost_usd`)
}

// ─── Query API ────────────────────────────────────────────────────────────────

// TeamSummary holds aggregated usage for a team.
type TeamSummary struct {
	TeamID           string  `db:"team_id"           json:"team_id"`
	ModelName        string  `db:"model_name"        json:"model_name"`
	Day              string  `db:"day"               json:"day"`
	RequestCount     int64   `db:"request_count"     json:"request_count"`
	ErrorCount       int64   `db:"error_count"       json:"error_count"`
	PromptTokens     int64   `db:"prompt_tokens"     json:"prompt_tokens"`
	CompletionTokens int64   `db:"completion_tokens" json:"completion_tokens"`
	TotalTokens      int64   `db:"total_tokens"      json:"total_tokens"`
	CostUSD          float64 `db:"cost_usd"          json:"cost_usd"`
}

// GetTeamDailyUsage returns daily usage for a team in a date range.
func (t *Tracker) GetTeamDailyUsage(ctx context.Context, teamID, from, to string) ([]TeamSummary, error) {
	var rows []TeamSummary
	err := t.db.SelectContext(ctx, &rows, `
		SELECT team_id, model_name, day::text, request_count, error_count,
		       prompt_tokens, completion_tokens, total_tokens, cost_usd
		FROM usage_daily
		WHERE team_id = $1 AND day BETWEEN $2::date AND $3::date
		ORDER BY day DESC, model_name`, teamID, from, to)
	return rows, err
}

// GetOrgMonthlySpend returns total spend for an org in the current month.
func (t *Tracker) GetOrgMonthlySpend(ctx context.Context, orgID string) (float64, error) {
	var total float64
	err := t.db.GetContext(ctx, &total, `
		SELECT COALESCE(SUM(ue.cost_usd), 0)
		FROM usage_events ue
		WHERE ue.org_id = $1
		  AND ue.created_at >= date_trunc('month', NOW())`, orgID)
	return total, err
}

func (t *Tracker) computeCost(promptTokens, completionTokens int) float64 {
	return float64(promptTokens)/1_000_000*t.inputCostPer1M +
		float64(completionTokens)/1_000_000*t.outputCostPer1M
}
