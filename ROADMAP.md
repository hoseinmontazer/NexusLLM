# NexusLLM — Enterprise Platform Roadmap

> Current state: Core platform is production-ready for a single GPU server.
> This document defines the remaining phases to reach a complete self-hosted enterprise AI platform.

---

## What Is Already Done ✅

| Capability | Status |
|---|---|
| Organizations / Teams / Users | ✅ |
| API Key + JWT authentication | ✅ |
| Model ACLs | ✅ |
| Rate limits + daily quotas | ✅ |
| OpenAI-compatible inference API | ✅ |
| Runtime Registry (vLLM / Ollama / TGI / OpenAI-compat) | ✅ |
| Runtime Watcher + health checks + circuit breaker | ✅ |
| Model Controller (start / stop / restart / upgrade / rollback via Docker) | ✅ |
| GPU Inventory + allocation + bin-packing | ✅ |
| Dynamic model lifecycle (state machine + idle eviction) | ✅ |
| Prompt Policy Engine (injection, PII, deny lists, output filters) | ✅ |
| AI Gateway Policy Engine (temp cap, tools, model restrictions) | ✅ |
| Usage tracking (async Redis Stream → PostgreSQL) | ✅ |
| Model Aliases / Virtual Models | ✅ |
| Priority queue scheduler (Redis Streams) | ✅ |
| Prometheus + Grafana observability | ✅ |
| Docker Compose single-GPU deployment | ✅ |
| Admin REST API (full CRUD for all entities) | ✅ |

---

## Phase 4 — Enterprise Identity & Access Control

**Goal:** Replace the simple role field with a proper RBAC system and add SSO so employees log in with their company account.

### 4.1 RBAC System

**Roles:**
- `platform_admin` — full platform access
- `org_admin` — full access within one organization
- `team_admin` — manage their own team
- `developer` — create API keys, read usage
- `viewer` — read-only

**Permissions:**
```
users.manage        teams.manage       models.manage
policies.manage     apikeys.manage     usage.read
audit.read          gpu.manage         budget.manage
```

**What needs to be built:**
- DB schema: `roles`, `permissions`, `role_permissions`, `user_roles` tables
- Permission middleware (Go) — replaces current simple role check
- `GET/POST/DELETE /admin/v1/roles` API
- Role assignment API: `POST /admin/v1/users/:id/roles`
- Permission check helper used by all admin handlers

**Estimated effort:** 3–5 days

---

### 4.2 SSO / OIDC Integration

**Supported providers:**
- Keycloak (self-hosted)
- Azure AD
- Google Workspace
- Any OIDC-compliant provider

**What needs to be built:**
- DB schema: `sso_providers`, `user_identities` tables
- OIDC callback handler (Go — use `coreos/go-oidc`)
- Automatic user provisioning on first login
- Group/team mapping from OIDC claims to NexusLLM teams
- Role mapping from OIDC claims to RBAC roles
- Session management (JWT issued after OIDC callback)
- Admin config page for SSO providers

**Flow:**
```
Browser → /auth/sso/:provider → OIDC Provider → /auth/callback
→ provision user if new → map groups → issue NexusLLM JWT → redirect to UI
```

**DB changes needed:**
```sql
CREATE TABLE sso_providers (
    id, name, provider_type, issuer_url, client_id,
    client_secret_enc, team_claim, role_claim, enabled
);
CREATE TABLE user_identities (
    id, user_id, provider_id, external_id, email, last_login_at
);
```

**Estimated effort:** 5–7 days

---

## Phase 5 — Audit Logging

**Goal:** Immutable, searchable audit trail for all admin and system actions.

**What needs to be built:**

**DB schema (already partially defined in migration 001 — needs extension):**
```sql
-- Enhance existing audit_logs table:
ALTER TABLE audit_logs ADD COLUMN actor_type VARCHAR(20); -- user | system | api_key
ALTER TABLE audit_logs ADD COLUMN old_value  JSONB;
ALTER TABLE audit_logs ADD COLUMN new_value  JSONB;
ALTER TABLE audit_logs ADD COLUMN ip_address INET;
ALTER TABLE audit_logs ADD COLUMN user_agent TEXT;
ALTER TABLE audit_logs ADD COLUMN session_id VARCHAR(255);
```

**Events to track:**
| Action | Trigger |
|---|---|
| `user.login` | SSO callback or API key use |
| `user.created / updated / deleted` | Admin API |
| `team.created / updated / deleted` | Admin API |
| `apikey.created / revoked` | Admin API |
| `model.enabled / disabled / upgraded` | Runtime controller |
| `policy.created / updated` | Admin API |
| `budget.exceeded.soft / hard` | Budget enforcer |
| `endpoint.start / stop / fail` | Model controller |
| `gpu.allocated / released` | GPU inventory |

**Go service needed:**
- `internal/audit/service.go` — `Record(ctx, event AuditEvent)` method
- Async write via Redis buffered channel (non-blocking on hot path)
- Injected into all admin handlers

**API endpoints:**
```
GET /admin/v1/audit-logs
  ?actor=<user_id>
  ?action=apikey.revoked
  ?resource_type=team
  ?from=2026-01-01&to=2026-01-31
  ?page=1&per_page=50
```

**Estimated effort:** 3–4 days

---

## Phase 6 — Web Admin UI

**Goal:** Visual management panel so admins don't need curl.

**Tech stack:** Next.js 14 · TypeScript · TailwindCSS · shadcn/ui · React Query

**Folder structure:**
```
web/
├── app/
│   ├── (auth)/
│   │   └── login/page.tsx          ← SSO login + API key login
│   ├── (dashboard)/
│   │   ├── layout.tsx              ← sidebar navigation
│   │   ├── page.tsx                ← Dashboard overview
│   │   ├── organizations/
│   │   │   ├── page.tsx            ← list orgs
│   │   │   └── [id]/page.tsx       ← org detail + teams
│   │   ├── teams/
│   │   │   ├── page.tsx
│   │   │   └── [id]/
│   │   │       ├── page.tsx        ← team detail, members, policy
│   │   │       └── api-keys/page.tsx
│   │   ├── models/
│   │   │   ├── page.tsx            ← model list + health badges
│   │   │   └── [id]/page.tsx       ← endpoint pool, lifecycle, logs
│   │   ├── gpu/page.tsx            ← GPU nodes, devices, utilization
│   │   ├── usage/page.tsx          ← charts: tokens, cost, requests/day
│   │   ├── budgets/page.tsx
│   │   ├── audit-logs/page.tsx
│   │   ├── policies/
│   │   │   ├── prompt/page.tsx
│   │   │   └── gateway/page.tsx
│   │   ├── aliases/page.tsx
│   │   └── settings/
│   │       ├── sso/page.tsx
│   │       └── rbac/page.tsx
│   └── api/                        ← Next.js API routes (thin proxy to Go admin)
├── components/
│   ├── ui/                         ← shadcn/ui primitives
│   ├── layout/
│   │   ├── Sidebar.tsx
│   │   └── Header.tsx
│   ├── models/
│   │   ├── ModelCard.tsx
│   │   ├── EndpointHealthBadge.tsx
│   │   └── LifecycleStateBadge.tsx
│   ├── gpu/
│   │   ├── GPUUtilizationBar.tsx
│   │   └── GPUPackingVisualizer.tsx
│   ├── usage/
│   │   ├── TokenUsageChart.tsx
│   │   ├── CostChart.tsx
│   │   └── RequestsPerDayChart.tsx
│   ├── teams/
│   │   ├── TeamPolicyForm.tsx
│   │   └── APIKeyTable.tsx
│   └── audit/
│       └── AuditLogTable.tsx
├── lib/
│   ├── api/                        ← typed API client (fetch wrappers)
│   │   ├── client.ts               ← base client with auth header
│   │   ├── models.ts
│   │   ├── teams.ts
│   │   ├── gpu.ts
│   │   ├── usage.ts
│   │   └── audit.ts
│   ├── hooks/                      ← React Query hooks
│   │   ├── useModels.ts
│   │   ├── useGPU.ts
│   │   └── useUsage.ts
│   └── types/                      ← TypeScript types matching Go structs
├── public/
├── next.config.ts
├── tailwind.config.ts
└── package.json
```

**Key pages:**

| Page | Key Components | Data Source |
|---|---|---|
| Dashboard | request rate chart, GPU util, active models, top teams | `/admin/v1/usage/...` + Prometheus |
| Models | health badge per endpoint, start/stop buttons, lifecycle state | `/admin/v1/models` |
| GPU Inventory | device list, VRAM bars, allocation table, packing simulator | `/admin/v1/gpu/nodes` |
| Teams | policy form, member list, API key table | `/admin/v1/orgs/:id/teams` |
| Usage Analytics | token charts per team/model, cost per day/month | `/admin/v1/usage/teams/:id` |
| Audit Logs | filterable table, actor + action + before/after | `/admin/v1/audit-logs` |
| Settings → SSO | provider config form, test connection | `/admin/v1/sso/providers` |

**Estimated effort:** 10–14 days

---

## Phase 7 — Budget & Cost Management

**Goal:** Hard and soft spending limits per team and org, with automatic throttling.

**DB schema:**
```sql
CREATE TABLE budgets (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope           VARCHAR(20) NOT NULL CHECK (scope IN ('org','team')),
    scope_id        UUID NOT NULL,
    period          VARCHAR(10) NOT NULL DEFAULT 'monthly', -- monthly | daily
    limit_usd       NUMERIC(12,4) NOT NULL,
    soft_limit_pct  INTEGER NOT NULL DEFAULT 80,  -- alert at 80%
    hard_limit_pct  INTEGER NOT NULL DEFAULT 100, -- throttle at 100%
    action_on_hard  VARCHAR(20) NOT NULL DEFAULT 'throttle', -- throttle | block
    notify_emails   JSONB NOT NULL DEFAULT '[]',
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(scope, scope_id, period)
);
```

**Enforcement flow:**
```
Every request (in policy engine hot path):
  1. Get current period spend → Redis counter (refreshed every 5min from DB)
  2. If spend >= soft_limit: add X-Nexus-Budget-Warning header
  3. If spend >= hard_limit AND action = throttle: inject 5s delay
  4. If spend >= hard_limit AND action = block: return HTTP 429 with budget_exceeded
  5. Background job checks budgets every hour, sends email alerts at thresholds
```

**Go service needed:**
- `internal/budget/enforcer.go`
- `internal/budget/notifier.go` (email via SMTP)
- Budget check integrated into `internal/policy/engine.go`

**API endpoints:**
```
POST   /admin/v1/budgets
GET    /admin/v1/budgets/:scope/:scope_id
PUT    /admin/v1/budgets/:id
DELETE /admin/v1/budgets/:id
GET    /admin/v1/budgets/:id/status   ← current spend vs limit
```

**Estimated effort:** 3–4 days

---

## Phase 8 — ClickHouse Analytics Layer

**Goal:** High-performance analytics for token usage, cost, latency, TTFT — much faster than PostgreSQL for time-series queries.

**Architecture:**
```
nexus-gateway
    │  usage event (async)
    ▼
Redis Stream (nexus:usage:events)
    │
    ▼
Usage Consumer (Go goroutine)
    ├── PostgreSQL  ← operational data (quotas, billing)
    └── ClickHouse  ← analytics (charts, dashboards, reports)
```

**ClickHouse schema:**
```sql
CREATE TABLE usage_events (
    event_id          UUID,
    org_id            UUID,
    team_id           UUID,
    model_name        LowCardinality(String),
    endpoint_id       UUID,
    request_id        String,
    prompt_tokens     UInt32,
    completion_tokens UInt32,
    total_tokens      UInt32,
    latency_ms        UInt32,
    ttft_ms           UInt32,
    queue_wait_ms     UInt32,
    status            Enum8('success'=1,'error'=2,'timeout'=3,'rejected'=4),
    error_code        Nullable(String),
    cost_usd          Float64,
    gpu_cache_util    Float32,
    created_at        DateTime64(3)
) ENGINE = MergeTree()
PARTITION BY toYYYYMM(created_at)
ORDER BY (org_id, team_id, created_at)
TTL created_at + INTERVAL 365 DAY;

-- Pre-aggregated hourly view
CREATE MATERIALIZED VIEW usage_hourly
ENGINE = SummingMergeTree()
ORDER BY (team_id, model_name, hour)
AS SELECT
    team_id, model_name,
    toStartOfHour(created_at) AS hour,
    sum(total_tokens) AS total_tokens,
    sum(cost_usd) AS cost_usd,
    count() AS requests,
    avg(latency_ms) AS avg_latency_ms,
    avg(ttft_ms) AS avg_ttft_ms
FROM usage_events GROUP BY team_id, model_name, hour;
```

**Go changes needed:**
- Add ClickHouse client to `internal/usage/tracker.go`
- Dual-write: PostgreSQL for quotas, ClickHouse for analytics
- Grafana datasource: ClickHouse plugin

**Docker Compose addition:**
```yaml
clickhouse:
  image: clickhouse/clickhouse-server:24.3
  ports: ["8123:8123", "9000:9000"]
  volumes:
    - clickhouse_data:/var/lib/clickhouse
    - ./migrations/clickhouse/:/docker-entrypoint-initdb.d/
```

**Estimated effort:** 4–5 days

---

## Phase 9 — MCP / Tool Gateway

**Goal:** Controlled access to external tools (GitHub, Jira, etc.) through NexusLLM's policy engine — teams get tool access based on their permissions, with full audit trail.

**Architecture:**
```
Team App ──► POST /v1/tools/call
              │
              ▼
       Tool Gateway Middleware
              │
         ┌────▼──────┐
         │  Policy   │  check team tool permissions
         │  Check    │
         └────┬──────┘
              │ allowed
              ▼
       Tool Registry → route to correct MCP server / API adapter
              │
              ▼
    GitHub API / Jira API / Internal API / MCP Server
              │
              ▼
       Audit Log (tool call recorded)
              │
              ▼
         Response
```

**DB schema:**
```sql
CREATE TABLE tool_definitions (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name         VARCHAR(255) NOT NULL UNIQUE,  -- "github.read_repo"
    display_name VARCHAR(255),
    tool_type    VARCHAR(50),   -- github | jira | confluence | mcp | internal_api
    endpoint_url VARCHAR(512),
    auth_type    VARCHAR(50),   -- none | bearer | basic | oauth2
    auth_secret  TEXT,          -- encrypted
    schema_json  JSONB,         -- JSON Schema for call parameters
    enabled      BOOLEAN DEFAULT TRUE,
    created_at   TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE tool_permissions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope       VARCHAR(20) CHECK (scope IN ('org','team','api_key')),
    scope_id    UUID NOT NULL,
    tool_id     UUID REFERENCES tool_definitions(id) ON DELETE CASCADE,
    allow       BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(scope, scope_id, tool_id)
);
```

**API endpoints:**
```
GET    /v1/tools                              ← list tools allowed for calling key
POST   /v1/tools/call                         ← execute a tool call

POST   /admin/v1/tools                        ← register tool
GET    /admin/v1/tools
POST   /admin/v1/tools/:id/permissions        ← grant to team/org
DELETE /admin/v1/tools/:id/permissions/:perm_id
```

**Estimated effort:** 6–8 days

---

## Implementation Order & Timeline

```
PHASE 4 — Identity & Access (Week 1–2)
  ├── 4.1 RBAC schema + middleware          3–5 days
  └── 4.2 SSO / OIDC integration            5–7 days

PHASE 5 — Audit Logging (Week 2–3)
  └── Audit service + API + migration       3–4 days

PHASE 6 — Web Admin UI (Week 3–5)
  └── Next.js app, all pages, API client    10–14 days

PHASE 7 — Budget & Cost (Week 5–6)
  └── Budget enforcer + notifier            3–4 days

PHASE 8 — ClickHouse Analytics (Week 6–7)
  └── Dual-write pipeline + schema          4–5 days

PHASE 9 — MCP Tool Gateway (Week 7–9)
  └── Tool registry + policy + adapters     6–8 days

─────────────────────────────────────────
TOTAL: ~9 weeks for full enterprise platform
```

---

## Production Readiness Gaps (Fix Before Any Phase)

These are existing risks that should be addressed in parallel with new features:

| Risk | Severity | Fix |
|---|---|---|
| JWT secret `dev-secret` in default config | 🔴 Critical | Enforce non-default via startup check |
| No TLS on Admin API | 🔴 Critical | Add TLS termination (nginx reverse proxy) |
| No rate limit on Admin API | 🔴 Critical | Add IP-based rate limit middleware |
| PostgreSQL no SSL in compose | 🟠 High | Add `sslmode=require` in prod config |
| Redis no password in dev compose | 🟠 High | Add `requirepass` in prod config |
| API key hashes exposed in logs | 🟠 High | Audit log output for key_hash leaks |
| No request body size limit on gateway | 🟡 Medium | Add `MaxBytesReader` on all handlers |
| Usage consumer single goroutine | 🟡 Medium | Add consumer group with 2–3 workers |
| No health check on Admin API | 🟡 Medium | Add `/healthz` with DB ping |
| Audit log table not write-protected | 🟡 Medium | Use append-only DB role for audit writes |
| No backup config for PostgreSQL | 🟡 Medium | Add `pg_dump` cron in docker-compose |
| Prometheus metrics port public | 🟡 Medium | Bind metrics to `127.0.0.1` only |
| No CORS config on gateway | 🟡 Medium | Add explicit CORS middleware |

---

## Full Feature Checklist After All Phases

```
Core Platform (done)
  [x] Multi-tenant (orgs/teams)
  [x] API key + JWT auth
  [x] OpenAI-compatible inference
  [x] Model lifecycle management
  [x] GPU inventory + packing
  [x] Prompt policy engine
  [x] Gateway policy engine
  [x] Usage tracking
  [x] Model aliases
  [x] Scheduler + priority queue
  [x] Runtime controller (start/stop/upgrade)
  [x] Multi-backend (vLLM/Ollama/TGI)
  [x] Prometheus + Grafana

Phase 4 — Identity
  [ ] RBAC (roles + permissions)
  [ ] SSO / OIDC (Keycloak, Azure AD, Google)

Phase 5 — Audit
  [ ] Immutable audit log
  [ ] Before/after state tracking
  [ ] Searchable audit API

Phase 6 — Web UI
  [ ] Dashboard
  [ ] Team/org management
  [ ] API key management
  [ ] Model health monitor
  [ ] GPU utilization view
  [ ] Usage analytics charts
  [ ] Budget management
  [ ] Audit log viewer
  [ ] Policy management
  [ ] SSO config

Phase 7 — Budget
  [ ] Per-team monthly budget
  [ ] Per-org monthly budget
  [ ] Soft limit alerts (email)
  [ ] Hard limit enforcement (throttle/block)

Phase 8 — ClickHouse
  [ ] Dual-write pipeline
  [ ] ClickHouse schema + materialized views
  [ ] Grafana ClickHouse dashboards

Phase 9 — Tool Gateway
  [ ] Tool registry
  [ ] Tool permission policies
  [ ] GitHub / Jira / Confluence adapters
  [ ] MCP server proxy
  [ ] Tool audit log
```
