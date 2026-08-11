package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

// ErrInsufficientFunds is returned when a prepaid wallet has insufficient
// available balance for a reservation.
var ErrInsufficientFunds = errors.New("insufficient wallet balance")

// ErrCreditLimitExceeded is returned when a postpaid account would exceed
// its credit limit with the requested reservation.
var ErrCreditLimitExceeded = errors.New("credit limit exceeded")

// ErrAlreadyAuthorized is returned when an inference_usage_id already has
// an active billing authorization (idempotent detect).
var ErrAlreadyAuthorized = errors.New("billing authorization already exists for this inference")

// Reserver handles billing authorization (reservation) for both prepaid
// and postpaid accounts.
type Reserver struct {
	db  *sqlx.DB
	log *zap.Logger
	// How long authorizations stay active before the expiry sweep fires.
	authTTL time.Duration
}

// NewReserver constructs a Reserver.
func NewReserver(db *sqlx.DB, log *zap.Logger) *Reserver {
	return &Reserver{
		db:      db,
		log:     log,
		authTTL: 15 * time.Minute,
	}
}

// WithAuthTTL overrides the default 15-minute authorization TTL.
func (r *Reserver) WithAuthTTL(d time.Duration) *Reserver {
	r.authTTL = d
	return r
}

// ReserveInput contains all parameters needed to create a billing authorization.
type ReserveInput struct {
	InferenceUsageID string
	BillingAccountID string
	AccountType      string
	EstimatedCost    decimal.Decimal
	Currency         string
}

// ReserveOutput is returned by Reserve on success.
type ReserveOutput struct {
	AuthorizationID string
	ExpiresAt       time.Time
}

// Reserve creates a billing authorization for an inference request.
// For prepaid: validates available balance and increases wallet.reserved.
// For postpaid: validates credit limit and increases credit_accounts.total_exposure.
// Both paths use SELECT ... FOR UPDATE to serialize concurrent reservations.
//
// Idempotent: if an authorization already exists for this inference_usage_id,
// returns ErrAlreadyAuthorized with the existing auth ID.
func (r *Reserver) Reserve(ctx context.Context, in ReserveInput) (ReserveOutput, error) {
	expiresAt := time.Now().Add(r.authTTL)

	switch in.AccountType {
	case AccountTypePrepaid:
		return r.reservePrepaid(ctx, in, expiresAt)
	case AccountTypePostpaid:
		return r.reservePostpaid(ctx, in, expiresAt)
	default:
		return ReserveOutput{}, fmt.Errorf("unknown account_type: %s", in.AccountType)
	}
}

// reservePrepaid handles T-RESERVE-PREPAID.
func (r *Reserver) reservePrepaid(ctx context.Context, in ReserveInput, expiresAt time.Time) (ReserveOutput, error) {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return ReserveOutput{}, fmt.Errorf("begin reserve prepaid: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Lock wallet row exclusively — serializes concurrent reservations.
	var walletID, balanceStr, reservedStr string
	err = tx.QueryRowContext(ctx, `
		SELECT w.id::text, w.balance::text, w.reserved::text
		FROM wallets w
		JOIN billing_accounts ba ON ba.id = w.billing_account_id
		WHERE ba.id = $1::uuid AND ba.status = 'active'
		FOR UPDATE`,
		in.BillingAccountID,
	).Scan(&walletID, &balanceStr, &reservedStr)
	if err != nil {
		return ReserveOutput{}, fmt.Errorf("wallet lock for account %s: %w", in.BillingAccountID, err)
	}

	balance, _ := decimal.NewFromString(balanceStr)
	reserved, _ := decimal.NewFromString(reservedStr)
	available := balance.Sub(reserved)

	if available.LessThan(in.EstimatedCost) {
		return ReserveOutput{}, ErrInsufficientFunds
	}

	// Update wallet reserved
	_, err = tx.ExecContext(ctx, `
		UPDATE wallets
		SET reserved = reserved + $1, version = version + 1, updated_at = NOW()
		WHERE id = $2::uuid`,
		in.EstimatedCost, walletID,
	)
	if err != nil {
		return ReserveOutput{}, fmt.Errorf("update wallet reserved: %w", err)
	}

	// Create authorization
	authID := uuid.New().String()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO billing_authorizations
		  (id, inference_usage_id, billing_account_id, account_type,
		   estimated_cost, currency, status, expires_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, 'active', $7)`,
		authID, in.InferenceUsageID, in.BillingAccountID, in.AccountType,
		in.EstimatedCost, in.Currency, expiresAt,
	)
	if err != nil {
		if isDuplicateKey(err) {
			return ReserveOutput{}, ErrAlreadyAuthorized
		}
		return ReserveOutput{}, fmt.Errorf("insert billing_authorizations: %w", err)
	}

	// Update inference_usage billing_status
	_, err = tx.ExecContext(ctx, `
		UPDATE inference_usage
		SET billing_status = 'reserved'
		WHERE id = $1::uuid AND billing_status = 'pending'`,
		in.InferenceUsageID,
	)
	if err != nil {
		return ReserveOutput{}, fmt.Errorf("update inference_usage billing_status: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return ReserveOutput{}, fmt.Errorf("commit reserve prepaid: %w", err)
	}

	r.log.Debug("prepaid reservation created",
		zap.String("auth_id", authID),
		zap.String("inference_usage_id", in.InferenceUsageID),
		zap.String("estimated_cost", in.EstimatedCost.String()),
	)
	return ReserveOutput{AuthorizationID: authID, ExpiresAt: expiresAt}, nil
}

// reservePostpaid handles T-RESERVE-POSTPAID.
func (r *Reserver) reservePostpaid(ctx context.Context, in ReserveInput, expiresAt time.Time) (ReserveOutput, error) {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return ReserveOutput{}, fmt.Errorf("begin reserve postpaid: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Lock credit_accounts row exclusively.
	var creditID, limitStr, exposureStr string
	err = tx.QueryRowContext(ctx, `
		SELECT ca.id::text, ca.credit_limit::text, ca.total_exposure::text
		FROM credit_accounts ca
		JOIN billing_accounts ba ON ba.id = ca.billing_account_id
		WHERE ba.id = $1::uuid AND ba.status = 'active'
		FOR UPDATE`,
		in.BillingAccountID,
	).Scan(&creditID, &limitStr, &exposureStr)
	if err != nil {
		return ReserveOutput{}, fmt.Errorf("credit account lock for account %s: %w", in.BillingAccountID, err)
	}

	limit, _ := decimal.NewFromString(limitStr)
	exposure, _ := decimal.NewFromString(exposureStr)
	available := limit.Sub(exposure)

	if available.LessThan(in.EstimatedCost) {
		return ReserveOutput{}, ErrCreditLimitExceeded
	}

	// Increase total_exposure
	_, err = tx.ExecContext(ctx, `
		UPDATE credit_accounts
		SET total_exposure = total_exposure + $1, version = version + 1, updated_at = NOW()
		WHERE id = $2::uuid`,
		in.EstimatedCost, creditID,
	)
	if err != nil {
		return ReserveOutput{}, fmt.Errorf("update credit total_exposure: %w", err)
	}

	// Create authorization
	authID := uuid.New().String()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO billing_authorizations
		  (id, inference_usage_id, billing_account_id, account_type,
		   estimated_cost, currency, status, expires_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, 'active', $7)`,
		authID, in.InferenceUsageID, in.BillingAccountID, in.AccountType,
		in.EstimatedCost, in.Currency, expiresAt,
	)
	if err != nil {
		if isDuplicateKey(err) {
			return ReserveOutput{}, ErrAlreadyAuthorized
		}
		return ReserveOutput{}, fmt.Errorf("insert billing_authorizations postpaid: %w", err)
	}

	// Record in credit_ledger
	_, err = tx.ExecContext(ctx, `
		INSERT INTO credit_ledger
		  (credit_account_id, entry_type, amount, inference_usage_id, description)
		VALUES ($1::uuid, 'authorized', $2, $3::uuid, 'admission reservation')`,
		creditID, in.EstimatedCost, in.InferenceUsageID,
	)
	if err != nil {
		return ReserveOutput{}, fmt.Errorf("insert credit_ledger authorized: %w", err)
	}

	// Update inference_usage billing_status
	_, err = tx.ExecContext(ctx, `
		UPDATE inference_usage
		SET billing_status = 'reserved'
		WHERE id = $1::uuid AND billing_status = 'pending'`,
		in.InferenceUsageID,
	)
	if err != nil {
		return ReserveOutput{}, fmt.Errorf("update inference_usage billing_status postpaid: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return ReserveOutput{}, fmt.Errorf("commit reserve postpaid: %w", err)
	}

	r.log.Debug("postpaid reservation created",
		zap.String("auth_id", authID),
		zap.String("inference_usage_id", in.InferenceUsageID),
		zap.String("estimated_cost", in.EstimatedCost.String()),
	)
	return ReserveOutput{AuthorizationID: authID, ExpiresAt: expiresAt}, nil
}

// Release transitions an authorization from active → released.
// Called when inference fails or billing is rejected.
// Idempotent: if already released/committed/expired, returns nil.
func (r *Reserver) Release(ctx context.Context, authorizationID, inferenceUsageID string) error {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin release: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Conditional transition: active → released only
	var accountType, estimatedCostStr, billingAccountID string
	err = tx.QueryRowContext(ctx, `
		UPDATE billing_authorizations
		SET status = 'released', released_at = NOW()
		WHERE id = $1::uuid AND status = 'active'
		RETURNING account_type, estimated_cost::text, billing_account_id::text`,
		authorizationID,
	).Scan(&accountType, &estimatedCostStr, &billingAccountID)
	if err != nil {
		// Zero rows = already in terminal state. Idempotent success.
		return nil
	}

	estimatedCost, _ := decimal.NewFromString(estimatedCostStr)

	switch accountType {
	case AccountTypePrepaid:
		if err := r.releasePrepaidWallet(ctx, tx, billingAccountID, estimatedCost, authorizationID); err != nil {
			return err
		}
	case AccountTypePostpaid:
		if err := r.releasePostpaidCredit(ctx, tx, billingAccountID, estimatedCost, authorizationID, inferenceUsageID); err != nil {
			return err
		}
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE inference_usage SET billing_status = 'released'
		WHERE id = $1::uuid AND billing_status = 'reserved'`,
		inferenceUsageID,
	)
	if err != nil {
		return fmt.Errorf("update inference_usage billing_status released: %w", err)
	}

	return tx.Commit()
}

func (r *Reserver) releasePrepaidWallet(
	ctx context.Context, tx *sqlx.Tx,
	billingAccountID string, amount decimal.Decimal, authorizationID string,
) error {
	var walletID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id::text FROM wallets WHERE billing_account_id = $1::uuid`,
		billingAccountID,
	).Scan(&walletID); err != nil {
		return fmt.Errorf("wallet lookup for release: %w", err)
	}

	_, err := tx.ExecContext(ctx, `
		UPDATE wallets
		SET reserved = reserved - $1, version = version + 1, updated_at = NOW()
		WHERE id = $2::uuid`,
		amount, walletID,
	)
	if err != nil {
		return fmt.Errorf("update wallet reserved (release): %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO wallet_ledger
		  (wallet_id, entry_type, amount, authorization_id, description)
		VALUES ($1::uuid, 'release', $2, $3::uuid, 'reservation released')`,
		walletID, amount, authorizationID,
	)
	return err
}

func (r *Reserver) releasePostpaidCredit(
	ctx context.Context, tx *sqlx.Tx,
	billingAccountID string, amount decimal.Decimal,
	authorizationID, inferenceUsageID string,
) error {
	var creditID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id::text FROM credit_accounts WHERE billing_account_id = $1::uuid`,
		billingAccountID,
	).Scan(&creditID); err != nil {
		return fmt.Errorf("credit account lookup for release: %w", err)
	}

	_, err := tx.ExecContext(ctx, `
		UPDATE credit_accounts
		SET total_exposure = total_exposure - $1, version = version + 1, updated_at = NOW()
		WHERE id = $2::uuid`,
		amount, creditID,
	)
	if err != nil {
		return fmt.Errorf("update credit total_exposure (release): %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO credit_ledger
		  (credit_account_id, entry_type, amount, authorization_id, inference_usage_id, description)
		VALUES ($1::uuid, 'released', $2, $3::uuid, $4::uuid, 'reservation released')`,
		creditID, amount, authorizationID, inferenceUsageID,
	)
	return err
}

// isDuplicateKey returns true when the error is a PostgreSQL unique violation.
func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	return contains(err.Error(), "duplicate key") || contains(err.Error(), "unique constraint")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
