# Workflow: Implement Feature

## Purpose
Plan, implement, test, and verify new functional requirements end-to-end across backend, web, and mobile.

## Workflow Steps

1. **Requirements & Investigation**:
   - Parse requirements and execute `deep-investigation` to locate existing touchpoints.
2. **Draft Plan**:
   - Create an implementation plan detailing model modifications, API viewsets/serializers, frontend service calls, and UI component changes.
3. **Execute Implementation**:
   - Apply minimal, clean edits using `feature-implementation`.
4. **Execute Testing & Verification**:
   - Run backend Django unit tests: `python manage.py test <app_name>`.
   - Run system/migration checks: `python manage.py check`.
   - Run web lint/build: `pnpm run lint` / `pnpm run build`.
5. **Self-Review & Fix**:
   - Review code diffs for unintended side effects or unhandled edge cases.
6. **Final Verification & Report**:
   - Present final report summarizing changes, verification results, and remaining risks.
