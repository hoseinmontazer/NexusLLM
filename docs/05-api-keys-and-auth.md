# API Keys & Authentication

NexusLLM supports two authentication methods:
- **API Keys** — the primary method for applications (`nxs_...` prefix)
- **JWT tokens** — for user-facing integrations

---

## API Keys

### Create an API key

```bash
curl -X POST http://localhost:8081/admin/v1/teams/TEAM_ID/api-keys \
  -H "Content-Type: application/json" \
  -d '{"name": "my-production-app"}'
```

Response (key shown **only once**):
```json
{
  "id":         "uuid-...",
  "team_id":    "uuid-...",
  "name":       "my-production-app",
  "key":        "nxs_a3f9d2b1c8e7...",
  "key_prefix": "nxs_a3f9",
  "active":     true,
  "created_at": "2026-06-21T10:00:00Z"
}
```

> **Save the `key` value immediately.** It is shown only once and never stored in plaintext. NexusLLM stores a SHA-256 hash.

### Create with expiry

```bash
curl -X POST http://localhost:8081/admin/v1/teams/TEAM_ID/api-keys \
  -H "Content-Type: application/json" \
  -d '{
    "name":       "temp-access",
    "expires_at": "2026-12-31T23:59:59Z"
  }'
```

### List keys for a team

```bash
curl http://localhost:8081/admin/v1/teams/TEAM_ID/api-keys
```

Returns all keys with prefix and metadata — never the actual key value.

### Revoke a key

```bash
curl -X DELETE http://localhost:8081/admin/v1/api-keys/KEY_ID
```

Revocation takes effect immediately — the Redis cache is purged so the key stops working within milliseconds.

---

## Using API keys

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

```typescript
import OpenAI from 'openai';

const client = new OpenAI({
  baseURL: 'http://localhost:8080/v1',
  apiKey: 'nxs_YOUR_KEY_HERE',
});
```

---

## How authentication works

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
  { org_id, team_id, team_name, team_priority, permissions[] }
```

**Performance:** After the first request, all subsequent requests for the same key are served entirely from Redis (~0.2ms). No database query on the hot path.

---

## JWT tokens

JWTs are available for programmatic token generation (e.g., short-lived tokens for specific users).

Issue a JWT (requires implementing a call to `auth.Service.IssueJWT`):
```go
// In your custom code:
token, err := authSvc.IssueJWT(ctx, &auth.TeamClaims{
    OrgID:    "uuid-...",
    TeamID:   "uuid-...",
    TeamName: "platform-eng",
}, 24*time.Hour)
```

Use it the same way as an API key:
```bash
-H "Authorization: Bearer eyJhbGci..."
```

JWTs are validated with HMAC-SHA256 using the secret in `NEXUS_AUTH_JWTSECRET`.

---

## Security notes

- Never commit API keys to source control
- Set `expires_at` for keys used in CI/CD pipelines
- The admin API has no authentication by default — restrict it to internal network access (firewall, VPN)
- Set a strong `NEXUS_AUTH_JWTSECRET` (at least 32 random bytes) in production: `openssl rand -hex 32`
