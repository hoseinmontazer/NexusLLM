# Policies — Rate Limits & Quotas

Policies are enforced **entirely in Redis** with no database calls on the hot path.  
Policy updates take effect **immediately** — the admin server pushes new limits to Redis as a hash (`nexus:policy:<team_id>`) when you call the update API.

---

## Policy fields

| Field | Name | Description | Example |
|---|---|---|---|
| `rpm` | Requests Per Minute | Max API calls per 60-second sliding window | `100` |
| `tpd` | **Tokens Per Day** | Total LLM tokens (input + output) allowed per UTC day | `1000000` |
| `max_concurrent` | Max concurrent requests | In-flight requests at the same time | `20` |
| `max_context_tokens` | Max context length | Max prompt tokens per request | `32768` |

### What is TPD?

**TPD (Tokens Per Day)** is the cumulative token budget for a team across the full UTC day (midnight to midnight).  
Both prompt tokens and completion tokens count toward TPD.

- `tpd: 1000000` → 1 million tokens/day (about 3,000–5,000 average chat responses)
- `tpd: 100` → 100 tokens/day (useful for testing limits)
- `tpd: 0` → no daily token limit enforced

The counter resets automatically at midnight UTC. You can check how many tokens a team has used today via the usage API.

---

## Set a team's policy

```bash
curl -X PUT http://localhost:8081/admin/v1/teams/TEAM_ID/policy \
  -H "Content-Type: application/json" \
  -d '{
    "rpm":                100,
    "tpd":                500000,
    "max_concurrent":     10,
    "max_context_tokens": 8192
  }'
```

The change is applied **instantly** — the gateway reads limits from Redis on every request.

---

## Get a team's policy

```bash
curl http://localhost:8081/admin/v1/teams/TEAM_ID/policy
```

---

## How each limit works

### RPM — Requests per minute (sliding window)

Uses a Redis sorted set (`nexus:ratelimit:<team_id>:rpm`) with a 60-second sliding window.

- Each request adds an entry with timestamp
- Entries older than 60s are trimmed before counting
- If the count ≥ `rpm`, the request is rejected immediately with HTTP 429

This is a **true sliding window** — not a fixed-minute boundary — so burst traffic at minute boundaries doesn't get double capacity.

**Example:** `rpm: 10` → allows at most 10 requests in any 60-second window.

### TPD — Tokens per day

Uses a Redis counter key `nexus:quota:<team_id>:daily:<YYYY-MM-DD>` with 48-hour TTL.

- The counter is incremented **after** each successful request with the actual tokens used
- Before the next request, the counter is read and compared to `tpd`
- If `counter ≥ tpd`, all further requests are rejected with HTTP 429 (`daily_quota_exceeded`)
- Counter resets automatically at midnight UTC (the key expires naturally)

**Example scenarios:**

| TPD | Typical usage | What happens |
|---|---|---|
| `1000000` | ~3,000–5,000 chat responses | Generous limit for active teams |
| `100000` | ~300–500 chat responses | Moderate daily budget |
| `100` | ~1 short chat response | Testing / tight sandbox |
| `0` | Unlimited | No daily cap enforced |

**Check current usage:**
```bash
docker compose exec redis redis-cli GET "nexus:quota:TEAM_ID:daily:$(date +%Y-%m-%d)"
```

### max_concurrent — Concurrency limit

Uses a Redis counter `nexus:inflight:<team_id>` with a 10-minute safety TTL.

- Incremented when a request starts
- Decremented when it completes (on success or error)
- Rejected with HTTP 429 (`concurrency_limit_reached`) if count ≥ `max_concurrent`

For teams that don't need concurrency limits: set `max_concurrent: 0` (unlimited).

### max_context_tokens — Context length limit

The estimated prompt token count is compared to `max_context_tokens` before the request is forwarded.

Estimation: `len(all message text) / 4` — fast and conservative, no tokenizer invoked.

Rejected with HTTP 403 (`context_length_exceeded`) if the estimate exceeds the limit.

---

## Error responses

All policy rejections return HTTP 429 (or 403 for context) with:

```json
{
  "error": {
    "message": "Request rejected by policy engine",
    "type":    "gateway_error",
    "code":    "rate_limit_exceeded"
  }
}
```

| Code | Meaning |
|---|---|
| `rate_limit_exceeded` | RPM window full |
| `daily_quota_exceeded` | TPD reached for today |
| `concurrency_limit_reached` | Too many in-flight requests |
| `context_length_exceeded` | Prompt too long |
| `model_not_allowed` | Team doesn't have access to this model |

---

## Live policy updates

When you call `PUT /admin/v1/teams/:id/policy`, the new values are:
1. Written to PostgreSQL (`policies` table)
2. **Pushed to Redis immediately** (`nexus:policy:<team_id>` hash)

The gateway reads limits from the Redis hash on every request — no restart needed, changes are effective within milliseconds.

---

## Default policy values

New teams without an explicit policy use these defaults:

| Field | Default |
|---|---|
| `rpm` | 100 |
| `tpd` | 1,000,000 |
| `max_concurrent` | 10 |
| `max_context_tokens` | 8,192 |

---

## Priority queuing

When `concurrency_limit_reached` or `gpu_capacity_exhausted` triggers, the request is **queued** rather than hard-rejected. The client receives:
```
HTTP 429  Retry-After: 5
```

Teams are assigned a priority (1–100) when created. Higher-priority teams' queued requests are dispatched first via three Redis Streams:
- **High stream** — teams with priority 70–100
- **Med stream** — teams with priority 35–69
- **Low stream** — teams with priority 1–34

---

---

## Project-level priority (weighted)

Projects introduce finer-grained SLA scheduling above team policies. Every project has a **`priority_weight`** in `[0, 1000]`. Scheduling uses the **effective_priority**, not the raw weight:

```
effective_priority = base_weight
                   + waiting_bonus      (anti-starvation aging: +1/60s, cap +200)
                   + reservation_bonus  (+50 when project has reserved quota)
                   - resource_penalty   (-100 when consuming beyond max quota)
```

| Weight range | Label | Preemption |
|---|---|---|
| 900–1000 | Production Critical / Emergency | Preempts anything with gap ≥ 50 |
| 700–800 | Core Internal | Preempts ≤ 650 |
| 500 | Standard | Preempts ≤ 450 |
| 300 | Batch | Limited |
| 100 | Development | No effective preemption |
| 0–50 | Playground / Best Effort | Never preempts |

**Preemption rule:** requestor's `effective_priority` must exceed victim's `priority_weight` by ≥ 50 points. Projects marked `preemptible=false` or `protected=true` can never be evicted.

```bash
# Create a project with priority 900 and 80 GB reserved VRAM
curl -X POST http://localhost:8081/admin/v1/projects \
  -H 'Content-Type: application/json' \
  -d '{"organization_id":"ORG","team_id":"TEAM",
       "name":"Customer Chat","priority_weight":900}'

# Set resource reservation + quota ceiling
curl -X POST http://localhost:8081/admin/v1/projects/PROJECT_ID/reserve \
  -H 'Content-Type: application/json' \
  -d '{"reserved_vram_mb":81920,"max_gpu_vram_mb":163840}'

# Change priority at runtime (takes effect within one scheduler cycle, ~60 s)
curl -X POST http://localhost:8081/admin/v1/projects/PROJECT_ID/priority \
  -H 'Content-Type: application/json' \
  -d '{"priority_weight":950}'

# Get effective priority breakdown for a project
curl http://localhost:8081/admin/v1/projects/PROJECT_ID
# Returns: priority_weight, effective_priority, waiting_bonus, reservation_bonus, resource_penalty

# View global queue ordered by effective_priority DESC
curl http://localhost:8081/admin/v1/scheduler/queue

# View placement decisions with full trace
curl http://localhost:8081/admin/v1/scheduler/decisions
```

See [docs/20-automatic-scheduler.md](docs/20-automatic-scheduler.md) and [docs/21-weighted-priority.md](docs/21-weighted-priority.md) for full details.

In addition to team policies, org-level gateway policies control:
- Temperature caps per org
- Tool/function-call restrictions
- Model blocklists

Managed via `PUT /admin/v1/gateway-policies/:org_id` (API) or the Admin UI → Policies page.

## Gateway policy (org-level)

In addition to team policies, org-level gateway policies control:
- Temperature caps per org
- Tool/function-call restrictions
- Model blocklists

Managed via `PUT /admin/v1/gateway-policies/:org_id` (API) or the Admin UI → Policies page.
