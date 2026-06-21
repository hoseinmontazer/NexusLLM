# Configuration Reference

NexusLLM is configured via environment variables (or a `.env` file). All variables are prefixed with `NEXUS_`.

---

## Environment variables

### Database

| Variable | Default | Description |
|---|---|---|
| `NEXUS_DATABASE_DSN` | `postgres://nexus:nexus@localhost:5432/nexusllm?sslmode=disable` | PostgreSQL connection string |
| `NEXUS_DATABASE_MAX_OPEN_CONNS` | `25` | Max open DB connections |
| `NEXUS_DATABASE_MAX_IDLE_CONNS` | `5` | Max idle DB connections |

### Redis

| Variable | Default | Description |
|---|---|---|
| `NEXUS_REDIS_ADDR` | `localhost:6379` | Redis address |
| `NEXUS_REDIS_PASSWORD` | `""` | Redis password (empty = no auth) |
| `NEXUS_REDIS_DB` | `0` | Redis database number |

### Auth

| Variable | Default | Description |
|---|---|---|
| `NEXUS_AUTH_JWTSECRET` | `dev-secret` | HMAC-SHA256 secret for JWT signing. **Change in production.** |
| `NEXUS_AUTH_APIKEY_CACHE_TTL` | `5m` | How long validated API keys are cached in Redis |

### Server (gateway)

| Variable | Default | Description |
|---|---|---|
| `NEXUS_SERVER_PORT` | `8080` | Gateway listen port |
| `NEXUS_SERVER_MODE` | `release` | Gin mode: `debug` or `release` |
| `NEXUS_SERVER_READ_TIMEOUT` | `5m` | HTTP read timeout |
| `NEXUS_SERVER_WRITE_TIMEOUT` | `5m` | HTTP write timeout |
| `NEXUS_SERVER_SHUTDOWN_TIMEOUT` | `30s` | Graceful shutdown timeout |
| `NEXUS_SERVER_METRICS_PORT` | `9090` | Prometheus metrics port (gateway) |

### Admin server

| Variable | Default | Description |
|---|---|---|
| `NEXUS_ADMIN_PORT` | `8081` | Admin API listen port |
| `NEXUS_ADMIN_METRICS_PORT` | `9091` | Prometheus metrics port (admin) |

### Runtime watcher

| Variable | Default | Description |
|---|---|---|
| `NEXUS_VLLM_POLL_INTERVAL` | `5s` | How often the watcher health-checks all endpoints |

---

## Example `.env` file

```bash
# Copy this to nexusllm/.env and adjust values

# PostgreSQL
NEXUS_DATABASE_DSN=postgres://nexus:nexus@localhost:5432/nexusllm?sslmode=disable

# Redis
NEXUS_REDIS_ADDR=localhost:6379

# Auth — CHANGE THIS IN PRODUCTION
NEXUS_AUTH_JWTSECRET=change-me-to-a-random-32-byte-string

# Ports
NEXUS_SERVER_PORT=8080
NEXUS_ADMIN_PORT=8081

# Mode (debug shows Gin route table and request logs)
NEXUS_SERVER_MODE=debug
```

Generate a secure JWT secret:
```bash
openssl rand -hex 32
```

---

## Docker Compose configuration

The dev stack is in `docker-compose.yml`:

```yaml
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER:     nexus
      POSTGRES_PASSWORD: nexus
      POSTGRES_DB:       nexusllm
    ports: ["5432:5432"]
    volumes:
      - postgres_data:/var/lib/postgresql/data
      - ./migrations:/migrations:ro

  redis:
    image: redis:7-alpine
    ports: ["6379:6379"]
```

Migrations are mounted at `/migrations` so `make migrate` can run them inside the container.

---

## Production checklist

- [ ] `NEXUS_AUTH_JWTSECRET` set to `$(openssl rand -hex 32)`
- [ ] PostgreSQL SSL: `?sslmode=require` in DSN
- [ ] Redis AUTH: `NEXUS_REDIS_PASSWORD` set
- [ ] Firewall: restrict port 8081 (admin) and 3001 (web UI) to internal network only
- [ ] `NEXUS_SERVER_MODE=release` (removes debug output)
- [ ] Set resource limits on Docker containers
- [ ] Backup PostgreSQL daily (all state lives there)
- [ ] Monitor `nexus_runtime_endpoint_up` Prometheus gauge for model health alerts

---

## Prometheus metrics

### Gateway metrics (`:9090/metrics`)

| Metric | Type | Labels | Description |
|---|---|---|---|
| `nexus_runtime_endpoint_up` | Gauge | model, endpoint_id, host | 1=healthy, 0=down |
| `nexus_runtime_endpoint_health_latency_ms` | Gauge | model, endpoint_id | Last health check latency |
| `nexus_runtime_health_checks_total` | Counter | model, endpoint_id, status | Total health checks |
| `nexus_runtime_endpoint_consecutive_failures` | Gauge | model, endpoint_id | Circuit breaker counter |
| `nexus_runtime_endpoint_active_connections` | Gauge | model, endpoint_id | In-flight requests |
| `nexus_runtime_endpoint_gpu_cache_utilization` | Gauge | model, endpoint_id | vLLM KV-cache % |
| `nexus_gateway_requests_total` | Counter | team, model, status | Total requests |
| `nexus_gateway_tokens_total` | Counter | team, model, type | Token counts |
| `nexus_gateway_active_requests` | Gauge | team, model | Current inflight |
| `nexus_gateway_rejected_requests_total` | Counter | team, reason | Rejected by policy |
| `nexus_gateway_ttft_seconds` | Histogram | team, model | Time to first token |
