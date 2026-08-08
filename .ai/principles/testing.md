# Testing & Verification Principles

## 1. Concrete Empirical Verification
- Never claim a task is complete, a bug is fixed, or a feature is working without running verification commands.
- Never claim tests passed unless they were actually executed in the environment.

## 2. Test Pyramid & Scope
- **Backend (`TimeWave-backend`)**:
  - Django test runner: `python manage.py test <app_name>`
  - Run specific test case: `python manage.py test work_schedule.tests_schedule_resolution.ActiveScheduleAPITests`
  - System checks: `python manage.py check`
  - Migration checks: `python manage.py migrate --check`
- **Web Frontend (`TimeWaveFront`)**:
  - Linter: `pnpm run lint` or `npx eslint`
  - Build validation: `pnpm run build`
- **Mobile (`TimeWaveNative`)**:
  - Dependency/syntax validation: `npm run lint` or Expo build checks.

## 3. Test Integrity Rules
- **No Test Degradation**: Never delete failing tests or weaken assertions to make test suites pass.
- **Regression Prevention**: Every bug fix must include a regression test that fails before the fix and passes after.
- **Isolation**: Tests must clean up after themselves and not rely on global database state across runs.
