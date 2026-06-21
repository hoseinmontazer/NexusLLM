# Organizations & Teams

NexusLLM uses a two-level tenant hierarchy: **Organizations** contain **Teams**.

```
Organization: Acme Corp
  ├── Team: Platform Engineering   (high priority, high limits)
  ├── Team: Data Science           (medium priority)
  └── Team: Staging / QA           (low priority)
```

Each team gets its own:
- API keys
- Rate limits and token quotas
- Model access permissions
- Usage tracking

---

## Organizations

### Create an organization

```bash
curl -X POST http://localhost:8081/admin/v1/orgs \
  -H "Content-Type: application/json" \
  -d '{"name": "Acme Corp", "slug": "acme-corp"}'
```

Response:
```json
{
  "id": "uuid-...",
  "name": "Acme Corp",
  "slug": "acme-corp",
  "active": true
}
```

### List organizations

```bash
curl http://localhost:8081/admin/v1/orgs
```

### Delete (deactivate) an organization

```bash
curl -X DELETE http://localhost:8081/admin/v1/orgs/ORG_ID
```

Deactivating an org deactivates all its teams. Existing API keys stop working immediately.

---

## Teams

### Create a team

```bash
curl -X POST http://localhost:8081/admin/v1/teams \
  -H "Content-Type: application/json" \
  -d '{
    "org_id":   "ORG_ID",
    "name":     "Platform Engineering",
    "slug":     "platform-eng",
    "priority": 80
  }'
```

**`priority`** (1–100) controls job queue precedence in the scheduler:
- 70–100: high priority stream
- 35–69: medium priority
- 1–34: low priority

### List teams

```bash
curl "http://localhost:8081/admin/v1/teams"
# or filter by org:
curl "http://localhost:8081/admin/v1/teams?org_id=ORG_ID"
```

### Get a team

```bash
curl http://localhost:8081/admin/v1/teams/TEAM_ID
```

### Delete a team

```bash
curl -X DELETE http://localhost:8081/admin/v1/teams/TEAM_ID
```

---

## Team policies

Each team has a policy that controls request rates and limits.

### View a team's policy

```bash
curl http://localhost:8081/admin/v1/teams/TEAM_ID/policy
```

### Update a team's policy

```bash
curl -X PUT http://localhost:8081/admin/v1/teams/TEAM_ID/policy \
  -H "Content-Type: application/json" \
  -d '{
    "rpm":               500,
    "tpd":               10000000,
    "max_concurrent":    20,
    "max_context_tokens": 32768
  }'
```

| Field | Description | Default |
|---|---|---|
| `rpm` | Requests per minute (sliding window) | 100 |
| `tpd` | Tokens per day (input + output) | 1,000,000 |
| `max_concurrent` | Max inflight requests at once | 10 |
| `max_context_tokens` | Max prompt length in tokens | 8192 |

All limits are enforced in Redis with no database calls on the hot path.

---

## Model permissions

A team can only call models it has been explicitly granted access to.

### Grant a model

```bash
curl -X POST http://localhost:8081/admin/v1/teams/TEAM_ID/models \
  -H "Content-Type: application/json" \
  -d '{"model_name": "gemma2:2b"}'
```

### Revoke a model

```bash
curl -X DELETE http://localhost:8081/admin/v1/teams/TEAM_ID/models/gemma2:2b
```

### What happens on the gateway?

When a request arrives with `"model": "gemma2:2b"`, the gateway checks Redis key `nexus:team:<team_id>:models` (a Set). If the model is not in that set, the request is rejected with:

```json
{"error": {"message": "Request rejected by policy engine", "code": "model_not_allowed"}}
```

Permissions are seeded into Redis on gateway startup and updated immediately when you call the API.
