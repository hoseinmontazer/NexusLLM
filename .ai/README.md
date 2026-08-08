# Portable AI Engineering System (.ai/)

This directory defines the **Portable AI Engineering Methodology** for the `TimeWave` codebase.

It serves as the **SINGLE SOURCE OF TRUTH** for engineering principles, workflow execution, finding validation gates, auditing standards, and quality verification across all AI coding tools:
- **Google Antigravity**
- **Kiro**
- **Claude Code**

---

## 1. Task Lifecycle & Finding Validation Gate

```
                         USER TASK
                             │
                  [ Automatic Classification ]
                             │
                   [ Select Workflow ]
                             │
              ┌──────────────┴──────────────┐
              ▼                             ▼
       Feature Task                    Issue / Audit Task
              │                             │
    Deep Investigation              Deep Investigation
              │                             │
     Implementation Plan              Deep Audit / Security
              │                             │
          Implement                 Finding Validation Gate
              │                     (CONFIRMED / PARTIALLY CONFIRMED)
              │                             │
              │                      Fix Confirmed Only
              │                             │
              └──────────────┬──────────────┘
                             ▼
                           Testing
                             │
                         Code Review
                             │
                           Re-test
                             │
                    Final Verification
                             │
                     Final Report Format
```

### The Finding Validation Gate (Mandatory Rule)
Whenever an agent discovers a defect, vulnerability, performance bottleneck, concurrency risk, or architectural issue:
`FINDING -> RE-INSPECT SOURCE -> VERIFY CLAIM -> CHECK FRAMEWORK BEHAVIOR -> CHECK TESTS -> CLASSIFY`

Possible classifications:
- **CONFIRMED**: Verified runtime defect. Approved for fix.
- **PARTIALLY CONFIRMED**: Real issue, but scope/severity adjusted. Approved for scope-adjusted fix.
- **FALSE POSITIVE**: Behaviour is handled by framework or existing code. **MUST NOT BE FIXED**.
- **NEEDS MORE EVIDENCE**: Requires trace evidence or staging log. Must remain unresolved without code edits.

---

## 2. Automatic Task Type Mapping

| Task Category | Trigger Example | Workflow Lifecycle |
| :--- | :--- | :--- |
| **Bug** | "Why does endpoint return 500?" | `deep-investigate` -> `reproduce` -> `root-cause` -> **Finding Validation Gate** -> `fix` -> `regression-test` -> `verify` |
| **Feature** | "Add shift scheduling to web" | `deep-investigate` -> `implementation-plan` -> `implement` -> `test` -> `review` -> `verify` |
| **Security** | "Audit authentication permissions" | `deep-investigate` -> `security-audit` -> **Finding Validation Gate** -> `fix confirmed` -> `security-tests` -> `verify` |
| **Performance** | "Optimize slow attendance query" | `deep-investigate` -> `performance-audit` -> `baseline` -> **Finding Validation Gate** -> `fix confirmed` -> `benchmark` -> `verify` |
| **Concurrency** | "Fix race condition in check-in" | `deep-investigate` -> `concurrency-audit` -> `reproduce` -> **Finding Validation Gate** -> `fix` -> `concurrency-tests` -> `verify` |
| **Deep Audit** | "Audit reports app" | `deep-investigate` -> `deep-audit` -> **Finding Validation Gate** -> `report` *(Fixes only after explicit user request)* |
| **Review** | "Review my PR changes" | `code-review` -> `report` |
| **Refactor** | "Refactor group service" | `test baseline` -> `refactor` -> `test` -> `verify` |

---

## 3. Directory Structure

- `principles/`: Baseline engineering rules for code quality, security, testing, git hygiene, finding validation, and task routing (`engineering.md`, `testing.md`, `security.md`, `git.md`).
- `skills/`: Reusable instruction packages (`deep-investigation`, `feature-implementation`, `bug-fixing`, `deep-audit`, `finding-validation`, `testing-and-verification`, `code-review`, `security-audit`, `performance-audit`, `concurrency-audit`, `refactoring`).
- `workflows/`: Multi-step procedure definitions (`deep-investigate.md`, `implement-feature.md`, `fix-bug.md`, `deep-audit.md`, `finding-validation.md`, `test-and-fix.md`, `code-review.md`, `refactor.md`).
- `checklists/`: Quality gates (`implementation.md`, `bug-fix.md`, `audit.md`, `security.md`, `performance.md`, `release.md`).

---

## 4. IDE Mapping

| Component | Portable Source | Antigravity | Kiro | Claude Code |
| :--- | :--- | :--- | :--- | :--- |
| **Principles** | `.ai/principles/*.md` | [.agents/AGENTS.md](file:///.agents/AGENTS.md) | `.kiro/steering/engineering.md` | [CLAUDE.md](file:///CLAUDE.md) |
| **Skills** | `.ai/skills/<name>/SKILL.md` | [.agents/AGENTS.md](file:///.agents/AGENTS.md) | `.kiro/steering/workflows.md` | [CLAUDE.md](file:///CLAUDE.md) |
| **Workflows** | `.ai/workflows/*.md` | [.agents/AGENTS.md](file:///.agents/AGENTS.md) | `.kiro/steering/workflows.md` | [CLAUDE.md](file:///CLAUDE.md) |
| **Checklists** | `.ai/checklists/*.md` | [.agents/AGENTS.md](file:///.agents/AGENTS.md) | `.kiro/steering/workflows.md` | [CLAUDE.md](file:///CLAUDE.md) |
