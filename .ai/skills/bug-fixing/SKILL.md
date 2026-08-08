# Skill: Bug Fixing

## Purpose
Systematically diagnose, reproduce, fix, and verify software bugs at their root cause without suppressing symptoms or degrading tests.

## Execution Steps

1. **Understand Failure & Gather Evidence**:
   - Inspect error tracebacks, console logs, network responses, or test failure logs.
   - Do not hypothesize without reading full error details.

2. **Reproduce the Issue**:
   - Locate or write a failing test case that reproduces the bug under isolation.

3. **Trace Root Cause**:
   - Trace execution upstream from the failure point to find the true root cause (data state inconsistency, unhandled null, race condition, boundary error).

4. **Implement Minimal Root-Cause Fix**:
   - Fix the underlying logic defect cleanly.
   - Prohibited actions:
     - DO NOT remove failing tests.
     - DO NOT weaken assertions.
     - DO NOT swallow exceptions silently in try/except blocks.
     - DO NOT return dummy fallbacks in production paths.

5. **Regression Verification**:
   - Re-run the reproducing test case to verify fix.
   - Run the broader app test suite to ensure no regressions were introduced.
