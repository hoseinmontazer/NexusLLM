// Package sweep contains background reconciliation and cleanup jobs for
// the NexusLLM billing subsystem.
//
// Jobs:
//   - PendingSweep: reconciles stale pending/running inference_usage rows
//   - ExpirySweep:  expires timed-out billing authorizations
//   - WalletReconciler: detects and repairs wallet balance drift
//   - RedisRebuilder: rebuilds admission Redis counters from PostgreSQL
package sweep

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/billing"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

// PendingSweep reconciles inference_usage rows that are stuck in
// 'pending' or 'running' status after the staleness threshold.
//
// For each stale row it:
//  1. Queries inference_completions for evidence (source hierarchy: provider > worker > gateway).
//  2. If evidence found: finalizes usage and triggers billing settlement.
//  3. If no evidence: marks execution_status='unknown', billing_status='disputed',
//     releases the authorization.
//
// Run every 5 minutes.
type PendingSweep struct {
	db             *sqlx.DB
	settler        *billing.Settler
	reserver       *billing.Reserver
	log            *zap.Logger
	stalenessLimit time.Duration // default: 15 minutes
}

// NewPendingSweep constructs a PendingSweep.
func NewPendingSweep(db *sqlx.DB, settler *billing.Settler, reserver *billing.Reserver, log *zap.Logger) *PendingSweep {
	return &PendingSweep{
		db:             db,
		settler:        settler,
		reserver:       reserver,
		log:            log,
		stalenessLimit: 15 * time.Minute,
	}
}

// WithStalenessLimit overrides the default 15-minute staleness threshold.
func (s *PendingSweep) WithStalenessLimit(d time.Duration) *PendingSweep {
	s.stalenessLimit = d
	return s
}

// Run executes one sweep pass. Safe to call concurrently — rows are processed
// individually and updates are conditional on current status.
func (s *PendingSweep) Run(ctx context.Context) error {
	cutoff := time.Now().Add(-s.stalenessLimit)

	type staleRow struct {
		ID               string  `db:"id"`
		BillingAccountID *string `db:"billing_account_id"`
		AccountType      *string `db:"account_type"`
		BillingStatus    string  `db:"billing_status"`
		Currency         string  `db:"currency"`
		InputRate        string  `db:"input_rate"`
		OutputRate       string  `db:"output_rate"`
		CachedRate       string  `db:"cached_rate"`
	}

	var rows []staleRow
	err := s.db.SelectContext(ctx, &rows, `
		SELECT iu.id::text,
		       iu.billing_account_id::text,
		       ba.account_type,
		       iu.billing_status,
		       COALESCE(iu.currency, 'USD')    AS currency,
		       iu.input_rate::text,
		       iu.output_rate::text,
		       iu.cached_rate::text
		FROM inference_usage iu
		LEFT JOIN billing_accounts ba ON ba.id = iu.billing_account_id
		WHERE iu.execution_status IN ('pending', 'running')
		  AND iu.started_at < $1
		ORDER BY iu.started_at ASC
		LIMIT 500`, cutoff,
	)
	if err != nil {
		return fmt.Errorf("pending sweep query: %w", err)
	}

	resolved, disputed, failed := 0, 0, 0
	for _, row := range rows {
		if ctx.Err() != nil {
			break
		}
		switch s.processRow(ctx, row.ID, row.BillingAccountID, row.AccountType, row.BillingStatus, row.Currency, row.InputRate, row.OutputRate, row.CachedRate) {
		case "resolved":
			resolved++
		case "disputed":
			disputed++
		default:
			failed++
		}
	}

	s.log.Info("pending sweep completed",
		zap.Int("total", len(rows)),
		zap.Int("resolved", resolved),
		zap.Int("disputed", disputed),
		zap.Int("failed", failed),
	)
	return nil
}

// Start runs the sweep on a ticker. Blocks until ctx is cancelled.
func (s *PendingSweep) Start(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.log.Info("pending sweep started", zap.Duration("interval", interval))
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Run(ctx); err != nil {
				s.log.Error("pending sweep error", zap.Error(err))
			}
		}
	}
}

// evidencePriority maps source to sort order (lower = higher priority).
var evidencePriority = map[string]int{
	"provider_reconcile": 1,
	"worker":             2,
	"gateway":            3,
	"manual":             4,
}

type completionEvidence struct {
	Source       string
	InputTokens  int
	OutputTokens int
	CachedTokens int
	FinishReason string
}

func (s *PendingSweep) processRow(
	ctx context.Context,
	usageID string,
	billingAccountID, accountType *string,
	billingStatus, currency, inputRateStr, outputRateStr, cachedRateStr string,
) string {
	// Query all completion evidence for this usage row.
	type evidenceRow struct {
		Source       string `db:"source"`
		InputTokens  int    `db:"input_tokens"`
		OutputTokens int    `db:"output_tokens"`
		CachedTokens int    `db:"cached_tokens"`
		FinishReason string `db:"finish_reason"`
	}
	var evidenceRows []evidenceRow
	_ = s.db.SelectContext(ctx, &evidenceRows, `
		SELECT source, input_tokens, output_tokens, cached_tokens,
		       COALESCE(finish_reason, '') AS finish_reason
		FROM inference_completions
		WHERE inference_usage_id = $1::uuid`, usageID,
	)

	// Apply evidence hierarchy.
	var best *completionEvidence
	for _, e := range evidenceRows {
		ev := &completionEvidence{
			Source:       e.Source,
			InputTokens:  e.InputTokens,
			OutputTokens: e.OutputTokens,
			CachedTokens: e.CachedTokens,
			FinishReason: e.FinishReason,
		}
		if best == nil || evidencePriority[e.Source] < evidencePriority[best.Source] {
			best = ev
		}
	}

	if best == nil {
		// No evidence — mark unknown/disputed, release authorization.
		s.markUnknown(ctx, usageID, billingStatus)
		return "disputed"
	}

	// Determine is_billable from finish_reason and source.
	isBillable := best.OutputTokens > 0

	// Compute actual cost.
	inputRate, _ := decimal.NewFromString(inputRateStr)
	outputRate, _ := decimal.NewFromString(outputRateStr)
	cachedRate, _ := decimal.NewFromString(cachedRateStr)

	snap := billing.PricingSnapshot{
		InputRate:  inputRate,
		OutputRate: outputRate,
		CachedRate: cachedRate,
		Currency:   currency,
	}
	tokens := billing.TokenCounts{
		InputTokens:  best.InputTokens,
		OutputTokens: best.OutputTokens,
		CachedTokens: best.CachedTokens,
	}
	_, _, _, totalCost := billing.ComputeCost(snap, tokens)

	// Finalize the inference_usage row.
	execStatus := billing.ExecCompleted
	if best.FinishReason == "error" || best.FinishReason == "timeout" {
		execStatus = billing.ExecPartial
	}

	_, err := s.db.ExecContext(ctx, `
		UPDATE inference_usage
		SET execution_status  = $1,
		    input_tokens       = $2,
		    output_tokens      = $3,
		    cached_tokens      = $4,
		    total_cost         = $5,
		    is_billable        = $6,
		    finish_reason      = $7,
		    completed_at       = NOW()
		WHERE id = $8::uuid
		  AND execution_status IN ('pending','running')`,
		execStatus,
		best.InputTokens, best.OutputTokens, best.CachedTokens,
		totalCost, isBillable, best.FinishReason,
		usageID,
	)
	if err != nil {
		s.log.Error("pending sweep: failed to finalize usage row",
			zap.String("usage_id", usageID), zap.Error(err))
		return "error"
	}

	if !isBillable || billingAccountID == nil || accountType == nil || billingStatus != billing.BillingReserved {
		// Not billable or no billing account — release reservation if exists.
		s.releaseIfReserved(ctx, usageID)
		return "resolved"
	}

	// Find the authorization and settle.
	var authID string
	err = s.db.QueryRowContext(ctx,
		`SELECT id::text FROM billing_authorizations
		 WHERE inference_usage_id = $1::uuid AND status = 'active'
		 LIMIT 1`, usageID,
	).Scan(&authID)
	if errors.Is(err, sql.ErrNoRows) {
		// Auth already transitioned — handle via expiry recovery path.
		s.log.Warn("pending sweep: no active auth found, skipping settlement",
			zap.String("usage_id", usageID))
		return "resolved"
	}

	settleErr := s.settler.Settle(ctx, billing.SettleInput{
		AuthorizationID:  authID,
		InferenceUsageID: usageID,
		BillingAccountID: *billingAccountID,
		AccountType:      *accountType,
		ActualCost:       totalCost,
		Currency:         currency,
	})
	if settleErr != nil {
		s.log.Error("pending sweep: settlement failed",
			zap.String("usage_id", usageID), zap.Error(settleErr))
		return "error"
	}

	s.log.Info("pending sweep: inference resolved and settled",
		zap.String("usage_id", usageID),
		zap.String("source", best.Source),
		zap.String("total_cost", totalCost.String()),
	)
	return "resolved"
}

func (s *PendingSweep) markUnknown(ctx context.Context, usageID, billingStatus string) {
	_, _ = s.db.ExecContext(ctx, `
		UPDATE inference_usage
		SET execution_status = 'unknown',
		    billing_status   = 'disputed',
		    completed_at     = NOW()
		WHERE id = $1::uuid
		  AND execution_status IN ('pending','running')`, usageID,
	)
	// Release the authorization to free reserved funds.
	s.releaseIfReserved(ctx, usageID)
	s.log.Warn("pending sweep: no evidence found — marked disputed",
		zap.String("usage_id", usageID),
	)
}

func (s *PendingSweep) releaseIfReserved(ctx context.Context, usageID string) {
	var authID string
	err := s.db.QueryRowContext(ctx,
		`SELECT id::text FROM billing_authorizations
		 WHERE inference_usage_id = $1::uuid AND status = 'active'
		 LIMIT 1`, usageID,
	).Scan(&authID)
	if errors.Is(err, sql.ErrNoRows) {
		return // No active auth to release.
	}
	if err != nil {
		s.log.Warn("pending sweep: auth lookup error during release",
			zap.String("usage_id", usageID), zap.Error(err))
		return
	}
	if releaseErr := s.reserver.Release(ctx, authID, usageID); releaseErr != nil {
		s.log.Warn("pending sweep: release failed",
			zap.String("auth_id", authID), zap.Error(releaseErr))
	}
}
