# Checklist: Release Gate

- [ ] All unit and integration test suites pass (`python manage.py test`).
- [ ] Django check passes (`python manage.py check`).
- [ ] Database migrations checked (`python manage.py migrate --check`).
- [ ] Frontend linter passes (`pnpm run lint`).
- [ ] Web production build compiles without error (`pnpm run build`).
- [ ] Mobile Expo build configuration validated.
- [ ] No uncommitted scratch scripts, credentials, or temporary files.
