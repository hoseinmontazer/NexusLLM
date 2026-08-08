# Skill: Performance Audit

## Purpose
Identify performance bottlenecks, inefficient queries, redundant computations, and unoptimized network requests.

## Scope & Checks

1. **Database Queries**:
   - Detect N+1 queries in Django viewsets and calculation functions (e.g. `select_related('group', 'user')`).
   - Check presence of database indexes on foreign keys, `is_active`, and date range query fields (`effective_from`, `effective_to`).

2. **Calculation Engines & Loops**:
   - Audit performance of `calendar_engine.py` resolution methods and in-request caching (`EngineCache`).
   - Ensure bulk operations (`bulk_assign`) batch operations efficiently.

3. **Frontend & Network**:
   - Inspect web component re-rendering and API payload sizes.
   - Verify static asset optimization and Vite production build size.
