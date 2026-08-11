# NexusLLM Billing & Admission — Implementation Plan

Status: APPROVED FOR IMPLEMENTATION
Derived from: Fifth Pass Adversarial Review + Final Design Decisions

---

## Infrastructure Decisions (frozen)

| Decision | Value |
|---|---|
| Admission Redis | Dedicated standalone instance `redis-admission` (not cluster) |
| Admission Redis memory policy | `maxmemory-policy noeviction`, `maxmemory 128mb` |
| Billing after expiry | `best_effort` by default (configurable to `write_off`) |
| Redis failure on model RPM/TPM | Fail closed — return 503 |
| Redis failure on project RPM/TPM | Fail closed with PostgreSQL fallback |
| Redis failure on ACL | Fail closed with PostgreSQL fallback |
| Prepaid / postpaid billing | PostgreSQL only — zero Redis dependency |
| max_tokens absent | Inject `model_policies.default_max_output_tokens` (default 2048) |
| Cost arithmetic | `shopspring/decimal` — never float64 |
| provider_request_id timing | Generated and stored BEFORE upstream dispatch |

---

## Implementation Order

1. `migrations/054_billing_core_tables.sql` — all new tables
2. `migrations/055_billing_hardening.sql` — triggers, constraints, column additions
3. `internal/admission/` — Redis client + gate + Lua scripts
4. `internal/billing/` — accounts, pricing, reservation, settlement
5. `internal/billing/sweep/` — background jobs
6. `internal/config/config.go` — add AdmissionRedisConfig
7. `cmd/gateway/main.go` — wire admission gate + sweep jobs
8. `internal/proxy/handler.go` — integrate admission into request pipeline
9. `go build ./...` — verify clean compilation

---

## New Tables (Migration 054)

### billing_accounts
Primary billing entity. One per org (initially). Supports prepaid and postpaid.

### wallets
One per prepaid billing_account. Materialized balance + reserved columns.

### wallet_ledger
Append-only financial record for prepaid accounts. entry_type: topup | debit | release | adjustment | unplanned_debit.

### credit_accounts
One per postpaid billing_account. total_exposure tracks active reservations + committed debt this cycle.

### credit_ledger
Append-only record for postpaid accounts. entry_type: authorized | settled | released | cycle_reset | adjustment.

### billing_authorizations
One per inference. Unified for prepaid and postpaid. State machine: active → committed | released | expired.

### model_pricing
Versioned pricing. Append-only (effective_to set on old row, new row inserted). Unique partial index: one active row per model.

### inference_usage
Canonical record for every inference attempt. Created at step 8 of lifecycle. Immutable billing fields after creation.
Key columns: request_id (UNIQUE), request_hash, billing_account_id, pricing snapshot (inline rates), execution_status, billing_status, admission_token.

### inference_completions
Execution evidence. Append-only. One row per (inference_usage_id, source). Sources: gateway | worker | provider_reconcile | manual.

### quota_ledger
PostgreSQL-authoritative quota tracking. Used as Redis rebuild source and fallback when Redis is unavailable.

---

## Migration 055 Hardening

1. Billing identity immutability trigger on inference_usage (billing_account_id, input_rate, output_rate, cached_rate)
2. Execution status forward-only trigger on inference_usage
3. Billing authorization state machine trigger (terminal states cannot re-transition)
4. wallet_ledger append-only trigger (no UPDATE/DELETE)
5. credit_ledger append-only trigger
6. inference_completions no-delete trigger
7. Add default_max_output_tokens to model_policies (INTEGER NOT NULL DEFAULT 2048)
8. Add admission_token column to inference_usage
9. Rename credit_accounts.current_exposure → total_exposure (if column exists from prior migration)
10. Remove static CHECK (current_exposure <= credit_limit) constraint
11. Add partial unique indexes: one debit per inference, one release per auth, one settled per inference
12. Add CHECK constraints: is_billable requires billing_account_id, tokens non-negative, completed_at required for terminal states

---

## Admission Redis Key Model

All keys on `redis-admission` standalone instance:

```
nexus:{admission}:model:<name>:rpm          ZSET   member=request_id  TTL=120s
nexus:{admission}:model:<name>:itpm         STRING INCRBY             TTL=70s
nexus:{admission}:model:<name>:otpm         STRING advisory           TTL=70s
nexus:{admission}:project:<id>:rpm          ZSET   member=request_id  TTL=120s
nexus:{admission}:project:<id>:itpm         STRING INCRBY             TTL=70s
nexus:{admission}:project:<id>:inflight     ZSET   member=request_id (score=heartbeat_ms)
nexus:{admission}:project:<id>:daily:<date> STRING max_reservation    EXPIREAT end-of-day UTC
nexus:{admission}:project:<id>:monthly:<ym> STRING max_reservation    EXPIREAT end-of-month UTC
nexus:{admission}:team:<id>:rpm             ZSET   always recorded    TTL=120s
nexus:{admission}:team:<id>:itpm            STRING always recorded    TTL=70s
nexus:{admission}:org:<id>:monthly          STRING post-response only TTL=33d
nexus:{admission}:token:<request_id>        STRING admission_token    TTL=900s
```

---

## Lua Scripts

### admission_gate.lua
Single atomic script. Phase 1: all checks, zero mutations. Phase 2: all mutations.
Returns {admitted, token} or {rejected, dimension} or {admitted_idempotent, existing_token}.
Idempotency: ZSCORE check on project_rpm ZSET before Phase 2.
Ownership: stores admission_token in token key.

### admission_rollback.lua
Verifies admission_token ownership before any mutation.
Returns {noop, token_mismatch_or_expired} if token does not match.
ZREM from all ZSET keys, safe_decr on all STRING counters (clamp at 0), DEL token key.

### tpm_correction.lua
Post-response. Adjusts input_tpm by (actual_input - estimated_input).
Records actual_output in otpm advisory counters.
Releases unused daily/monthly quota reservation.
Increments org monthly with actual_total.
ZREM from inflight ZSET. DEL token key.

---

## Billing Transaction Boundaries

All billing operations are single PostgreSQL transactions with FOR UPDATE row locking.

### T-RESERVE-PREPAID
SELECT wallets FOR UPDATE → check available → UPDATE wallets.reserved += estimated
→ INSERT billing_authorizations → UPDATE inference_usage.billing_status='reserved'

### T-COMMIT-PREPAID
UPDATE billing_authorizations WHERE status='active' RETURNING estimated_cost
→ IF rows_affected=0: return (already transitioned, no wallet mutation)
→ UPDATE wallets: balance -= actual_cost (capped at estimated_cost), reserved -= estimated_cost
→ INSERT wallet_ledger debit + optional release entries
→ UPDATE inference_usage.billing_status='committed'

### T-RELEASE-PREPAID
UPDATE billing_authorizations WHERE status='active' RETURNING estimated_cost
→ IF rows_affected=0: return (idempotent)
→ UPDATE wallets.reserved -= estimated_cost
→ INSERT wallet_ledger release entry
→ UPDATE inference_usage.billing_status='released'

### T-RESERVE-POSTPAID
SELECT credit_accounts FOR UPDATE → check total_exposure + estimated <= credit_limit
→ UPDATE credit_accounts.total_exposure += estimated
→ INSERT billing_authorizations → UPDATE inference_usage.billing_status='reserved'

### T-COMMIT-POSTPAID
UPDATE billing_authorizations WHERE status='active' RETURNING estimated_cost
→ IF rows_affected=0: return
→ UPDATE credit_accounts.current_cycle_settled += actual_cost
→ IF actual_cost < estimated_cost: total_exposure -= (estimated-actual)
→ INSERT credit_ledger settled entry + optional released entry
→ UPDATE inference_usage.billing_status='committed'

### T-RELEASE-POSTPAID
UPDATE billing_authorizations WHERE status='active' RETURNING estimated_cost
→ IF rows_affected=0: return (idempotent)
→ UPDATE credit_accounts.total_exposure -= estimated_cost
→ INSERT credit_ledger released entry

---

## Background Sweep Jobs

### pending_sweep (every 5 minutes)
Finds inference_usage WHERE execution_status IN (pending,running) AND started_at < NOW()-15min.
Applies evidence hierarchy (provider > worker > gateway).
If evidence found: finalize and commit.
If no evidence: mark unknown/disputed, release authorization.

### expiry_sweep (every 2 minutes)
Finds billing_authorizations WHERE status=active AND expires_at < NOW().
Runs T-EXPIRE for each (conditional UPDATE, rows_affected guard).
For best_effort policy: checks if completion exists + funds available → unplanned_debit.

### wallet_reconciliation (every 1 hour)
Verifies wallet.balance = SUM(ledger debits subtracted from topups).
Verifies wallet.reserved = SUM(active authorization estimated_costs).
Inserts adjustment ledger entry if drift detected.

### redis_rebuild (on reconnect + every 6 hours)
Rebuilds concurrency ZSET from inference_usage WHERE execution_status=running.
Rebuilds daily/monthly quota counters from quota_ledger.
RPM/TPM start fresh (60-70s burst window acceptable).

---

## P0 Tests (mandatory before production)

1. 100 concurrent reservations against wallet.balance=$10 — exactly 20 succeed
2. Duplicate commit — second commit does not debit wallet
3. Expiry racing commit — wallet never goes negative
4. actual_cost > estimated_cost — capped at estimated, no negative balance
5. 100 concurrent postpaid reservations — exactly 1 admitted when exposure near limit
6. Same request_id different hash — 409 returned, no re-execution
7. Same request_id same hash in-flight — 202 returned
8. Admission retry (same request_id) — counters not double-incremented
9. admission_rollback.lua with wrong token — no counter mutation
10. Crash simulation: pending row after 15min → released by sweep
11. Redis restart with daily quota at 80% — rebuild correct, no bypass
12. max_tokens absent — upstream receives default_max_output_tokens
13. provider_request_id stored before dispatch verified

---

## is_billable Policy

| Scenario | is_billable |
|---|---|
| Auth/ACL failure | false |
| Rate limit rejection | false |
| Billing rejection | false |
| Model unavailable, no execution | false |
| Provider error before first token | false |
| Provider/client timeout before first token | false |
| Client disconnect before first token | false |
| Client disconnect after first token | true |
| Provider error after first token | true |
| Partial stream | true |
| Successful response | true |
