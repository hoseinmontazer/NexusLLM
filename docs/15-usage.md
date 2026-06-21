# Usage & Billing

NexusLLM tracks every request's token counts and latency, aggregates them hourly and daily, and provides cost estimates per team and org.

---

## How usage is tracked

```
Request completes
      │
      ▼
Usage event published to Redis Stream (nexus:usage:events)
      │  ~0.1ms, non-blocking
      ▼
Background consumer reads stream (batches of 200)
      │
      ▼
INSERT into usage_events (PostgreSQL)
      │
      ▼  (every hour, background goroutine)
Aggregated into usage_hourly and usage_daily
```

The usage pipeline never blocks inference. Even if PostgreSQL is slow, the Redis Stream buffers up to 100,000 events.

---

## Usage event fields

| Field | Description |
|---|---|
| `org_id` / `team_id` | Who made the request |
| `model_name` | Which model (resolved, after alias) |
| `endpoint_id` | Which backend endpoint served it |
| `prompt_tokens` | Input token count |
| `completion_tokens` | Output token count |
| `total_tokens` | Sum |
| `latency_ms` | Total request time from gateway |
| `ttft_ms` | Time to first token (streaming only) |
| `status` | `success` or `error_NNN` |
| `cost_usd` | Estimated cost |

---

## Cost model

Current rates (hardcoded, per-model pricing coming in v0.7):
- Input: $0.50 per 1M tokens
- Output: $1.50 per 1M tokens

```
cost = (prompt_tokens / 1,000,000 × 0.50) + (completion_tokens / 1,000,000 × 1.50)
```

---

## Query usage

### Team daily usage

```bash
curl "http://localhost:8081/admin/v1/usage/teams/TEAM_ID?from=2026-06-01&to=2026-06-30"
```

Response:
```json
{
  "data": [
    {
      "team_id":           "uuid-...",
      "model_name":        "gemma2:2b",
      "day":               "2026-06-21",
      "request_count":     1247,
      "error_count":       3,
      "prompt_tokens":     892340,
      "completion_tokens": 445120,
      "total_tokens":      1337460,
      "cost_usd":          1.114
    }
  ]
}
```

### Org monthly spend

```bash
curl http://localhost:8081/admin/v1/usage/orgs/ORG_ID/monthly-spend
```

Response:
```json
{"monthly_spend_usd": 47.83}
```

### Trigger manual aggregation

If you need fresh hourly/daily rollups now (aggregation normally runs every hour):

```bash
curl -X POST http://localhost:8081/admin/v1/usage/aggregate
```

---

## Usage data schema

### `usage_events` (raw)

Every request, written asynchronously.

```sql
SELECT model_name, COUNT(*), SUM(total_tokens), AVG(latency_ms)
FROM usage_events
WHERE team_id = 'TEAM_ID'
  AND created_at > NOW() - INTERVAL '1 hour'
GROUP BY model_name;
```

### `usage_daily` (aggregated)

Pre-aggregated per team per model per day. Fastest for reporting.

```sql
SELECT day, model_name, request_count, total_tokens, cost_usd
FROM usage_daily
WHERE team_id = 'TEAM_ID'
ORDER BY day DESC;
```

---

## Redis keys for current usage

Check live rate-limit counters:

```bash
# Current RPM request count for a team
redis-cli ZCARD "nexus:ratelimit:TEAM_ID:rpm"

# Today's token usage
redis-cli GET "nexus:quota:TEAM_ID:daily:$(date +%Y-%m-%d)"

# Active inflight requests
redis-cli GET "nexus:inflight:TEAM_ID"
```
