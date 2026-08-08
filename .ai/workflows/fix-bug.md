# Workflow: Fix Bug

## Purpose
Investigate, reproduce, validate, fix, and verify software defects at their root cause.

## Workflow Steps

1. **Failure Analysis & Deep Investigation**:
   - Read full tracebacks and failure logs. Execute `deep-investigation`.
2. **Reproduction**:
   - Write a reproducing test case that isolates the failure.
3. **Finding Validation Gate (MANDATORY)**:
   - Execute `finding-validation`:
     - Re-inspect source code and framework behavior.
     - Classify: `CONFIRMED`, `PARTIALLY CONFIRMED`, `FALSE POSITIVE`, `NEEDS MORE EVIDENCE`.
   - If `FALSE POSITIVE`: Abort fix, document why issue is not present.
4. **Implement Root-Cause Fix**:
   - Apply minimal fix for confirmed findings using `bug-fixing` skill.
5. **Verification**:
   - Re-run reproducing test.
   - Run full regression suite using `testing-and-verification`.
6. **Report**:
   - Provide standard report with finding validation and test verification evidence.
