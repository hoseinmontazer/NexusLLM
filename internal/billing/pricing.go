package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/shopspring/decimal"
)

// ErrNoPricing is returned when no active pricing row exists for a model.
var ErrNoPricing = errors.New("no active pricing found for this model")

// PricingResolver resolves and snapshots model pricing at admission time.
type PricingResolver struct {
	db *sqlx.DB
}

// NewPricingResolver constructs a PricingResolver.
func NewPricingResolver(db *sqlx.DB) *PricingResolver {
	return &PricingResolver{db: db}
}

// SnapshotForModel fetches the currently active pricing for a model and
// returns an immutable PricingSnapshot. The caller must store the snapshot
// inline in inference_usage immediately — settlement must use the stored
// rates, never re-query model_pricing.
//
// If no pricing row exists, a zero-cost snapshot is returned so the request
// can proceed without billing (appropriate for internal/free-tier models).
func (r *PricingResolver) SnapshotForModel(ctx context.Context, modelID string) (PricingSnapshot, error) {
	type row struct {
		ID           string  `db:"id"`
		InputPerToken  string  `db:"input_per_token"`
		OutputPerToken string  `db:"output_per_token"`
		CachedPerToken string  `db:"cached_per_token"`
		Currency     string  `db:"currency"`
	}
	var pr row
	err := r.db.QueryRowxContext(ctx, `
		SELECT id::text, input_per_token::text, output_per_token::text,
		       cached_per_token::text, currency
		FROM model_pricing
		WHERE model_id = $1::uuid
		  AND effective_to IS NULL
		LIMIT 1`, modelID,
	).StructScan(&pr)

	if errors.Is(err, sql.ErrNoRows) {
		// No pricing configured — return zero-cost snapshot.
		// This allows the request to proceed; the model is effectively free.
		return PricingSnapshot{
			Currency:   "USD",
			ResolvedAt: time.Now(),
		}, nil
	}
	if err != nil {
		return PricingSnapshot{}, fmt.Errorf("pricing lookup for model %s: %w", modelID, err)
	}

	inputRate, err := decimal.NewFromString(pr.InputPerToken)
	if err != nil {
		return PricingSnapshot{}, fmt.Errorf("invalid input_per_token for model %s: %w", modelID, err)
	}
	outputRate, err := decimal.NewFromString(pr.OutputPerToken)
	if err != nil {
		return PricingSnapshot{}, fmt.Errorf("invalid output_per_token for model %s: %w", modelID, err)
	}
	cachedRate, err := decimal.NewFromString(pr.CachedPerToken)
	if err != nil {
		return PricingSnapshot{}, fmt.Errorf("invalid cached_per_token for model %s: %w", modelID, err)
	}

	return PricingSnapshot{
		PricingID:  pr.ID,
		InputRate:  inputRate,
		OutputRate: outputRate,
		CachedRate: cachedRate,
		Currency:   pr.Currency,
		ResolvedAt: time.Now(),
	}, nil
}

// DefaultMaxOutputTokens returns the model's configured default_max_output_tokens,
// falling back to 2048 if the model has no policy row or the column is zero.
func DefaultMaxOutputTokens(ctx context.Context, db *sqlx.DB, modelID string) int {
	var v int
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(default_max_output_tokens, 2048)
		 FROM model_policies WHERE model_id = $1::uuid`, modelID,
	).Scan(&v)
	if err != nil || v <= 0 {
		return 2048
	}
	return v
}
