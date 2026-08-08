# Skill: Finding Validation Gate

## Purpose
Rigorously validate any discovered bug, security flaw, performance bottleneck, architectural issue, concurrency risk, or audit finding BEFORE attempting any fix.

## Execution Steps

1. **Re-Inspect Source Code**:
   - Re-open exact line numbers and surrounding context in authoritative source files.
   - Do not rely on initial quick readings or assumptions.

2. **Verify Claim Against Actual Implementation**:
   - Trace exact execution path to test if the claimed issue actually occurs under runtime conditions.

3. **Check Framework & Library Normalization**:
   - Verify whether Django, Django REST Framework, Axios, React, or standard libraries automatically normalize, sanitize, or shield against the issue (e.g. Django `HttpHeaders` case-insensitivity, default escaping, built-in ORM parameterization).

4. **Review Existing Tests & Documentation**:
   - Check existing test suites (`tests*.py`) and configuration files (`settings.py`, `.env`) for existing safeguards or overrides.

5. **Classify Finding**:
   - **CONFIRMED**: Issue is verified, technically accurate, reproducible, and has direct impact.
   - **PARTIALLY CONFIRMED**: Behavior exists as described, but scope, severity, or impact was inaccurate.
   - **FALSE POSITIVE**: Claimed behavior does not occur due to framework behavior, language mechanics, or existing code handling.
   - **NEEDS MORE EVIDENCE**: Cannot be confirmed without additional empirical evidence, environment logs, or staging tests.

6. **Enforce Gate Rule**:
   - Only **CONFIRMED** or **PARTIALLY CONFIRMED** findings may proceed to the implementation/fix stage.
   - **FALSE POSITIVE** findings MUST NOT be modified or fixed.
   - **NEEDS MORE EVIDENCE** findings MUST remain unresolved until sufficient evidence is obtained.
