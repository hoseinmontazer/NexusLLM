# Skill: Feature Implementation

## Purpose
Safely design and implement new capabilities or extensions while maintaining system integrity, backwards compatibility, and test coverage.

## Execution Steps

1. **Requirements Analysis**:
   - Parse requirements carefully. Identify core functional requirements, edge cases, permission constraints, and UI/UX expectations.

2. **Architectural Inspection**:
   - Inspect existing implementation patterns in the codebase. Maintain visual, operational, and structural consistency.

3. **Plan Implementation**:
   - Draft an implementation plan covering:
     - Backend: Models, migrations, serializers, views, permissions, URLs.
     - Frontend Web: Service APIs, state management, UI components, pages.
     - Mobile: Service clients, context, navigation screens, UI components.
     - Tests & Verification.

4. **Incremental Implementation**:
   - Implement backend data models and migrations first.
   - Implement REST serializers, viewsets, permissions, and routes.
   - Implement frontend service methods and React/React-Native UI components.
   - Avoid modifying unrelated business logic or files.

5. **Testing & Static Verification**:
   - Run relevant test suites (`manage.py test <app>`).
   - Execute linter/static check commands where applicable (`pnpm run lint`, `python manage.py check`).

6. **Self-Review & Polish**:
   - Review diffs for unintended side effects, missing imports, or missing error handling.
