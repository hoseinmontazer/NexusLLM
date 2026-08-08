# Skill: Security Audit

## Purpose
Inspect the repository for security vulnerabilities, permission leaks, unvalidated inputs, and sensitive data exposure.

## Scope & Checks

1. **Authentication & Permissions**:
   - Verify every ViewSet in Django backend specifies `permission_classes`.
   - Inspect custom permission classes (`IsGroupAdminOrManager`, `IsWorkspaceOwner`).

2. **Multi-Tenant Data Isolation**:
   - Ensure all queries are scoped by `group_id` / `workspace_id`.
   - Prevent horizontal privilege escalation (user accessing another user's group/attendance/leaves without authorization).

3. **Input Sanitization & Injection**:
   - Verify all API endpoints parse request payloads using Django REST serializers.
   - Verify raw parameters are not passed to dynamic SQL execution.

4. **Secret & Credential Management**:
   - Ensure no credentials, secret keys, or private keys are checked into source code.
   - Verify `.env.example` lists variables without real secret values.
