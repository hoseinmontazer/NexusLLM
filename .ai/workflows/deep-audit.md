# Workflow: Deep Audit

## Purpose
Perform a deep, multi-pass audit of codebase modules across 7 dimensions, validate all findings, and report results without modifying code unless explicitly requested.

## Workflow Steps

1. **Scope Selection**: Select target apps/packages (e.g. `work_schedule`, `attendance`, `TimeWaveFront/src/pages`).
2. **Execute Audit Passes**:
   - Invoke `deep-audit`, `security-audit`, `performance-audit`, and `concurrency-audit`.
3. **Finding Validation Gate (MANDATORY)**:
   - For EVERY finding, execute `finding-validation`:
     - Re-inspect exact source lines.
     - Check framework normalization & behavior.
     - Classify: `CONFIRMED`, `PARTIALLY CONFIRMED`, `FALSE POSITIVE`, or `NEEDS MORE EVIDENCE`.
4. **Audit Report**:
   - Present validated findings classified by severity and status.
   - STOP AFTER REPORTing.
5. **Remediation Pass (ONLY IF USER EXPLICITLY REQUESTS FIXES)**:
   - Fix ONLY `CONFIRMED` or `PARTIALLY CONFIRMED` findings.
   - Run tests after each fix.
   - Re-audit modified areas.
