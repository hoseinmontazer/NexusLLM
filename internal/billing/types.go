// Package billing provides financial authorization, settlement, and
// reconciliation for NexusLLM inference requests.
//
// Architecture:
//   - All financial operations use PostgreSQL with FOR UPDATE row locking.
//   - Redis has zero involvement in billing decisions.
//   - wallet_ledger and credit_ledger are append-only.
//   - billing_authorizations follow a strict state machine enforced by DB trigger.
//   - actual_cost is always capped at estimated_cost (max-reservation policy).
package billing

import (
	"time"

	"github.com/shopspring/decimal"
)

// ── Authorization states ──────────────────────────────────────────────────────

const (
	AuthStatusActive    = "active"
	AuthStatusCommitted = "committed"
	AuthStatusReleased  = "released"
	AuthStatusExpired   = "expired"
)

// ── Execution states ──────────────────────────────────────────────────────────

const (
	ExecPending   = "pending"
	ExecRunning   = "running"
	ExecCompleted = "completed"
	ExecPartial   = "partial"
	ExecFailed    = "failed"
	ExecUnknown   = "unknown"
)

// ── Billing states ────────────────────────────────────────────────────────────

const (
	BillingNotApplicable = "not_applicable"
	BillingPending       = "pending"
	BillingReserved      = "reserved"
	BillingCommitted     = "committed"
	BillingReleased      = "released"
	BillingDisputed      = "disputed"
)

// ── Ledger entry types ────────────────────────────────────────────────────────

const (
	LedgerTopup          = "topup"
	LedgerDebit          = "debit"
	LedgerRelease        = "release"
	LedgerAdjustment     = "adjustment"
	LedgerUnplannedDebit = "unplanned_debit"

	CreditLedgerAuthorized = "authorized"
	CreditLedgerSettled    = "settled"
	CreditLedgerReleased   = "released"
	CreditLedgerCycleReset = "cycle_reset"
	CreditLedgerAdjustment = "adjustment"
)

// ── Account types ─────────────────────────────────────────────────────────────

const (
	AccountTypePrepaid  = "prepaid"
	AccountTypePostpaid = "postpaid"
)

// ExpiryRecoveryPolicy controls what happens when a billing authorization
// expires but the inference completed successfully.
type ExpiryRecoveryPolicy string

const (
	// PolicyBestEffort attempts to bill the customer if funds are available.
	PolicyBestEffort ExpiryRecoveryPolicy = "best_effort"
	// PolicyWriteOff accepts the revenue loss and marks as disputed.
	PolicyWriteOff ExpiryRecoveryPolicy = "write_off"
)

// PricingSnapshot holds the pricing rates captured at admission time.
// These are stored inline in inference_usage and are immutable after creation.
type PricingSnapshot struct {
	PricingID    string          // UUID of model_pricing row used
	InputRate    decimal.Decimal // per token
	OutputRate   decimal.Decimal // per token
	CachedRate   decimal.Decimal // per token (for cached input)
	Currency     string
	ResolvedAt   time.Time
}

// Authorization is a billing_authorizations row.
type Authorization struct {
	ID               string
	InferenceUsageID string
	BillingAccountID string
	AccountType      string
	EstimatedCost    decimal.Decimal
	ActualCost       *decimal.Decimal
	Currency         string
	Status           string
	ExpiresAt        time.Time
	CommittedAt      *time.Time
	ReleasedAt       *time.Time
	ExpiredAt        *time.Time
	CreatedAt        time.Time
}

// TokenCounts holds finalized token counts from an inference.
type TokenCounts struct {
	InputTokens    int
	OutputTokens   int
	CachedTokens   int
	ReasoningTokens int
}

// ComputeCost calculates the total cost from a pricing snapshot and token counts.
// All arithmetic uses decimal.Decimal — never float64.
func ComputeCost(snap PricingSnapshot, tokens TokenCounts) (inputCost, cachedCost, outputCost, totalCost decimal.Decimal) {
	billableInput := decimal.NewFromInt(int64(tokens.InputTokens - tokens.CachedTokens))
	if billableInput.IsNegative() {
		billableInput = decimal.Zero
	}
	cachedInput := decimal.NewFromInt(int64(tokens.CachedTokens))
	output := decimal.NewFromInt(int64(tokens.OutputTokens))

	inputCost = billableInput.Mul(snap.InputRate)
	cachedCost = cachedInput.Mul(snap.CachedRate)
	outputCost = output.Mul(snap.OutputRate)
	totalCost = inputCost.Add(cachedCost).Add(outputCost)
	return
}

// MaxEstimatedCost computes the worst-case cost for reservation purposes.
// Uses estimated input tokens and max_output_tokens from the request.
func MaxEstimatedCost(snap PricingSnapshot, estimatedInput, maxOutputTokens int) decimal.Decimal {
	input := decimal.NewFromInt(int64(estimatedInput))
	output := decimal.NewFromInt(int64(maxOutputTokens))
	return input.Mul(snap.InputRate).Add(output.Mul(snap.OutputRate))
}
