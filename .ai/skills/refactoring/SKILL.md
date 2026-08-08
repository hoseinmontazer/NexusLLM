# Skill: Refactoring

## Purpose
Safely restructure existing code to improve maintainability, legibility, and cohesion without changing external runtime behavior.

## Refactoring Protocol

1. **Establish Baseline Tests**:
   - Verify that test coverage exists for the targeted code. Run tests before touching code to establish a clean baseline.

2. **Behavior-Preserving Incremental Edits**:
   - Make small, incremental modifications (e.g. extracting helpers, unifying logic, removing duplicate code).
   - Do not mix unrelated feature additions or bug fixes during a refactoring pass.

3. **Verify After Every Step**:
   - Re-run test suites after each refactoring step.

4. **Justification**:
   - Clearly document why the refactored code is cleaner, more maintainable, or safer than the baseline.
