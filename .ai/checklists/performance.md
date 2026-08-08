# Checklist: Performance

- [ ] All foreign key relationship lookups in API views use `select_related` or `prefetch_related`.
- [ ] Database indexes present on frequently filtered columns (`is_active`, `group`, date ranges).
- [ ] Bulk actions avoid per-item database save loops where possible.
- [ ] Heavy calculations use in-memory request caching (`EngineCache`).
- [ ] Frontend render trees avoid unnecessary re-renders.
