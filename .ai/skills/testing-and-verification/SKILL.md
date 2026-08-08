# Skill: Testing & Verification

## Purpose
Ensure all modified or newly introduced code is verified using empirical test execution and static validation tools.

## Execution Steps

1. **Identify Required Checks**:
   - Determine applicable checks based on modified subprojects:
     - Django backend: Unit/integration tests (`python manage.py test <app>`), Django checks (`python manage.py check`), migration checks (`python manage.py migrate --check`).
     - Web frontend: ESLint (`pnpm run lint`), build check (`pnpm run build`).
     - Mobile: React Native build / lint verification.

2. **Execute Commands**:
   - Run verification commands explicitly using shell execution tools.
   - Never assume success without seeing actual process exit codes and output.

3. **Interpret & Resolve Failures**:
   - If tests fail, analyze traceback logs immediately. Fix the root cause and re-execute verification until clean execution is confirmed.

4. **Document Verification**:
   - Report exact commands executed, test counts, duration, and output results.
