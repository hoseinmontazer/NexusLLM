# Workflow: Finding Validation

## Purpose
Execute a mandatory validation pass on all discovered issues before writing code or modifying settings.

## Workflow Steps

1. **Re-Open Exact Source Lines**: Inspect file location and surrounding implementation context.
2. **Verify Runtime Claim**: Confirm whether the claimed defect/risk actually exists.
3. **Inspect Framework Mechanics**: Verify if the underlying framework (Django, DRF, React, etc.) already handles or normalizes the scenario.
4. **Classify Finding**:
   - `CONFIRMED` -> Approved for fix planning.
   - `PARTIALLY CONFIRMED` -> Approved for scope-adjusted fix planning.
   - `FALSE POSITIVE` -> Reject fix. Document why it is not a real issue.
   - `NEEDS MORE EVIDENCE` -> Keep open without code modification until evidence is provided.
5. **Produce Validation Summary**: Log finding status, evidence, correct severity, and recommended action.
