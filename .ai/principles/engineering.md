# Core Engineering Principles

These principles govern all software engineering work across this repository.

## 1. Single Source of Truth
- System architecture, domain logic, and business rules must have a single authoritative definition.
- AI assistant configuration and instructions must derive from `.ai/` as the single source of truth.

---

## 2. Automatic Task Classification & Workflow Selection
The agent must automatically infer the task type from user requests and select the appropriate workflow lifecycle:

| User Task Trigger / Example | Inferred Task Type | Default Workflow Flow |
| :--- | :--- | :--- |
| "Why does X fail / return 500?" | **BUG** | `deep-investigate` -> `reproduce` -> `root-cause` -> **Finding Validation Gate** -> `fix` -> `regression-test` -> `review` -> `verify` |
| "Add feature / implement X" | **FEATURE** | `deep-investigate` -> `implementation-plan` -> `implement` -> `test` -> `review` -> `verify` |
| "Audit security / auth check" | **SECURITY ISSUE** | `deep-investigate` -> `security-audit` -> **Finding Validation Gate** -> `fix confirmed findings` -> `security-tests` -> `verify` |
| "Why is endpoint X slow?" | **PERFORMANCE ISSUE**| `deep-investigate` -> `performance-audit` -> `baseline` -> **Finding Validation Gate** -> `fix confirmed` -> `benchmark` -> `verify` |
| "Find race condition / lock issue" | **CONCURRENCY ISSUE**| `deep-investigate` -> `concurrency-audit` -> `reproduce` -> **Finding Validation Gate** -> `fix` -> `concurrency-tests` -> `verify` |
| "Audit application / module" | **DEEP AUDIT** | `deep-investigate` -> `deep-audit` -> **Finding Validation Gate** -> `report` *(Fixes ONLY after explicit user request)* |
| "Review my PR / changes" | **CODE REVIEW** | `code-review` -> `classify findings` -> `report` |
| "Refactor service / view" | **REFACTORING** | `test baseline` -> `refactor` -> `test` -> `verify` |
| "How does X work?" | **INVESTIGATION** | `deep-investigation` -> `trace paths` -> `report` |

---

## 3. Finding Validation Gate (MANDATORY)
Whenever any bug, security issue, performance bottleneck, architectural defect, or audit finding is discovered:

**DO NOT immediately fix it.**

Execute the validation gate:
`FINDING -> RE-INSPECT SOURCE -> VERIFY CLAIM -> CHECK FRAMEWORK BEHAVIOR -> CHECK TESTS -> CLASSIFY`

- **CONFIRMED**: Issue is verified and technically accurate. Approved for fix.
- **PARTIALLY CONFIRMED**: Behavior exists, but severity/scope was inaccurate. Approved for scope-adjusted fix.
- **FALSE POSITIVE**: Claimed behavior does not occur due to framework behavior or existing code handling. **MUST NOT BE FIXED**.
- **NEEDS MORE EVIDENCE**: Requires empirical trace or logs. Remains open without code changes.

### Audit Task Constraint
For audit tasks, default behavior is **AUDIT -> FINDINGS -> VALIDATION -> REPORT**. Do NOT modify application code during an audit unless the user explicitly requests fixes for confirmed findings.

---

## 4. Proportionality Guidelines

- **Small Task**: `UNDERSTAND -> CHANGE -> TEST -> VERIFY`
- **Medium Task**: `INVESTIGATE -> PLAN -> IMPLEMENT -> TEST -> REVIEW -> VERIFY`
- **Large / Risky Task**: `INVESTIGATE -> PLAN -> IMPLEMENT -> TEST -> REVIEW -> FIX -> RE-TEST -> VERIFY`

*Note: Security, concurrency, data integrity, and multi-tenant tasks always default to the stricter flow.*

---

## 5. Self-Verification & No False Confidence Rules

Before declaring any task complete, verify:
1. Did I actually inspect the relevant source code?
2. Did I verify my assumptions against framework mechanics?
3. Did I use the correct skill and workflow?
4. Did I run concrete verification commands (`manage.py test`, linters, build checks)?
5. Did I review changes for side effects?
6. Did I investigate test/build failures rather than hiding or swallowing them?
7. Did I validate important findings before attempting fixes?
8. Can I provide empirical output evidence for my completion claim?

**Strict Truth Standards**:
- "I found something" != "This is a real bug" (Must validate first).
- "I changed the code" != "The problem is fixed" (Must test and verify).
- "Tests were not run" != "The implementation works" (Must execute tests).

---

## 6. Standard Final Report Format

For all non-trivial tasks, conclude with:

```markdown
## Task
[Summary of user request]

## Workflow Used
[Selected workflow lifecycle and skills]

## Investigation
[Files inspected and control/data flows traced]

## Findings
[Key issues or architectural observations]

## Finding Validation
- Finding 1: [CONFIRMED | PARTIALLY CONFIRMED | FALSE POSITIVE | NEEDS MORE EVIDENCE]
  - Evidence & Technical Reasoning: ...

## Changes
[Exact files modified and rationales]

## Verification
[Commands executed and empirical results/logs]

## Remaining Issues
[Any unresolved items]

## Risk
[Remaining technical risks]
```
