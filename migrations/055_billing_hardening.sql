-- NexusLLM Migration 055 — Billing Hardening
--
-- Adds database-level enforcement for every billing invariant.
-- All triggers, constraints, and indexes that protect financial correctness.
-- This migration depends on 054_billing_core_tables.sql.
--
-- Invariants enforced here:
--   1. Billing identity immutability on inference_usage
--   2. Execution status forward-only transition on inference_usage
--   3. Billing authorization terminal-state guard
--   4. wallet_ledger and credit_ledger append-only enforcement
--   5. inference_completions no-delete enforcement
--   6. Wallet balance/reserved non-negative CHECK constraints
--   7. One debit per inference (partial unique index)
--   8. One release per authorization (partial unique index)
--   9. One settled entry per inference on credit_ledger
--  10. is_billable requires billing_account_id
--  11. completed_at required when execution reaches terminal state
--  12. Wallet updated_at auto-trigger
--  13. billing_authorizations FK to wallet_ledger and credit_ledger

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. Billing identity immutability on inference_usage
--    billing_account_id, input_rate, output_rate, cached_rate, pricing_currency
--    are immutable after row creation.
--    Prevents re-routing billing to a different account after the fact.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION fn_prevent_billing_identity_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.billing_account_id IS DISTINCT FROM OLD.billing_account_id THEN
        RAISE EXCEPTION
            'inference_usage.billing_account_id is immutable after creation. '
            'row id=%, attempted change from % to %',
            OLD.id, OLD.billing_account_id, NEW.billing_account_id;
    END IF;
    IF NEW.input_rate IS DISTINCT FROM OLD.input_rate OR
       NEW.output_rate IS DISTINCT FROM OLD.output_rate OR
       NEW.cached_rate IS DISTINCT FROM OLD.cached_rate OR
       NEW.pricing_currency IS DISTINCT FROM OLD.pricing_currency THEN
        RAISE EXCEPTION
            'inference_usage pricing rates are immutable after creation. row id=%', OLD.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_billing_identity ON inference_usage;
CREATE TRIGGER trg_billing_identity
    BEFORE UPDATE ON inference_usage
    FOR EACH ROW EXECUTE FUNCTION fn_prevent_billing_identity_mutation();

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. Execution status forward-only transition on inference_usage
--    Terminal states: completed, partial, failed, unknown
--    Once in a terminal state, execution_status cannot change.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION fn_guard_execution_transition()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.execution_status IN ('completed','partial','failed','unknown')
       AND NEW.execution_status IS DISTINCT FROM OLD.execution_status THEN
        RAISE EXCEPTION
            'inference_usage % has terminal execution_status=%. '
            'Cannot transition to %.',
            OLD.id, OLD.execution_status, NEW.execution_status;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_execution_transition ON inference_usage;
CREATE TRIGGER trg_execution_transition
    BEFORE UPDATE OF execution_status ON inference_usage
    FOR EACH ROW EXECUTE FUNCTION fn_guard_execution_transition();

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. Billing authorization terminal-state guard
--    Legal transitions: active → committed | released | expired
--    Terminal states (committed, released, expired) cannot re-transition.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION fn_guard_auth_transition()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status IN ('committed','released','expired')
       AND NEW.status IS DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION
            'billing_authorization % is in terminal state=%. '
            'Transition to % is illegal.',
            OLD.id, OLD.status, NEW.status;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_auth_transition ON billing_authorizations;
CREATE TRIGGER trg_auth_transition
    BEFORE UPDATE OF status ON billing_authorizations
    FOR EACH ROW EXECUTE FUNCTION fn_guard_auth_transition();

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. Ledger append-only enforcement
--    wallet_ledger and credit_ledger: no UPDATE or DELETE ever.
--    Financial audit trail must be immutable.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION fn_ledger_append_only()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION
        'Ledger tables are append-only. UPDATE and DELETE are forbidden. '
        'table=%, operation=%', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_wallet_ledger_immutable ON wallet_ledger;
CREATE TRIGGER trg_wallet_ledger_immutable
    BEFORE UPDATE OR DELETE ON wallet_ledger
    FOR EACH ROW EXECUTE FUNCTION fn_ledger_append_only();

DROP TRIGGER IF EXISTS trg_credit_ledger_immutable ON credit_ledger;
CREATE TRIGGER trg_credit_ledger_immutable
    BEFORE UPDATE OR DELETE ON credit_ledger
    FOR EACH ROW EXECUTE FUNCTION fn_ledger_append_only();

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. inference_completions no-delete enforcement
--    Evidence rows are immutable — corrections come as new rows (new source).
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION fn_completions_no_delete()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION
        'inference_completions is append-only. DELETE is forbidden. id=%', OLD.id;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_completions_no_delete ON inference_completions;
CREATE TRIGGER trg_completions_no_delete
    BEFORE DELETE ON inference_completions
    FOR EACH ROW EXECUTE FUNCTION fn_completions_no_delete();

-- ─────────────────────────────────────────────────────────────────────────────
-- 6. Wallet balance/reserved non-negative CHECK constraints
--    These are hard invariants — the application should never reach them,
--    but the DB must catch bugs.
-- ─────────────────────────────────────────────────────────────────────────────
-- Postgres has no ADD CONSTRAINT IF NOT EXISTS for CHECK constraints, so this
-- migration re-runs cleanly (e.g. after a partial apply) by dropping first.
ALTER TABLE wallets DROP CONSTRAINT IF EXISTS chk_wallet_balance_non_negative;
ALTER TABLE wallets DROP CONSTRAINT IF EXISTS chk_wallet_reserved_non_negative;
ALTER TABLE wallets DROP CONSTRAINT IF EXISTS chk_wallet_reserved_le_balance;

ALTER TABLE wallets
    ADD CONSTRAINT chk_wallet_balance_non_negative
        CHECK (balance >= 0),
    ADD CONSTRAINT chk_wallet_reserved_non_negative
        CHECK (reserved >= 0),
    ADD CONSTRAINT chk_wallet_reserved_le_balance
        CHECK (reserved <= balance);

-- ─────────────────────────────────────────────────────────────────────────────
-- 7. One debit per inference (wallet_ledger)
--    Prevents double-debit from duplicate billing worker execution.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE UNIQUE INDEX IF NOT EXISTS idx_wl_one_debit_per_usage
    ON wallet_ledger(inference_usage_id)
    WHERE entry_type = 'debit' AND inference_usage_id IS NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 8. One release per authorization (wallet_ledger)
--    Prevents releasing the same reservation twice.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE UNIQUE INDEX IF NOT EXISTS idx_wl_one_release_per_auth
    ON wallet_ledger(authorization_id)
    WHERE entry_type = 'release' AND authorization_id IS NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 9. One settled entry per inference (credit_ledger)
--    Prevents double-settlement on postpaid accounts.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE UNIQUE INDEX IF NOT EXISTS idx_cl_one_settled_per_usage
    ON credit_ledger(inference_usage_id)
    WHERE entry_type = 'settled' AND inference_usage_id IS NOT NULL;

-- One release per authorization (credit_ledger)
CREATE UNIQUE INDEX IF NOT EXISTS idx_cl_one_release_per_auth
    ON credit_ledger(authorization_id)
    WHERE entry_type = 'released' AND authorization_id IS NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 10. is_billable requires billing_account_id
--     A billable inference must always have a billing account.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE inference_usage DROP CONSTRAINT IF EXISTS chk_billable_needs_account;
ALTER TABLE inference_usage
    ADD CONSTRAINT chk_billable_needs_account
        CHECK (is_billable = FALSE OR billing_account_id IS NOT NULL);

-- ─────────────────────────────────────────────────────────────────────────────
-- 11. Terminal execution states require completed_at
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE inference_usage DROP CONSTRAINT IF EXISTS chk_terminal_has_completed_at;
ALTER TABLE inference_usage
    ADD CONSTRAINT chk_terminal_has_completed_at
        CHECK (
            execution_status NOT IN ('completed','partial','failed','unknown')
            OR completed_at IS NOT NULL
        );

-- ─────────────────────────────────────────────────────────────────────────────
-- 12. Auto-update updated_at on wallets and credit_accounts
-- ─────────────────────────────────────────────────────────────────────────────
DROP TRIGGER IF EXISTS set_wallets_updated_at ON wallets;
CREATE TRIGGER set_wallets_updated_at
    BEFORE UPDATE ON wallets
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

DROP TRIGGER IF EXISTS set_credit_accounts_updated_at ON credit_accounts;
CREATE TRIGGER set_credit_accounts_updated_at
    BEFORE UPDATE ON credit_accounts
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

DROP TRIGGER IF EXISTS set_billing_accounts_updated_at ON billing_accounts;
CREATE TRIGGER set_billing_accounts_updated_at
    BEFORE UPDATE ON billing_accounts
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- 13. wallet_ledger FK to billing_authorizations for release entries
--     Added here (not 054) because billing_authorizations is in 054.
-- ─────────────────────────────────────────────────────────────────────────────
ALTER TABLE wallet_ledger DROP CONSTRAINT IF EXISTS fk_wallet_ledger_auth;
ALTER TABLE wallet_ledger
    ADD CONSTRAINT fk_wallet_ledger_auth
        FOREIGN KEY (authorization_id)
        REFERENCES billing_authorizations(id) ON DELETE RESTRICT
        DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE wallet_ledger DROP CONSTRAINT IF EXISTS fk_wallet_ledger_usage;
ALTER TABLE wallet_ledger
    ADD CONSTRAINT fk_wallet_ledger_usage
        FOREIGN KEY (inference_usage_id)
        REFERENCES inference_usage(id) ON DELETE RESTRICT
        DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE credit_ledger DROP CONSTRAINT IF EXISTS fk_credit_ledger_auth;
ALTER TABLE credit_ledger
    ADD CONSTRAINT fk_credit_ledger_auth
        FOREIGN KEY (authorization_id)
        REFERENCES billing_authorizations(id) ON DELETE RESTRICT
        DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE credit_ledger DROP CONSTRAINT IF EXISTS fk_credit_ledger_usage;
ALTER TABLE credit_ledger
    ADD CONSTRAINT fk_credit_ledger_usage
        FOREIGN KEY (inference_usage_id)
        REFERENCES inference_usage(id) ON DELETE RESTRICT
        DEFERRABLE INITIALLY DEFERRED;

-- ─────────────────────────────────────────────────────────────────────────────
-- 14. billing_authorizations FK to wallet_ledger for debit traceability
--     (soft reference via inference_usage_id — already covered by unique index)
-- ─────────────────────────────────────────────────────────────────────────────

-- ─────────────────────────────────────────────────────────────────────────────
-- 15. Default billing account creation trigger
--     When a new org is created, automatically create a default prepaid
--     billing account and wallet. Allows the system to function without
--     manual billing setup for single-tenant deployments.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION fn_create_default_billing_account()
RETURNS TRIGGER AS $$
DECLARE
    v_account_id UUID;
    v_wallet_id  UUID;
BEGIN
    -- Create a default prepaid billing account for the new org
    INSERT INTO billing_accounts (org_id, name, account_type, status)
    VALUES (NEW.id, NEW.name || ' Default Account', 'prepaid', 'active')
    RETURNING id INTO v_account_id;

    -- Create a zero-balance wallet for the account
    INSERT INTO wallets (billing_account_id, currency, balance, reserved)
    VALUES (v_account_id, 'USD', 0, 0);

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_org_billing_setup ON organizations;
CREATE TRIGGER trg_org_billing_setup
    AFTER INSERT ON organizations
    FOR EACH ROW EXECUTE FUNCTION fn_create_default_billing_account();

COMMIT;
