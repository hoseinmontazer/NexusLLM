# Security Principles

## 1. Authentication & Authorization
- Every API endpoint in `TimeWave-backend` must enforce strict authentication and permission classes (`permissions.IsAuthenticated`, `IsGroupAdminOrManager`).
- Verify multi-tenant / group isolation in every queryset (e.g. check `GroupMembership` or `workspace.owner`).

## 2. Data Protection & Secrets
- Never hardcode API keys, passwords, credentials, or private keys in source code.
- Sensitive user data (biometric hashes, tokens, passwords) must be protected and never logged in plain text.
- Inspect `.env` / environment variable patterns (`django-environ`, `process.env`).

## 3. Input Validation & Injection Defense
- Validate all incoming user inputs via Django REST Serializers or strict validation routines.
- Avoid raw SQL queries unless parameterised.
- Use safe ORM queries to prevent SQL injection.
- Sanitize web outputs to protect against XSS in React components.
