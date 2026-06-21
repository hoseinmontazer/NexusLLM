# Policies — Rate Limits & Quotas

Policies are enforced **entirely in Redis** with no database calls on the hot path. They are evaluated on every request in milliseconds.

---

## Policy fields

| Field | Description | Example |
|---|---|---|
| `rpm` | Max requests per minute (sliding window) | `500` |
| `tpd` | Max tokens per day (input + output combined) | `10000000` |
| `max_concurrent` | Max in-flight requests at the same time | `20` |
| `max_context_tokens` | Max prompt length the team can send | `32768` |

---

## Set a team's policy

```bash
curl -X PUT http://localhost:8081/admin/v1/teams/TEAM_ID/policy \
  -H "Content-Type: application/json" \
  -d '{
    "rpm":                500,
    "tpd":                50000000,
    "max_concurrent":     20,
    "max_context_tokens": 32768
  }'
```

---

## Get a team's policy

```bash
curl http://localhost:8081/admin/v1/teams/TEAM_ID/policy
```

---

## How each limit works

### RPM — Rate limit (sliding window)

Uses a Redis sorted set (`nexus:ratelimit:<team_id>:rpm`) with a 60-second sliding window.

- Each request adds an entry with timestamp
- Entries older than 60s are trimmed
- If the count ≥ `rpm`, the request is rejected with HTTP 429

The window is a true sliding window, not a fixed minute boundary, so bursts at minute boundaries don't get double capacity.

### TPD — Daily token quota

Uses a Redis counter (`nexus:quota:<team_id>:daily:<YYYY-MM-DD>`) with 48-hour TTL.

- After every request, actual tokens used (prompt + completion) are added
- If the counter exceeds `tpd`, requests are rejected with HTTP 429

The counter resets automatically at midnight UTC (key expires).

### Concurrency

Uses a Redis counter (`nexus:inflight:<team_id>`) with 10-minute TTL (safety valve).

- Incremented when a request starts
- Decremented when it completes (even on error)
- Rejected with HTTP 429 if count ≥ `max_concurrent`

### Context length

Estimated token count of the prompt is compared to `max_context_tokens`. The estimate uses `len(text) / 4` which is good enough for enforcement purposes (actual tokenizer isn't invoked for speed).

### GPU capacity

If the target model's vLLM pool is at >90% GPU KV-cache utilization, requests are rejected or queued via the scheduler (`nexus:pool:<model>:at_capacity`). The scheduler's GPU watcher updates this every 5 seconds.

---

## What happens on rejection

Rejected requests return HTTP 429 with a `Retry-After: 5` header and a JSON error:

```json
{
  "error": {
    "message": "Request rejected by policy engine",
    "type":    "gateway_error",
    "code":    "rate_limit_exceeded"
  }
}
```

Some rejections trigger **queuing** instead of hard rejection — the request is placed in the scheduler's priority queue and the client receives `Retry-After: 5`. The team's `priority` field determines which queue (high/med/low) the request goes into.

---

## Priority queuing

The scheduler has three Redis Streams:
- **High** (team priority 70–100)
- **Medium** (team priority 35–69)
- **Low** (team priority 1–34)

Higher-priority teams' requests are dispatched first. Long-waiting jobs are promoted to higher streams to prevent starvation (every 10 seconds, jobs waiting >30 seconds move up one level).

Set a team's priority when creating:
```json
{"org_id": "...", "name": "Critical Team", "slug": "critical", "priority": 90}
```

Or update it:
```bash
# Currently via DB — UI coming in v0.6
docker compose exec postgres psql -U nexus -d nexusllm \
  -c "UPDATE teams SET priority=90 WHERE slug='critical';"
```

---

## Default policy values

New teams without an explicit policy use these defaults (applied in the gateway):

| Field | Default |
|---|---|
| `rpm` | 100 |
| `tpd` | 1,000,000 |
| `max_concurrent` | 10 |
| `max_context_tokens` | 8192 |

---

## Gateway policy (org-level)

In addition to team policies, there are org-level gateway policies for:
- Temperature caps (prevent teams from using temperature > X)
- Tool restrictions (disable function calling for certain orgs)
- Model restrictions

These are managed via `gatewaypolicy.Engine` and stored in PostgreSQL. The web UI for this is on the roadmap (v0.6).
