# CLAUDE.md - Claude Code Adapter

This file connects Claude Code to the **Portable AI Engineering System** defined in `.ai/`.

## Single Source of Truth
The canonical engineering methodology, skills, workflows, principles, finding validation gates, and checklists reside under `.ai/`.
Refer to `.ai/README.md` for full system architecture.

---

## Monorepo Subprojects & Essential Commands

### 1. Django Backend (`TimeWave-backend`)
- **Run Dev Server**: `python manage.py runserver`
- **Run Unit Tests**: `python manage.py test <app_name>`
- **System Checks**: `python manage.py check`
- **Migration Check**: `python manage.py migrate --check`
- **Make Migrations**: `python manage.py makemigrations`
- **Apply Migrations**: `python manage.py migrate`

### 2. Web Frontend (`TimeWaveFront`)
- **Run Dev Server**: `pnpm run dev`
- **Build Production**: `pnpm run build`
- **Run Linter**: `pnpm run lint`

### 3. Mobile App (`TimeWaveNative`)
- **Start Expo**: `npm start`
- **Build Android Release**: `./build-release-apk.sh`

---

## Automatic Task Classification & Portable Workflows

Infer task type automatically and execute the appropriate workflow in `.ai/workflows/`:

- **Bug Task**: Execute `.ai/workflows/fix-bug.md`
- **Feature Task**: Execute `.ai/workflows/implement-feature.md`
- **Security Issue Task**: Execute `.ai/workflows/deep-audit.md` + `.ai/skills/security-audit/SKILL.md`
- **Performance Issue Task**: Execute `.ai/skills/performance-audit/SKILL.md`
- **Concurrency Issue Task**: Execute `.ai/skills/concurrency-audit/SKILL.md`
- **Deep Audit Task**: Execute `.ai/workflows/deep-audit.md` *(Fixes ONLY after explicit user request)*
- **Review Task**: Execute `.ai/workflows/code-review.md`
- **Refactor Task**: Execute `.ai/workflows/refactor.md`
- **Investigation Task**: Execute `.ai/workflows/deep-investigate.md`

---

## Finding Validation Gate (MANDATORY)

Before fixing any bug, security risk, performance issue, or audit finding:
1. Re-inspect exact source code.
2. Verify runtime claim.
3. Check framework normalization/behavior.
4. Classify: `CONFIRMED` | `PARTIALLY CONFIRMED` | `FALSE POSITIVE` | `NEEDS MORE EVIDENCE`.
5. Fix ONLY `CONFIRMED` or `PARTIALLY CONFIRMED` findings.

---

## Standard Final Report Format

Conclude all substantial tasks with:
- **Task**
- **Workflow Used**
- **Investigation**
- **Findings**
- **Finding Validation**
- **Changes**
- **Verification**
- **Remaining Issues**
- **Risk**
