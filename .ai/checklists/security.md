# Checklist: Security

- [ ] Every Django ViewSet has `permission_classes` configured.
- [ ] Every query filters by `group` / `workspace` to prevent cross-tenant access.
- [ ] Request parameters are validated using serializers.
- [ ] No hardcoded passwords, API keys, or private tokens in source code.
- [ ] Error messages do not expose sensitive internal stack traces to users.
- [ ] XSS protection active on web frontend outputs.
