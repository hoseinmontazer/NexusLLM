-- NexusLLM Migration 054 — Billing Core Tables
--
-- Creates the complete billing and admission accounting schema.
-- All tables are append-only or state-machine guarded (triggers added in 055).
--
-- Table creation order respects FK dependencies:
--   billing_accounts
--   → wallets, credit_accounts
--   → wallet_ledger, credit_ledger
--   → billing_authorizations
--   → model_pricing
--   → inference_usage
--   → inference_completions
--   → quota_ledger
--
-- This migration is additive. It does NOT modify any existing tables.

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. billing_accounts
--    Primary billing entity. One per org initially.
--    Supports both prepaid and postpaid account types.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS billing_accounts (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          UUID        NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
    name            TEXT        NOT NULL,
    account_type    TEXT        NOT NULL DEFAULT 'prepaid'
        CHECK (account_type IN ('prepaid','postpaid')),
    status          TEXT        NOT NULL DEFAULT 'active'
        CHECK (status IN ('active','suspended','closed')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_billing_accounts_org
    ON billing_accounts(org_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. wallets  (prepaid accounts only)
--    Materialized balance and reserved columns for O(1) admission checks.
--    Kept consistent with wallet_ledger via transactional updates.
--    Invariants: balance >= 0, reserved >= 0, reserved <= balance.
--    (CHECK constraints added in migration 055.)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS wallets (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    billing_account_id  UUID        NOT NULL UNIQUE REFERENCES billing_accounts(id) ON DELETE RESTRICT,
    currency            CHAR(3)     NOT NULL DEFAULT 'USD',
    balance             NUMERIC(16,8) NOT NULL DEFAULT 0,
    reserved            NUMERIC(16,8) NOT NULL DEFAULT 0,
    version             BIGINT      NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. wallet_ledger  (prepaid, append-only)
--    Financial source of truth for prepaid accounts.
--    Immutable after insert (enforced by trigger in 055).
--    entry_type semantics:
--      topup           — funds added to wallet
--      debit           — actual inference cost settled
--      release         — unused reservation returned to available balance
--      adjustment      — reconciliation correction (auditable)
--      unplanned_debit — post-expiry recovery charge (best_effort policy)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS wallet_ledger (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    wallet_id            UUID        NOT NULL REFERENCES wallets(id) ON DELETE RESTRICT,
    entry_type           TEXT        NOT NULL
        CHECK (entry_type IN ('topup','debit','release','adjustment','unplanned_debit')),
    amount               NUMERIC(16,8) NOT NULL CHECK (amount >= 0),
    -- Explicit typed references (never overloaded)
    inference_usage_id   UUID,       -- set for debit and unplanned_debit
    authorization_id     UUID,       -- set for release
    topup_transaction_id TEXT,       -- set for topup (external payment reference)
    description          TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_wallet_ledger_wallet
    ON wallet_ledger(wallet_id, created_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. credit_accounts  (postpaid accounts only)
--    total_exposure = active_reservations + committed_debt_this_cycle.
--    Invariant: total_exposure <= credit_limit enforced in RESERVE transaction only.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS credit_accounts (
    id                      UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    billing_account_id      UUID        NOT NULL UNIQUE REFERENCES billing_accounts(id) ON DELETE RESTRICT,
    currency                CHAR(3)     NOT NULL DEFAULT 'USD',
    credit_limit            NUMERIC(16,8) NOT NULL DEFAULT 0 CHECK (credit_limit >= 0),
    -- total_exposure = SUM(active_reservation.estimated_cost)
    --                + SUM(committed_this_cycle.actual_cost)
    -- Enforced transactionally, not as a static CHECK constraint
    -- (removing static CHECK allows admin credit limit reductions while
    --  existing authorizations are still active).
    total_exposure          NUMERIC(16,8) NOT NULL DEFAULT 0 CHECK (total_exposure >= 0),
    soft_limit_pct          INTEGER     NOT NULL DEFAULT 90
        CHECK (soft_limit_pct BETWEEN 0 AND 100),
    hard_limit_action       TEXT        NOT NULL DEFAULT 'reject'
        CHECK (hard_limit_action IN ('reject','queue')),
    cycle_start_day         INTEGER     NOT NULL DEFAULT 1
        CHECK (cycle_start_day BETWEEN 1 AND 28),
    current_cycle_start     DATE        NOT NULL DEFAULT CURRENT_DATE,
    current_cycle_settled   NUMERIC(16,8) NOT NULL DEFAULT 0,
    version                 BIGINT      NOT NULL DEFAULT 0,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. credit_ledger  (postpaid, append-only)
--    Audit trail for postpaid credit transactions.
--    entry_type semantics:
--      authorized  — credit reserved for in-flight request
--      settled     — actual cost committed after inference
--      released    — unused reservation freed
--      cycle_reset — monthly cycle boundary record
--      adjustment  — manual reconciliation correction
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS credit_ledger (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    credit_account_id   UUID        NOT NULL REFERENCES credit_accounts(id) ON DELETE RESTRICT,
    entry_type          TEXT        NOT NULL
        CHECK (entry_type IN ('authorized','settled','released','cycle_reset','adjustment')),
    amount              NUMERIC(16,8) NOT NULL CHECK (amount >= 0),
    inference_usage_id  UUID,       -- set for authorized, settled
    authorization_id    UUID,       -- set for released
    description         TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_credit_ledger_account
    ON credit_ledger(credit_account_id, created_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. model_pricing  (versioned, append-only rows)
--    Never UPDATE existing rows. Set effective_to on old row, INSERT new row.
--    Unique partial index ensures one active row per model at any time.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS model_pricing (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id            UUID        NOT NULL REFERENCES models(id) ON DELETE RESTRICT,
    -- Per-token rates (USD). Use NUMERIC for exact arithmetic.
    input_per_token     NUMERIC(16,10) NOT NULL DEFAULT 0 CHECK (input_per_token >= 0),
    output_per_token    NUMERIC(16,10) NOT NULL DEFAULT 0 CHECK (output_per_token >= 0),
    cached_per_token    NUMERIC(16,10) NOT NULL DEFAULT 0 CHECK (cached_per_token >= 0),
    currency            CHAR(3)     NOT NULL DEFAULT 'USD',
    effective_from      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    effective_to        TIMESTAMPTZ,            -- NULL = currently active
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by          TEXT        NOT NULL DEFAULT 'system'
);

-- Only one active pricing row per model
CREATE UNIQUE INDEX IF NOT EXISTS idx_model_pricing_active
    ON model_pricing(model_id)
    WHERE effective_to IS NULL;

CREATE INDEX IF NOT EXISTS idx_model_pricing_model
    ON model_pricing(model_id, effective_from DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 7. inference_usage
--    Canonical record for every inference attempt.
--    Created at step 8 of the request lifecycle (after admission, before billing).
--    Billing identity fields (billing_account_id, rates) are immutable after
--    creation (enforced by trigger in migration 055).
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS inference_usage (
    id                      UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- ── Idempotency ──────────────────────────────────────────────────────────
    request_id              TEXT        NOT NULL,
    UNIQUE(request_id),
    -- SHA-256 of (model + sorted_messages_json + temperature + max_tokens + stream)
    -- Application checks hash match; DB has UNIQUE(request_id) only.
    request_hash            TEXT        NOT NULL,

    -- ── Attribution (all resolved once at admission, immutable) ──────────────
    org_id                  UUID        NOT NULL,
    team_id                 UUID        NOT NULL,
    project_id              UUID,                   -- NULL for legacy team-only tokens
    api_key_id              UUID,
    model_id                UUID        REFERENCES models(id) ON DELETE SET NULL,
    model_name              TEXT        NOT NULL,
    billing_account_id      UUID        REFERENCES billing_accounts(id) ON DELETE RESTRICT,

    -- ── Pricing snapshot (resolved at admission, IMMUTABLE after creation) ───
    -- Settlement MUST use these inline values, never re-query model_pricing.
    pricing_id              UUID        REFERENCES model_pricing(id) ON DELETE RESTRICT,
    input_rate              NUMERIC(16,10) NOT NULL DEFAULT 0,
    output_rate             NUMERIC(16,10) NOT NULL DEFAULT 0,
    cached_rate             NUMERIC(16,10) NOT NULL DEFAULT 0,
    pricing_currency        CHAR(3)     NOT NULL DEFAULT 'USD',
    pricing_resolved_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- ── Execution state (mutable until terminal) ─────────────────────────────
    execution_status        TEXT        NOT NULL DEFAULT 'pending'
        CHECK (execution_status IN (
            'pending',      -- row created, not yet dispatched to backend
            'running',      -- dispatched, awaiting response
            'completed',    -- clean finish_reason=stop or length
            'partial',      -- stream ended early, some tokens received
            'failed',       -- backend error before any tokens generated
            'unknown'       -- cannot determine outcome; needs reconciliation
        )),

    -- ── Billing state (mutable via state machine) ────────────────────────────
    billing_status          TEXT        NOT NULL DEFAULT 'pending'
        CHECK (billing_status IN (
            'not_applicable',   -- request rejected before billing (auth/acl/rate-limit)
            'pending',          -- usage row created, billing not yet reserved
            'reserved',         -- authorization active, funds/credit held
            'committed',        -- actual cost settled
            'released',         -- no charge (failed path)
            'disputed'          -- conflicting evidence, manual review required
        )),

    -- ── Token counts (finalized at stream end) ────────────────────────────────
    estimated_input_tokens  INTEGER     NOT NULL DEFAULT 0 CHECK (estimated_input_tokens >= 0),
    input_tokens            INTEGER     NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens           INTEGER     NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cached_tokens           INTEGER     NOT NULL DEFAULT 0 CHECK (cached_tokens >= 0),
    reasoning_tokens        INTEGER     NOT NULL DEFAULT 0 CHECK (reasoning_tokens >= 0),
    total_tokens            INTEGER     GENERATED ALWAYS AS (input_tokens + output_tokens) STORED,

    -- ── Cost (computed at finalization using inline rates) ───────────────────
    input_cost              NUMERIC(16,8) NOT NULL DEFAULT 0,
    output_cost             NUMERIC(16,8) NOT NULL DEFAULT 0,
    cached_input_cost       NUMERIC(16,8) NOT NULL DEFAULT 0,
    total_cost              NUMERIC(16,8) NOT NULL DEFAULT 0,
    currency                CHAR(3)     NOT NULL DEFAULT 'USD',

    -- ── Request metadata ─────────────────────────────────────────────────────
    max_tokens_requested    INTEGER,                -- from request body (post-injection)
    finish_reason           TEXT,
    is_streaming            BOOLEAN     NOT NULL DEFAULT FALSE,
    is_billable             BOOLEAN     NOT NULL DEFAULT FALSE,

    -- ── Provider / runtime ───────────────────────────────────────────────────
    provider_name           TEXT,
    -- Generated BEFORE upstream dispatch so crash recovery can use it.
    provider_request_id     TEXT,
    runtime_request_id      TEXT,
    endpoint_id             UUID,

    -- ── Admission (for rollback ownership verification) ──────────────────────
    admission_token         TEXT,

    -- ── Timing ───────────────────────────────────────────────────────────────
    latency_ms              INTEGER,
    ttft_ms                 INTEGER,
    started_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at            TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Query indexes
CREATE INDEX IF NOT EXISTS idx_iu_org         ON inference_usage(org_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_iu_team        ON inference_usage(team_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_iu_project     ON inference_usage(project_id, started_at DESC)
    WHERE project_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_iu_billing     ON inference_usage(billing_account_id, started_at DESC)
    WHERE billing_account_id IS NOT NULL;
-- Sweep index: finds stale running/pending rows efficiently
CREATE INDEX IF NOT EXISTS idx_iu_sweep       ON inference_usage(started_at)
    WHERE execution_status IN ('pending','running');
-- Dispute review
CREATE INDEX IF NOT EXISTS idx_iu_disputed    ON inference_usage(created_at)
    WHERE billing_status = 'disputed';

-- ─────────────────────────────────────────────────────────────────────────────
-- 8. billing_authorizations
--    One per inference_usage. Unified for both prepaid and postpaid.
--    State machine: active → committed | released | expired.
--    (Transition trigger added in migration 055.)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS billing_authorizations (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    inference_usage_id  UUID        NOT NULL UNIQUE REFERENCES inference_usage(id) ON DELETE RESTRICT,
    billing_account_id  UUID        NOT NULL REFERENCES billing_accounts(id) ON DELETE RESTRICT,
    account_type        TEXT        NOT NULL CHECK (account_type IN ('prepaid','postpaid')),
    estimated_cost      NUMERIC(16,8) NOT NULL CHECK (estimated_cost >= 0),
    -- actual_cost is set at commitment. Capped at estimated_cost.
    actual_cost         NUMERIC(16,8),
    currency            CHAR(3)     NOT NULL DEFAULT 'USD',
    status              TEXT        NOT NULL DEFAULT 'active'
        CHECK (status IN ('active','committed','released','expired')),
    -- TTL for crash recovery — expiry_sweep uses this
    expires_at          TIMESTAMPTZ NOT NULL,
    committed_at        TIMESTAMPTZ,
    released_at         TIMESTAMPTZ,
    expired_at          TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ba_account_status
    ON billing_authorizations(billing_account_id, status);
CREATE INDEX IF NOT EXISTS idx_ba_expires
    ON billing_authorizations(expires_at)
    WHERE status = 'active';

-- ─────────────────────────────────────────────────────────────────────────────
-- 9. inference_completions
--    Execution evidence from multiple sources.
--    Append-only (no DELETE trigger added in migration 055).
--    Multiple rows per inference_usage — one per source.
--    Reconciliation hierarchy: provider_reconcile > worker > gateway > manual.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS inference_completions (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    inference_usage_id  UUID        NOT NULL REFERENCES inference_usage(id) ON DELETE RESTRICT,
    request_id          TEXT        NOT NULL,
    source              TEXT        NOT NULL
        CHECK (source IN ('gateway','worker','provider_reconcile','manual')),
    -- One completion record per source per inference
    UNIQUE(inference_usage_id, source),
    input_tokens        INTEGER     NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens       INTEGER     NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cached_tokens       INTEGER     NOT NULL DEFAULT 0 CHECK (cached_tokens >= 0),
    finish_reason       TEXT,
    provider_request_id TEXT,
    runtime_request_id  TEXT,
    -- Raw provider response payload for audit. Never used in billing computation.
    raw_response        JSONB,
    recorded_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Which gateway instance or worker node wrote this record
    recorded_by         TEXT        NOT NULL DEFAULT 'unknown'
);

CREATE INDEX IF NOT EXISTS idx_ic_usage_id    ON inference_completions(inference_usage_id);
CREATE INDEX IF NOT EXISTS idx_ic_request_id  ON inference_completions(request_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- 10. quota_ledger
--     PostgreSQL-authoritative quota tracking.
--     Used as Redis rebuild source and fallback when Redis is unavailable.
--     Updated post-response by quota_ledger_sync job.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS quota_ledger (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    scope_type      TEXT        NOT NULL
        CHECK (scope_type IN ('project','team','org','model')),
    scope_id        UUID        NOT NULL,
    period_type     TEXT        NOT NULL CHECK (period_type IN ('daily','monthly')),
    -- 'daily': 'YYYY-MM-DD', 'monthly': 'YYYY-MM'
    period_key      TEXT        NOT NULL,
    tokens_used     BIGINT      NOT NULL DEFAULT 0 CHECK (tokens_used >= 0),
    tokens_limit    BIGINT      NOT NULL DEFAULT 0 CHECK (tokens_limit >= 0), -- 0 = unlimited
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(scope_type, scope_id, period_type, period_key)
);

CREATE INDEX IF NOT EXISTS idx_ql_lookup
    ON quota_ledger(scope_type, scope_id, period_type, period_key);

-- ─────────────────────────────────────────────────────────────────────────────
-- 11. model_policies
--     Per-model behavioural limits. Created here with IF NOT EXISTS so this
--     migration is safe whether or not an earlier migration already created
--     the table. The ADD COLUMN below is then a guaranteed no-op on fresh
--     installs and a safe add-if-missing on existing databases.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS model_policies (
    id                          UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id                    UUID    NOT NULL UNIQUE REFERENCES models(id) ON DELETE CASCADE,
    default_max_output_tokens   INTEGER NOT NULL DEFAULT 2048
        CHECK (default_max_output_tokens > 0),
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_model_policies_model_id ON model_policies(model_id);

-- Ensure the column exists on databases where the table predates this migration.
ALTER TABLE model_policies
    ADD COLUMN IF NOT EXISTS default_max_output_tokens INTEGER NOT NULL DEFAULT 2048
        CHECK (default_max_output_tokens > 0);

-- ─────────────────────────────────────────────────────────────────────────────
-- 12. billing_invoices  (postpaid, one per billing cycle)
--     Created during cycle reset. Links to credit_ledger cycle_reset entry.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS billing_invoices (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    credit_account_id   UUID        NOT NULL REFERENCES credit_accounts(id) ON DELETE RESTRICT,
    cycle_start         DATE        NOT NULL,
    cycle_end           DATE        NOT NULL,
    settled_amount      NUMERIC(16,8) NOT NULL DEFAULT 0,
    currency            CHAR(3)     NOT NULL DEFAULT 'USD',
    status              TEXT        NOT NULL DEFAULT 'open'
        CHECK (status IN ('open','paid','overdue','void')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_invoices_account
    ON billing_invoices(credit_account_id, cycle_start DESC);

COMMIT;
