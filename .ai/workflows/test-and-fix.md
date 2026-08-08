# Workflow: Test and Fix

## Purpose
Execute full repository verification, diagnose any encountered test or build failures, and fix them cleanly.

## Workflow Steps

1. **Execute Tests**:
   - Run backend tests: `python manage.py test`.
   - Run system checks: `python manage.py check`.
   - Run frontend linter/build: `pnpm run lint`, `pnpm run build`.
2. **Diagnose Failures**:
   - Inspect traceback logs for any failing tests.
3. **Fix & Re-test**:
   - Apply minimal root-cause fixes.
   - Re-run failing tests until 100% pass rate is achieved.
4. **Report Verification**:
   - Provide summary of test outputs and execution counts.
