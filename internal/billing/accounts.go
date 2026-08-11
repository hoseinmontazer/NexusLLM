package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// ErrNoBillingAccount is returned when no billing account can be resolved
// for an org/project/api-key combination.
var ErrNoBillingAccount = errors.New("no billing account found for this request")

// AccountResolver resolves the billing_account_id for a request.
// Resolution order (first non-empty wins):
//  1. api_keys.billing_account_id
//  2. projects.billing_account_id
//  3. org default billing account (first active account for the org)
type AccountResolver struct {
	db *sqlx.DB
}

// NewAccountResolver constructs an AccountResolver.
func NewAccountResolver(db *sqlx.DB) *AccountResolver {
	return &AccountResolver{db: db}
}

// ResolveForRequest resolves billing_account_id, account_type, and currency
// for the given api key, project, and org.
// The resolved billing_account_id must be stored immutably in inference_usage
// at creation time and never re-inferred.
func (r *AccountResolver) ResolveForRequest(
	ctx context.Context,
	apiKeyID, projectID, orgID string,
) (accountID, accountType, currency string, err error) {

	// 1. API key override
	if apiKeyID != "" {
		row := r.db.QueryRowContext(ctx, `
			SELECT ba.id::text, ba.account_type, w.currency
			FROM api_keys ak
			JOIN billing_accounts ba ON ba.id = ak.billing_account_id
			LEFT JOIN wallets w ON w.billing_account_id = ba.id
			WHERE ak.id = $1::uuid
			  AND ak.billing_account_id IS NOT NULL
			  AND ba.status = 'active'
			LIMIT 1`, apiKeyID)
		if scanErr := row.Scan(&accountID, &accountType, &currency); scanErr == nil {
			return
		}
	}

	// 2. Project override
	if projectID != "" {
		row := r.db.QueryRowContext(ctx, `
			SELECT ba.id::text, ba.account_type, COALESCE(w.currency, ca.currency, 'USD')
			FROM projects p
			JOIN billing_accounts ba ON ba.id = p.billing_account_id
			LEFT JOIN wallets w ON w.billing_account_id = ba.id
			LEFT JOIN credit_accounts ca ON ca.billing_account_id = ba.id
			WHERE p.id = $1::uuid
			  AND p.billing_account_id IS NOT NULL
			  AND ba.status = 'active'
			LIMIT 1`, projectID)
		if scanErr := row.Scan(&accountID, &accountType, &currency); scanErr == nil {
			return
		}
	}

	// 3. Org default
	if orgID != "" {
		row := r.db.QueryRowContext(ctx, `
			SELECT ba.id::text, ba.account_type, COALESCE(w.currency, ca.currency, 'USD')
			FROM billing_accounts ba
			LEFT JOIN wallets w ON w.billing_account_id = ba.id
			LEFT JOIN credit_accounts ca ON ca.billing_account_id = ba.id
			WHERE ba.org_id = $1::uuid
			  AND ba.status = 'active'
			ORDER BY ba.created_at ASC
			LIMIT 1`, orgID)
		if scanErr := row.Scan(&accountID, &accountType, &currency); scanErr == nil {
			return
		}
	}

	return "", "", "", ErrNoBillingAccount
}

// GetWalletID returns the wallet ID for a prepaid billing account.
func GetWalletID(ctx context.Context, db *sqlx.DB, billingAccountID string) (string, error) {
	var walletID string
	err := db.QueryRowContext(ctx,
		`SELECT id::text FROM wallets WHERE billing_account_id = $1::uuid`,
		billingAccountID,
	).Scan(&walletID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("no wallet for billing account %s", billingAccountID)
	}
	return walletID, err
}

// GetCreditAccountID returns the credit account ID for a postpaid billing account.
func GetCreditAccountID(ctx context.Context, db *sqlx.DB, billingAccountID string) (string, error) {
	var creditID string
	err := db.QueryRowContext(ctx,
		`SELECT id::text FROM credit_accounts WHERE billing_account_id = $1::uuid`,
		billingAccountID,
	).Scan(&creditID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("no credit account for billing account %s", billingAccountID)
	}
	return creditID, err
}
