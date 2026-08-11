package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

// Settler handles billing settlement (commit) after inference completes.
// It also handles the expiry recovery path when authorization has already expired.
type Settler struct {
	db             *sqlx.DB
	log            *zap.Logger
	expiryPolicy   ExpiryRecoveryPolicy
}

// NewSettler constructs a Settler.
func NewSettler(db *sqlx.DB, log *zap.Logger, policy ExpiryRecoveryPolicy) *Settler {
	return &Settler{db: db, log: log, expiryPolicy: policy}
}

// SettleInput contains everything needed to finalize billing for an inference.
type SettleInput struct {
	AuthorizationID  string
	InferenceUsageID string
	BillingAccountID string
	AccountType      string
	ActualCost       decimal.Decimal
	Currency         string
}

// Settle commits the actual cost of an inference, transitioning the
// authorization from active → committed.
//
// The actual_cost is capped at estimated_cost (max-reservation policy).
// This prevents wallet balance going negative when a provider returns
// more tokens than max_tokens.
//
// If rows_affected == 0 (authorization already expired/released):
//   - PolicyBestEffort: attempts unplanned debit if funds available.
//   - PolicyWriteOff:  marks as disputed, logs, returns nil.
func (s *Settler) Settle(ctx context.Context, in SettleInput) error {
	switch in.AccountType {
	case AccountTypePrepaid:
		return s.settlePrepaid(ctx, in)
	case AccountTypePostpaid:
		return s.settlePostpaid(ctx, in)
	default:
		return fmt.Errorf("unknown account_type: %s", in.AccountType)
	}
}

// settlePrepaid runs T-COMMIT-PREPAID.
func (s *Settler) settlePrepaid(ctx context.Context, in SettleInput) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin settle prepaid: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Conditional state transition: active → committed
	var estimatedCostStr string
	err = tx.QueryRowContext(ctx, `
		UPDATE billing_authorizations
		SET status = 'committed',
		    actual_cost = $1,
		    committed_at = NOW()
		WHERE id = $2::uuid AND status = 'active'
		RETURNING estimated_cost::text`,
		in.ActualCost, in.AuthorizationID,
	).Scan(&estimatedCostStr)

	if err != nil {
		// Zero rows: auth already in terminal state
		tx.Rollback() //nolint:errcheck
		return s.handleExpiryRecovery(ctx, in)
	}

	estimatedCost, _ := decimal.NewFromString(estimatedCostStr)

	// Cap actual_cost at estimated_cost (max-reservation guarantee).
	actualCost := in.ActualCost
	if actualCost.GreaterThan(estimatedCost) {
		s.log.Warn("actual_cost exceeds estimated_cost — capping at reservation amount",
			zap.String("inference_usage_id", in.InferenceUsageID),
			zap.String("estimated", estimatedCost.String()),
			zap.String("actual", actualCost.String()),
		)
		actualCost = estimatedCost
	}

	// Get wallet ID
	var walletID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id::text FROM wallets WHERE billing_account_id = $1::uuid`,
		in.BillingAccountID,
	).Scan(&walletID); err != nil {
		return fmt.Errorf("wallet lookup for settlement: %w", err)
	}

	// Debit balance and release reserved (one atomic UPDATE).
	result, err := tx.ExecContext(ctx, `
		UPDATE wallets
		SET balance   = balance   - $1,
		    reserved  = reserved  - $2,
		    version   = version + 1,
		    updated_at = NOW()
		WHERE id = $3::uuid`,
		actualCost, estimatedCost, walletID,
	)
	if err != nil {
		return fmt.Errorf("update wallet on commit: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("wallet update affected 0 rows for wallet %s", walletID)
	}

	// Debit ledger entry
	_, err = tx.ExecContext(ctx, `
		INSERT INTO wallet_ledger
		  (wallet_id, entry_type, amount, inference_usage_id, authorization_id, description)
		VALUES ($1::uuid, 'debit', $2, $3::uuid, $4::uuid, 'inference settled')`,
		walletID, actualCost, in.InferenceUsageID, in.AuthorizationID,
	)
	if err != nil {
		return fmt.Errorf("insert wallet_ledger debit: %w", err)
	}

	// Release ledger entry for unused reservation delta
	delta := estimatedCost.Sub(actualCost)
	if delta.IsPositive() {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO wallet_ledger
			  (wallet_id, entry_type, amount, authorization_id, description)
			VALUES ($1::uuid, 'release', $2, $3::uuid, 'unused reservation released')`,
			walletID, delta, in.AuthorizationID,
		)
		if err != nil {
			return fmt.Errorf("insert wallet_ledger release: %w", err)
		}
	}

	// Update inference_usage
	_, err = tx.ExecContext(ctx, `
		UPDATE inference_usage
		SET billing_status = 'committed', total_cost = $1
		WHERE id = $2::uuid`,
		actualCost, in.InferenceUsageID,
	)
	if err != nil {
		return fmt.Errorf("update inference_usage committed: %w", err)
	}

	return tx.Commit()
}

// settlePostpaid runs T-COMMIT-POSTPAID.
func (s *Settler) settlePostpaid(ctx context.Context, in SettleInput) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin settle postpaid: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var estimatedCostStr string
	err = tx.QueryRowContext(ctx, `
		UPDATE billing_authorizations
		SET status = 'committed',
		    actual_cost = $1,
		    committed_at = NOW()
		WHERE id = $2::uuid AND status = 'active'
		RETURNING estimated_cost::text`,
		in.ActualCost, in.AuthorizationID,
	).Scan(&estimatedCostStr)

	if err != nil {
		tx.Rollback() //nolint:errcheck
		return s.handleExpiryRecovery(ctx, in)
	}

	estimatedCost, _ := decimal.NewFromString(estimatedCostStr)
	actualCost := in.ActualCost
	if actualCost.GreaterThan(estimatedCost) {
		actualCost = estimatedCost
	}

	var creditID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id::text FROM credit_accounts WHERE billing_account_id = $1::uuid`,
		in.BillingAccountID,
	).Scan(&creditID); err != nil {
		return fmt.Errorf("credit account lookup for settlement: %w", err)
	}

	// Settle: add to current_cycle_settled
	_, err = tx.ExecContext(ctx, `
		UPDATE credit_accounts
		SET current_cycle_settled = current_cycle_settled + $1,
		    version = version + 1,
		    updated_at = NOW()
		WHERE id = $2::uuid`,
		actualCost, creditID,
	)
	if err != nil {
		return fmt.Errorf("update credit cycle settled: %w", err)
	}

	// If actual < estimated: release the delta from total_exposure
	delta := estimatedCost.Sub(actualCost)
	if delta.IsPositive() {
		_, err = tx.ExecContext(ctx, `
			UPDATE credit_accounts
			SET total_exposure = total_exposure - $1
			WHERE id = $2::uuid`,
			delta, creditID,
		)
		if err != nil {
			return fmt.Errorf("reduce credit total_exposure delta: %w", err)
		}
	}

	// Settled ledger entry
	_, err = tx.ExecContext(ctx, `
		INSERT INTO credit_ledger
		  (credit_account_id, entry_type, amount, inference_usage_id, authorization_id, description)
		VALUES ($1::uuid, 'settled', $2, $3::uuid, $4::uuid, 'inference settled')`,
		creditID, actualCost, in.InferenceUsageID, in.AuthorizationID,
	)
	if err != nil {
		return fmt.Errorf("insert credit_ledger settled: %w", err)
	}

	// Release ledger entry for unused delta
	if delta.IsPositive() {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO credit_ledger
			  (credit_account_id, entry_type, amount, authorization_id, description)
			VALUES ($1::uuid, 'released', $2, $3::uuid, 'unused reservation released')`,
			creditID, delta, in.AuthorizationID,
		)
		if err != nil {
			return fmt.Errorf("insert credit_ledger released: %w", err)
		}
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE inference_usage
		SET billing_status = 'committed', total_cost = $1
		WHERE id = $2::uuid`,
		actualCost, in.InferenceUsageID,
	)
	if err != nil {
		return fmt.Errorf("update inference_usage committed postpaid: %w", err)
	}

	return tx.Commit()
}

// handleExpiryRecovery is called when the authorization has already
// transitioned out of 'active' before settlement could run.
// This happens when the expiry sweep fires concurrently with settlement.
func (s *Settler) handleExpiryRecovery(ctx context.Context, in SettleInput) error {
	// Check current auth status
	var currentStatus string
	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM billing_authorizations WHERE id = $1::uuid`,
		in.AuthorizationID,
	).Scan(&currentStatus)
	if err != nil {
		return fmt.Errorf("lookup auth status for recovery: %w", err)
	}

	if currentStatus == AuthStatusCommitted {
		// Already committed by another worker — idempotent success.
		s.log.Debug("settlement already committed by another worker",
			zap.String("auth_id", in.AuthorizationID),
		)
		return nil
	}

	// Auth is released or expired. Apply recovery policy.
	switch s.expiryPolicy {
	case PolicyBestEffort:
		return s.unplannedDebit(ctx, in)
	default: // PolicyWriteOff
		s.log.Warn("auth expired before settlement — marking disputed (write_off policy)",
			zap.String("auth_id", in.AuthorizationID),
			zap.String("inference_usage_id", in.InferenceUsageID),
			zap.String("actual_cost", in.ActualCost.String()),
		)
		_, _ = s.db.ExecContext(ctx,
			`UPDATE inference_usage SET billing_status = 'disputed' WHERE id = $1::uuid`,
			in.InferenceUsageID,
		)
		return nil
	}
}

// unplannedDebit attempts a direct debit when the authorization has expired
// but inference completed and funds may still be available.
// Only runs under PolicyBestEffort.
func (s *Settler) unplannedDebit(ctx context.Context, in SettleInput) error {
	if in.AccountType != AccountTypePrepaid {
		// Postpaid: just mark disputed. We cannot create a retroactive credit entry.
		_, _ = s.db.ExecContext(ctx,
			`UPDATE inference_usage SET billing_status = 'disputed' WHERE id = $1::uuid`,
			in.InferenceUsageID,
		)
		return nil
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin unplanned_debit: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Lock wallet and check if funds are available
	var walletID, balanceStr, reservedStr string
	err = tx.QueryRowContext(ctx, `
		SELECT w.id::text, w.balance::text, w.reserved::text
		FROM wallets w
		WHERE w.billing_account_id = $1::uuid
		FOR UPDATE`,
		in.BillingAccountID,
	).Scan(&walletID, &balanceStr, &reservedStr)
	if err != nil {
		return fmt.Errorf("wallet lock for unplanned debit: %w", err)
	}

	balance, _ := decimal.NewFromString(balanceStr)
	reserved, _ := decimal.NewFromString(reservedStr)
	available := balance.Sub(reserved)

	if available.LessThan(in.ActualCost) {
		// Insufficient funds even for unplanned debit — mark disputed
		tx.Rollback() //nolint:errcheck
		s.log.Warn("insufficient funds for unplanned debit — marking disputed",
			zap.String("inference_usage_id", in.InferenceUsageID),
			zap.String("available", available.String()),
			zap.String("actual_cost", in.ActualCost.String()),
		)
		_, _ = s.db.ExecContext(ctx,
			`UPDATE inference_usage SET billing_status = 'disputed' WHERE id = $1::uuid`,
			in.InferenceUsageID,
		)
		return nil
	}

	// Debit balance directly (no reservation to release)
	_, err = tx.ExecContext(ctx, `
		UPDATE wallets
		SET balance = balance - $1, version = version + 1, updated_at = NOW()
		WHERE id = $2::uuid`,
		in.ActualCost, walletID,
	)
	if err != nil {
		return fmt.Errorf("update wallet unplanned debit: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO wallet_ledger
		  (wallet_id, entry_type, amount, inference_usage_id, description)
		VALUES ($1::uuid, 'unplanned_debit', $2, $3::uuid, 'late settlement after auth expiry')`,
		walletID, in.ActualCost, in.InferenceUsageID,
	)
	if err != nil {
		return fmt.Errorf("insert unplanned_debit ledger: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE inference_usage
		SET billing_status = 'committed', total_cost = $1
		WHERE id = $2::uuid`,
		in.ActualCost, in.InferenceUsageID,
	)
	if err != nil {
		return fmt.Errorf("update inference_usage unplanned_debit: %w", err)
	}

	s.log.Info("unplanned debit applied (best_effort recovery)",
		zap.String("inference_usage_id", in.InferenceUsageID),
		zap.String("amount", in.ActualCost.String()),
		zap.Time("now", time.Now()),
	)
	return tx.Commit()
}
