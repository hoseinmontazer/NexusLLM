# Skill: Deep Audit

## Purpose
Perform a multi-dimensional, comprehensive code audit across architecture, correctness, security, performance, concurrency, reliability, and observability.

## Audit Dimensions

1. **Architecture**:
   - Check coupling, cohesion, boundary leaks between Django apps or frontend modules.
   - Verify dependency direction and duplication.

2. **Correctness**:
   - Check edge cases (timezone crossovers, overnight shifts, empty inputs, null handling).
   - Validate state transitions and data model invariants.

3. **Security**:
   - Inspect authentication, authorization rules, permissions checks, input validation, SQL injection risks, and sensitive data leakage.

4. **Performance**:
   - Identify N+1 queries (`select_related`/`prefetch_related` missing), heavy synchronous loops, unindexed DB filters, and unnecessary render cycles.

5. **Concurrency & Reliability**:
   - Audit race conditions, multi-device check-ins, database locks, idempotency of API endpoints, timeout handling, and failure recovery.

6. **Observability & Testing**:
   - Check audit log records (`AuditLogManager`), exception logging, and test coverage gaps.

## Audit Findings Format
For every issue found, record:
- **Severity**: Critical | High | Medium | Low | Info
- **Location**: `path/to/file.py:L123`
- **Evidence**: Snippet or trace description
- **Root Cause**: Why it occurs
- **Impact**: Potential consequences
- **Recommendation**: Concrete fix strategy
