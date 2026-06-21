# Model Aliases

Aliases let you map virtual model names to real ones. Your code continues using a stable name (e.g., `gpt-4o`) while you swap the backend model without touching client code.

---

## Alias scopes

| Scope | Applies to |
|---|---|
| `global` | All teams across all orgs |
| `org` | All teams within a specific org |
| `team` | One specific team only |

More specific scopes take precedence: team > org > global.

---

## Create an alias

### Global alias (any team can use it)

```bash
curl -X POST http://localhost:8081/admin/v1/aliases \
  -H "Content-Type: application/json" \
  -d '{
    "alias":      "gpt-4o",
    "model_name": "qwen3-32b",
    "scope":      "global"
  }'
```

Now any request with `"model": "gpt-4o"` routes to `qwen3-32b`.

### Org-scoped alias

```bash
curl -X POST http://localhost:8081/admin/v1/aliases \
  -H "Content-Type: application/json" \
  -d '{
    "alias":      "fast-model",
    "model_name": "gemma2:2b",
    "scope":      "org",
    "org_id":     "ORG_ID"
  }'
```

### Team-scoped alias

```bash
curl -X POST http://localhost:8081/admin/v1/aliases \
  -H "Content-Type: application/json" \
  -d '{
    "alias":      "code-model",
    "model_name": "qwen2.5-coder:7b",
    "scope":      "team",
    "team_id":    "TEAM_ID"
  }'
```

---

## List aliases

```bash
curl http://localhost:8081/admin/v1/aliases
```

---

## Test an alias

```bash
curl "http://localhost:8081/admin/v1/aliases/resolve?alias=gpt-4o&team_id=TEAM_ID&org_id=ORG_ID"
```

Response:
```json
{"alias": "gpt-4o", "resolved": "qwen3-32b", "scope": "global"}
```

---

## Delete an alias

```bash
curl -X DELETE http://localhost:8081/admin/v1/aliases \
  -H "Content-Type: application/json" \
  -d '{"alias": "gpt-4o", "scope": "global"}'
```

---

## How aliases work

Alias resolution happens **before** any policy check. If a request arrives with `"model": "gpt-4o"`:

1. `aliasResolver.Resolve("gpt-4o", teamID, orgID)` is called
2. Checks Redis cache first (`nexus:alias:team:<id>:gpt-4o`, TTL 5 min)
3. Falls back to PostgreSQL if not cached
4. Resolves: team alias > org alias > global alias
5. Returns `"qwen3-32b"` — the rest of the pipeline uses this name

This means:
- The API key's team must have permission for the **resolved** model (`qwen3-32b`), not the alias
- Usage is tracked under the resolved model name
- The `X-Nexus-Model` response header shows the resolved name

---

## Common patterns

### OpenAI compatibility layer

Map all OpenAI model names to your local models:

```bash
curl -X POST http://localhost:8081/admin/v1/aliases -d '{"alias":"gpt-4o","model_name":"qwen3-32b","scope":"global"}'
curl -X POST http://localhost:8081/admin/v1/aliases -d '{"alias":"gpt-4o-mini","model_name":"gemma2:2b","scope":"global"}'
curl -X POST http://localhost:8081/admin/v1/aliases -d '{"alias":"gpt-3.5-turbo","model_name":"phi3:mini","scope":"global"}'
curl -X POST http://localhost:8081/admin/v1/aliases -d '{"alias":"text-embedding-3-small","model_name":"bge-m3","scope":"global"}'
```

Now all existing OpenAI code works with zero changes.

### A/B testing

Point the alias to different models for different teams:

```bash
# Team A uses the fast model
curl -X POST http://localhost:8081/admin/v1/aliases \
  -d '{"alias":"default-chat","model_name":"gemma2:2b","scope":"team","team_id":"TEAM_A_ID"}'

# Team B uses the large model
curl -X POST http://localhost:8081/admin/v1/aliases \
  -d '{"alias":"default-chat","model_name":"qwen3-32b","scope":"team","team_id":"TEAM_B_ID"}'
```

Both teams call `"model": "default-chat"` — they get different backends.
