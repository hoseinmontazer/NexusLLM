# Kiro Steering Adapter: Core Engineering

This steering configuration connects Kiro to the Portable AI Engineering System under `.ai/`.

## Single Source of Truth
Refer to `.ai/README.md` for full engineering methodology details.

## Core Rules & Validation Gate
- Engineering & Task Classification: Refer to `.ai/principles/engineering.md`
- Mandatory Finding Validation Gate: Refer to `.ai/skills/finding-validation/SKILL.md`
- Testing: Refer to `.ai/principles/testing.md`
- Security: Refer to `.ai/principles/security.md`
- Git: Refer to `.ai/principles/git.md`

## Testing Commands
- Backend: `cd TimeWave-backend && python manage.py test`
- Web Frontend: `cd TimeWaveFront && pnpm run lint` / `pnpm run build`
- Mobile: `cd TimeWaveNative && npm test`


# MANDATORY ENGINEERING LIFECYCLE

For every user request:

1. Classify the task automatically.
2. Select the appropriate workflow.
3. Load the required principles, skills, and checklists.
4. For any Bug, Security, Performance, Concurrency, or Audit finding:
   ALWAYS execute the Finding Validation Gate before proposing or applying fixes.
5. Never fix a finding before validation.
6. FALSE POSITIVE findings must never be modified.
7. NEEDS MORE EVIDENCE findings must remain unresolved until evidence exists.
8. For confirmed findings, create an implementation plan.
9. Do not modify application source code until the user explicitly approves the implementation plan, unless the user explicitly requested immediate implementation.
10. After implementation:

    * run tests
    * review changes
    * re-test
    * perform final verification
    * report changed files and test results.
