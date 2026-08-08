# Skill: Concurrency Audit

## Purpose
Identify race conditions, data corruption risks, multi-device check-in conflicts, and thread safety issues.

## Scope & Checks

1. **Simultaneous Check-Ins / Check-Outs**:
   - Audit simultaneous attendance check-in requests for the same user (biometric, QR, manual).
   - Verify unique constraints or transaction atomicity (e.g. `atomic()`, unique constraints on `(user, group, date)` where appropriate).

2. **Schedule Resolution Consistency**:
   - Ensure `resolve_schedule()` and `EmployeeGroupMembership` unique constraint (`unique_together = [['user', 'group']]`) prevent ambiguous schedule matches during concurrent reads/writes.

3. **Background Tasks & Signals**:
   - Inspect Celery task execution, signal handlers, and Redis queue mechanisms for idempotency.
