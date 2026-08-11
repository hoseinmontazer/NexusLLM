package sweep

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

// WalletReconciler verifies that wallet.balance matches the ledger sum
// and that wallet.reserved matches the sum of active authorizations.
//
// If drift is detected:
//   - A wallet_ledger 'adjustment' entry is inserted (auditable).
//   - wallet.balance is corrected to match the ledger.
//   - wallet.reserved is corrected to match active authorizations.
//
// Run every 1 hour.
type WalletReconciler struct {
	db  *sqlx.DB
	log *zap.Logger
	// driftAlertThreshold is the minimum drift that triggers a warning.
	// Smaller drifts are corrected silently (float precision noise).
	driftAlertThreshold decimal.Decimal
}

// NewWalletReconciler constructs a WalletReconciler.
func NewWalletReconciler(db *sqlx.DB, log *zap.Logger) *WalletReconciler {
	return &WalletReconciler{
		db:                  db,
		log:                 log,
		driftAlertThreshold: decimal.NewFromFloat(0.000001), // $0.000001 minimum
	}
}

// Run executes one reconciliation pass across all wallets.
func (r *WalletReconciler) Run(ctx context.Context) error {
	type walletRow struct {
		ID               string `db:"id"`
		BillingAccountID string `db:"billing_account_id"`
		Balance          string `db:"balance"`
		Reserved         string `db:"reserved"`
	}

	var wallets []walletRow
	if err := r.db.SelectContext(ctx, &wallets,
		`SELECT id::text, billing_account_id::text, balance::text, reserved::text FROM wallets`,
	); err != nil {
		return fmt.Errorf("wallet reconciler: list wallets: %w", err)
	}

	diverged := 0
	for _, w := range wallets {
		if ctx.Err() != nil {
			break
		}
		if err := r.reconcileWallet(ctx, w.ID, w.BillingAccountID, w.Balance, w.Reserved); err != nil {
			r.log.Error("wallet reconciliation error",
				zap.String("wallet_id", w.ID), zap.Error(err))
			diverged++
		}
	}

	r.log.Info("wallet reconciliation completed",
		zap.Int("total", len(wallets)),
		zap.Int("diverged", diverged),
	)
	return nil
}

// Start runs the reconciler on a ticker. Blocks until ctx is cancelled.
func (r *WalletReconciler) Start(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	r.log.Info("wallet reconciler started", zap.Duration("interval", interval))
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.Run(ctx); err != nil {
				r.log.Error("wallet reconciler error", zap.Error(err))
			}
		}
	}
}

func (r *WalletReconciler) reconcileWallet(
	ctx context.Context, walletID, billingAccountID, balanceStr, reservedStr string,
) error {
	currentBalance, _ := decimal.NewFromString(balanceStr)
	currentReserved, _ := decimal.NewFromString(reservedStr)

	// ── Compute correct balance from ledger ───────────────────────────────────
	// balance = SUM(topup + unplanned_debit) - SUM(debit)
	// Note: 'release' and 'adjustment' entries do NOT affect balance.
	var correctBalanceStr string
	err := r.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(
		    CASE entry_type
		        WHEN 'topup'           THEN  amount
		        WHEN 'debit'           THEN -amount
		        WHEN 'unplanned_debit' THEN -amount
		        ELSE 0
		    END
		), 0)::text
		FROM wallet_ledger WHERE wallet_id = $1::uuid`, walletID,
	).Scan(&correctBalanceStr)
	if err != nil {
		return fmt.Errorf("ledger sum query: %w", err)
	}
	correctBalance, _ := decimal.NewFromString(correctBalanceStr)

	// ── Compute correct reserved from active authorizations ───────────────────
	var correctReservedStr string
	err = r.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(estimated_cost), 0)::text
		FROM billing_authorizations
		WHERE billing_account_id = $1::uuid AND status = 'active'`, billingAccountID,
	).Scan(&correctReservedStr)
	if err != nil {
		return fmt.Errorf("active reservations sum query: %w", err)
	}
	correctReserved, _ := decimal.NewFromString(correctReservedStr)

	balanceDrift := correctBalance.Sub(currentBalance).Abs()
	reservedDrift := correctReserved.Sub(currentReserved).Abs()

	// No drift — nothing to do.
	if balanceDrift.LessThan(r.driftAlertThreshold) &&
		reservedDrift.LessThan(r.driftAlertThreshold) {
		return nil
	}

	if balanceDrift.GreaterThanOrEqual(r.driftAlertThreshold) {
		r.log.Warn("wallet balance drift detected",
			zap.String("wallet_id", walletID),
			zap.String("current", currentBalance.String()),
			zap.String("correct", correctBalance.String()),
			zap.String("drift", correctBalance.Sub(currentBalance).String()),
		)
	}

	// ── Repair under lock ─────────────────────────────────────────────────────
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reconcile tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Re-lock and re-compute under lock to catch concurrent mutations.
	var lockedBalanceStr, lockedReservedStr string
	err = tx.QueryRowContext(ctx,
		`SELECT balance::text, reserved::text FROM wallets WHERE id = $1::uuid FOR UPDATE`,
		walletID,
	).Scan(&lockedBalanceStr, &lockedReservedStr)
	if err != nil {
		return fmt.Errorf("wallet lock for reconcile: %w", err)
	}

	// Re-compute under lock
	var finalCorrectBalStr string
	_ = tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(
		    CASE entry_type
		        WHEN 'topup'           THEN  amount
		        WHEN 'debit'           THEN -amount
		        WHEN 'unplanned_debit' THEN -amount
		        ELSE 0
		    END
		), 0)::text
		FROM wallet_ledger WHERE wallet_id = $1::uuid`, walletID,
	).Scan(&finalCorrectBalStr)
	finalCorrectBalance, _ := decimal.NewFromString(finalCorrectBalStr)

	lockedBalance, _ := decimal.NewFromString(lockedBalanceStr)
	finalDrift := finalCorrectBalance.Sub(lockedBalance)

	if finalDrift.Abs().GreaterThanOrEqual(r.driftAlertThreshold) {
		// Insert an adjustment ledger entry (auditable — never silently modify).
		_, err = tx.ExecContext(ctx, `
			INSERT INTO wallet_ledger
			  (wallet_id, entry_type, amount, description)
			VALUES ($1::uuid, 'adjustment', $2,
			        'reconciliation correction: drift=' || $3::text)`,
			walletID,
			finalDrift.Abs(),
			finalDrift.String(),
		)
		if err != nil {
			return fmt.Errorf("insert adjustment ledger entry: %w", err)
		}
	}

	// Update both balance and reserved atomically.
	_, err = tx.ExecContext(ctx, `
		UPDATE wallets
		SET balance    = $1,
		    reserved   = $2,
		    version    = version + 1,
		    updated_at = NOW()
		WHERE id = $3::uuid`,
		finalCorrectBalance, correctReserved, walletID,
	)
	if err != nil {
		return fmt.Errorf("wallet balance/reserved correction: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reconcile commit: %w", err)
	}

	r.log.Info("wallet balance corrected",
		zap.String("wallet_id", walletID),
		zap.String("new_balance", finalCorrectBalance.String()),
		zap.String("new_reserved", correctReserved.String()),
	)
	return nil
}
