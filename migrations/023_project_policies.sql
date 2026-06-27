-- NexusLLM Migration 023 — Project-level Policies, Quotas and Usage Rollups
--
-- Projects become the primary quota/billing/rate-limit unit.
-- Teams remain organizational entities and their limits become aggregate caps.
--
-- Architecture after this migration:
--   Request arrives → resolve project (from API key or X-Nexus-Project header)
--   → check project RPM / TPM / concurrency first
--   → if project limit hit → reject/queue (project-level response)
--   → also enforce team aggregate caps (team still owns the ceiling)
--   → usage is rolled up per project
--
-- Backward-compatible: all columns have sensible defaults.
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. PROJECT POLICIES
-- Per-project rate limits and quota budgets.
-- 0 = unlimited for that dimension.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS project_policies (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id          UUID        NOT NULL UNIQUE REFERENCES projects(id) ON DELETE CASCADE,
    -- Rate limits
    rpm                 INTEGER     NOT NULL DEFAULT 0   CHECK (rpm >= 0),       -- requests/minute; 0 = unlimited
    tpm                 INTEGER     NOT NULL DEFAULT 0   CHECK (tpm >= 0),       -- tokens/minute; 0 = unlimited
    max_concurrent      INTEGER     NOT NULL DEFAULT 0   CHECK (max_concurrent >= 0),
    max_context_tokens  INTEGER     NOT NULL DEFAULT 0   CHECK (max_context_tokens >= 0),
    -- Token budgets
    daily_token_budget  BIGINT      NOT NULL DEFAULT 0   CHECK (daily_token_budget >= 0),   -- 0 = unlimited
    monthly_token_budget BIGINT     NOT NULL DEFAULT 0   CHECK (monthly_token_budget >= 0), -- 0 = unlimited
    -- Cost budgets (USD)
    daily_cost_budget   NUMERIC(12,6) NOT NULL DEFAULT 0  CHECK (daily_cost_budget >= 0),   -- 0 = unlimited
    monthly_cost_budget NUMERIC(12,6) NOT NULL DEFAULT 0  CHECK (monthly_cost_budget >= 0), -- 0 = unlimited
    -- Metadata
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

DROP TRIGGER IF EXISTS set_project_policies_updated_at ON project_policies;
CREATE TRIGGER set_project_policies_updated_at
    BEFORE UPDATE ON project_policies
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- Seed default (all-unlimited) policies for existing projects
INSERT INTO project_policies (project_id)
SELECT id FROM projects
WHERE id NOT IN (SELECT project_id FROM project_policies)
ON CONFLICT DO NOTHING;

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. PROJECT USAGE ROLLUP TABLES
-- Project-scoped daily and monthly aggregations.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS project_usage_daily (
    project_id        UUID        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    model_name        TEXT        NOT NULL DEFAULT '',
    day               DATE        NOT NULL,
    request_count     BIGINT      NOT NULL DEFAULT 0,
    error_count       BIGINT      NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT      NOT NULL DEFAULT 0,
    completion_tokens BIGINT      NOT NULL DEFAULT 0,
    total_tokens      BIGINT      NOT NULL DEFAULT 0,
    cost_usd          NUMERIC(14,8) NOT NULL DEFAULT 0,
    avg_latency_ms    NUMERIC(10,2) NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, model_name, day)
);

CREATE INDEX IF NOT EXISTS idx_project_usage_daily_project_day
    ON project_usage_daily(project_id, day DESC);
CREATE INDEX IF NOT EXISTS idx_project_usage_daily_day
    ON project_usage_daily(day DESC);

CREATE TABLE IF NOT EXISTS project_usage_monthly (
    project_id        UUID        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    model_name        TEXT        NOT NULL DEFAULT '',
    month             DATE        NOT NULL,  -- first day of month
    request_count     BIGINT      NOT NULL DEFAULT 0,
    error_count       BIGINT      NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT      NOT NULL DEFAULT 0,
    completion_tokens BIGINT      NOT NULL DEFAULT 0,
    total_tokens      BIGINT      NOT NULL DEFAULT 0,
    cost_usd          NUMERIC(14,8) NOT NULL DEFAULT 0,
    avg_latency_ms    NUMERIC(10,2) NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, model_name, month)
);

CREATE INDEX IF NOT EXISTS idx_project_usage_monthly_project
    ON project_usage_monthly(project_id, month DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. DAILY BUDGET CONSUMPTION VIEW
-- Fast view for checking remaining budget without scanning usage_events.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE VIEW project_budget_status AS
SELECT
    pp.project_id,
    p.name                                              AS project_name,
    pp.daily_token_budget,
    pp.monthly_token_budget,
    pp.daily_cost_budget,
    pp.monthly_cost_budget,
    -- Today's usage
    COALESCE(tod.total_tokens, 0)                       AS tokens_today,
    COALESCE(tod.cost_usd, 0)                           AS cost_today,
    COALESCE(tod.request_count, 0)                      AS requests_today,
    -- This month's usage
    COALESCE(mon.total_tokens, 0)                       AS tokens_this_month,
    COALESCE(mon.cost_usd, 0)                           AS cost_this_month,
    COALESCE(mon.request_count, 0)                      AS requests_this_month,
    -- Remaining budgets (NULL = unlimited)
    CASE WHEN pp.daily_token_budget  > 0
         THEN pp.daily_token_budget  - COALESCE(tod.total_tokens, 0) END AS daily_tokens_remaining,
    CASE WHEN pp.monthly_token_budget > 0
         THEN pp.monthly_token_budget - COALESCE(mon.total_tokens, 0) END AS monthly_tokens_remaining,
    CASE WHEN pp.daily_cost_budget   > 0
         THEN pp.daily_cost_budget   - COALESCE(tod.cost_usd, 0) END     AS daily_cost_remaining,
    CASE WHEN pp.monthly_cost_budget  > 0
         THEN pp.monthly_cost_budget  - COALESCE(mon.cost_usd, 0) END    AS monthly_cost_remaining
FROM project_policies pp
JOIN projects p ON p.id = pp.project_id
LEFT JOIN LATERAL (
    SELECT SUM(total_tokens) AS total_tokens, SUM(cost_usd) AS cost_usd, COUNT(*) AS request_count
    FROM project_usage_daily
    WHERE project_id = pp.project_id AND day = CURRENT_DATE
) tod ON TRUE
LEFT JOIN LATERAL (
    SELECT SUM(total_tokens) AS total_tokens, SUM(cost_usd) AS cost_usd, COUNT(*) AS request_count
    FROM project_usage_monthly
    WHERE project_id = pp.project_id AND month = date_trunc('month', CURRENT_DATE)::date
) mon ON TRUE;

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. BACK-FILL TODAY'S USAGE for existing projects
-- ─────────────────────────────────────────────────────────────────────────────
INSERT INTO project_usage_daily (project_id, model_name, day,
    request_count, error_count, prompt_tokens, completion_tokens,
    total_tokens, cost_usd, avg_latency_ms)
SELECT
    project_id,
    COALESCE(model_name, '') AS model_name,
    created_at::date          AS day,
    COUNT(*)                  AS request_count,
    COUNT(*) FILTER (WHERE status != 'success') AS error_count,
    COALESCE(SUM(prompt_tokens), 0),
    COALESCE(SUM(completion_tokens), 0),
    COALESCE(SUM(total_tokens), 0),
    COALESCE(SUM(cost_usd), 0),
    COALESCE(AVG(latency_ms), 0)
FROM usage_events
WHERE project_id IS NOT NULL
  AND created_at::date >= CURRENT_DATE - INTERVAL '30 days'
GROUP BY project_id, COALESCE(model_name,''), created_at::date
ON CONFLICT (project_id, model_name, day) DO UPDATE SET
    request_count     = EXCLUDED.request_count,
    error_count       = EXCLUDED.error_count,
    prompt_tokens     = EXCLUDED.prompt_tokens,
    completion_tokens = EXCLUDED.completion_tokens,
    total_tokens      = EXCLUDED.total_tokens,
    cost_usd          = EXCLUDED.cost_usd,
    avg_latency_ms    = EXCLUDED.avg_latency_ms;

COMMIT;
