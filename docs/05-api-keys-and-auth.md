# API Keys & Authentication

NexusLLM supports two authentication methods:
- **API Keys** — the primary method for applications (`nxs_...` prefix) with support for project scoping and zero-downtime key rotation.
- **JWT tokens** — for developer self-service portal sessions and user authentication.

---

## API Keys

### Create an API key (Team or Project Scoped)

```bash
# Scope key to a Team (Admin API)
curl -X POST http://localhost:8081/admin/v1/teams/TEAM_ID/api-keys \
  -H "Content-Type: application/json" \
  -d '{"name": "my-production-app"}'

# Scope key to a Project (Developer Portal API)
curl -X POST http://localhost:8081/portal/v1/projects/PROJECT_ID/api-keys \
  -H "Authorization: Bearer YOUR_JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "production-key-v1"}'
```

Response (key secret shown **only once**):
```json
{
  "id":         "uuid-...",
  "team_id":    "uuid-...",
  "name":       "my-production-app",
  "api_key_secret": "nxs_a3f9d2b1c8e7...",
  "key_prefix": "nxs_a3f9",
  "active":     true,
  "created_at": "2026-06-21T10:00:00Z"
}
```

> **Save the `api_key_secret` value immediately.** It is shown only once and never stored in plaintext. NexusLLM stores a SHA-256 hash.

---

### Zero-Downtime Key Rotation

To rotate an API key without service interruption, use the rotation API. The old key remains active for a **24-hour grace period** while the new key is immediately provisioned:

```bash
curl -X POST http://localhost:8081/portal/v1/api-keys/KEY_ID/rotate \
  -H "Authorization: Bearer YOUR_JWT_TOKEN"
```

Response:
```json
{
  "message": "key rotated successfully (old key remains active for 24h grace period)",
  "old_key_id": "key-uuid-1",
  "old_key_grace_expires": "2026-08-07T12:00:00Z",
  "new_key_id": "key-uuid-2",
  "new_key_prefix": "nxs_b4e8",
  "new_api_key_secret": "nxs_b4e8c3d2e1f0..."
}
```

---

### Revoke a Key

```bash
curl -X DELETE http://localhost:8081/portal/v1/api-keys/KEY_ID
```

Revocation takes effect immediately — the Redis cache is purged so the key stops working within milliseconds.

---

## Using API Keys

Pass the key as a Bearer token in the `Authorization` header:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer nxs_YOUR_KEY_HERE" \
  -H "Content-Type: application/json" \
  -d '{"model": "gemma2:2b", "messages": [...]}'
```

This works with any OpenAI-compatible SDK:

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="nxs_YOUR_KEY_HERE"
)

response = client.chat.completions.create(
    model="gemma2:2b",
    messages=[{"role": "user", "content": "Hello!"}]
)
```

---

## How Authentication Works

```
Request arrives with "Authorization: Bearer nxs_abc123..."
        │
        ▼
SHA-256 hash the token
        │
        ▼
Redis lookup: nexus:apikey:<hash>
  ├── HIT  → return cached TeamClaims (TTL: 5 min)
  └── MISS → PostgreSQL query:
              JOIN api_keys → teams → organizations
              check active=TRUE, expires_at > NOW()
              load model permissions
              cache result in Redis
        │
        ▼
Attach TeamClaims to request context:
  { org_id, team_id, project_id, permissions[] }
```

**Performance:** After the first request, all subsequent requests for the same key are served entirely from Redis (~0.2ms). No database query on the hot path.

---

## Developer JWT Authentication & Registration

The Developer Portal uses JWT tokens issued via self-registration or login:

- **Register**: `POST /portal/v1/auth/register`
- **Login**: `POST /portal/v1/auth/login`
- **Admin Login**: `POST /admin/v1/auth/login`
- **Profile Management**: `GET /portal/v1/profile` & `PUT /portal/v1/profile`
