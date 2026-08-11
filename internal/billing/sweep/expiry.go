package sweep

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

// ExpirySweep expires billing_authorizations that have passed their
// expires_at TTL without being committed or released.
//
// For each expired authorization it:
//  1. Conditionally transitions active → expired (guarded by DB trigger).
//  2. Releases reserved funds from wallet.reserved or credit_accounts.total_exposure.
//  3. Inserts a wallet_ledger or credit_ledger release entry.
//  4. Updates inference_usage.billing_status = 'released'.
//
// Run every 2 minutes.
type ExpirySweep struct {
	db  *sqlx.DB
	log *zap.Logger
}

// NewExpirySweep constructs an ExpirySweep.
func NewExpirySweep(db *sqlx.DB, log *zap.Logger) *ExpirySweep {
	return &ExpirySweep{db: db, log: log}
}

// Run executes one expiry sweep pass.
func (s *ExpirySweep) Run(ctx context.Context) error {
	type expiredAuth struct {
		ID               string  `db:"id"`
		InferenceUsageID string  `db:"inference_usage_id"`
		BillingAccountID string  `db:"billing_account_id"`
		AccountType      string  `db:"account_type"`
		EstimatedCost    string  `db:"estimated_cost"`
		Currency         string  `db:"currency"`
	}

	// Find all active authorizations past their expiry.
	// Process up to 200 per sweep to bound execution time.
	var expired []expiredAuth
	err := s.db.SelectContext(ctx, &expired, `
		SELECT id::text, inference_usage_id::text, billing_account_id::text,
		       account_type, estimated_cost::text, currency
		FROM billing_authorizations
		WHERE status = 'active' AND expires_at < NOW()
		ORDER BY expires_at ASC
		LIMIT 200`,
	)
	if err != nil {
		return fmt.Errorf("expiry sweep query: %w", err)
	}

	processed, skipped, errCount := 0, 0, 0
	for _, auth := range expired {
		if ctx.Err() != nil {
			break
		}
		switch s.expireOne(ctx, auth.ID, auth.InferenceUsageID, auth.BillingAccountID, auth.AccountType, auth.EstimatedCost, auth.Currency) {
		case "expired":
			processed++
		case "skipped":
			skipped++
		default:
			errCount++
		}
	}

	if len(expired) > 0 {
		s.log.Info("expiry sweep completed",
			zap.Int("total", len(expired)),
			zap.Int("expired", processed),
			zap.Int("skipped", skipped),
			zap.Int("errors", errCount),
		)
	}
	return nil
}

// Start runs the sweep on a ticker. Blocks until ctx is cancelled.
func (s *ExpirySweep) Start(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.log.Info("expiry sweep started", zap.Duration("interval", interval))
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Run(ctx); err != nil {
				s.log.Error("expiry sweep error", zap.Error(err))
			}
		}
	}
}

func (s *ExpirySweep) expireOne(
	ctx context.Context,
	authID, usageID, billingAccountID, accountType, estimatedCostStr, currency string,
) string {
	// T-EXPIRE: one atomic transaction.
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		s.log.Error("expiry: begin tx failed", zap.String("auth_id", authID), zap.Error(err))
		return "error"
	}
	defer tx.Rollback() //nolint:errcheck

	// Conditional transition: active → expired.
	// The DB trigger fn_guard_auth_transition prevents invalid re-transitions.
	var confirmedEstimated string
	err = tx.QueryRowContext(ctx, `
		UPDATE billing_authorizations
		SET status = 'expired', expired_at = NOW()
		WHERE id = $1::uuid AND status = 'active'
		RETURNING estimated_cost::text`, authID,
	).Scan(&confirmedEstimated)
	if err != nil {
		// 0 rows: already transitioned (committed/released by concurrent path).
		return "skipped"
	}

	estimatedCost, _ := decimal.NewFromString(confirmedEstimated)

	// Release reserved funds based on account type.
	switch accountType {
	case "prepaid":
		if err := s.releasePrepaid(ctx, tx, billingAccountID, estimatedCost, authID); err != nil {
			s.log.Error("expiry: prepaid release failed",
				zap.String("auth_id", authID), zap.Error(err))
			return "error"
		}
	case "postpaid":
		if err := s.releasePostpaid(ctx, tx, billingAccountID, estimatedCost, authID, usageID); err != nil {
			s.log.Error("expiry: postpaid release failed",
				zap.String("auth_id", authID), zap.Error(err))
			return "error"
		}
	}

	// Update inference_usage billing_status
	_, _ = tx.ExecContext(ctx, `
		UPDATE inference_usage
		SET billing_status = 'released'
		WHERE id = $1::uuid AND billing_status = 'reserved'`, usageID,
	)

	if err := tx.Commit(); err != nil {
		s.log.Error("expiry: commit failed", zap.String("auth_id", authID), zap.Error(err))
		return "error"
	}

	s.log.Info("authorization expired and funds released",
		zap.String("auth_id", authID),
		zap.String("usage_id", usageID),
		zap.String("amount", estimatedCost.String()),
	)
	return "expired"
}

func (s *ExpirySweep) releasePrepaid(
	ctx context.Context, tx *sqlx.Tx,
	billingAccountID string, amount decimal.Decimal, authID string,
) error {
	var walletID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id::text FROM wallets WHERE billing_account_id = $1::uuid`,
		billingAccountID,
	).Scan(&walletID); err != nil {
		return fmt.Errorf("wallet lookup: %w", err)
	}

	_, err := tx.ExecContext(ctx, `
		UPDATE wallets
		SET reserved = reserved - $1, version = version + 1, updated_at = NOW()
		WHERE id = $2::uuid`,
		amount, walletID,
	)
	if err != nil {
		return fmt.Errorf("wallet reserved decrement: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO wallet_ledger
		  (wallet_id, entry_type, amount, authorization_id, description)
		VALUES ($1::uuid, 'release', $2, $3::uuid, 'authorization expired — funds released')`,
		walletID, amount, authID,
	)
	return err
}

func (s *ExpirySweep) releasePostpaid(
	ctx context.Context, tx *sqlx.Tx,
	billingAccountID string, amount decimal.Decimal,
	authID, usageID string,
) error {
	var creditID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id::text FROM credit_accounts WHERE billing_account_id = $1::uuid`,
		billingAccountID,
	).Scan(&creditID); err != nil {
		return fmt.Errorf("credit account lookup: %w", err)
	}

	_, err := tx.ExecContext(ctx, `
		UPDATE credit_accounts
		SET total_exposure = total_exposure - $1, version = version + 1, updated_at = NOW()
		WHERE id = $2::uuid`,
		amount, creditID,
	)
	if err != nil {
		return fmt.Errorf("credit exposure decrement: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO credit_ledger
		  (credit_account_id, entry_type, amount, authorization_id, inference_usage_id, description)
		VALUES ($1::uuid, 'released', $2, $3::uuid, $4::uuid, 'authorization expired')`,
		creditID, amount, authID, usageID,
	)
	return err
}
