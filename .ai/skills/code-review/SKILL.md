# Skill: Code Review

## Purpose
Perform a rigorous, senior-level code review of pending changes, pull requests, or newly created features to ensure quality and compliance.

## Review Checklist

1. **Functional Correctness**:
   - Does the implementation satisfy all requirements without side effects?
   - Are edge cases (null values, boundary conditions, overnight calculations) handled?

2. **Architecture & Design**:
   - Does it adhere to project patterns and directory conventions?
   - Is logic placed in appropriate layers (models, serializers, service layers, components)?

3. **Security**:
   - Are authentication and permission checks enforced?
   - Is tenant isolation maintained? Are querysets properly filtered by group/workspace?

4. **Performance**:
   - Are ORM queries optimized? Are database lookups using `select_related`/`prefetch_related`?

5. **Maintainability & Documentation**:
   - Are variable and function names descriptive?
   - Are docstrings and comments accurate and non-redundant?

## Severity Classification
- **Critical**: Security vulnerability, data loss risk, API breaking change.
- **High**: Functional defect, permission bypass, severe performance bottleneck.
- **Medium**: Code duplication, missing error handling, missing tests.
- **Low**: Code cleanup, mild readability improvements.
- **Info**: Stylistic suggestions or context notes.
