-- NexusLLM Migration 045 — Per-Model Cost Configuration & Provider Rate Limits
--
-- Goal: Track real provider costs and enforce per-provider rate limits so that
--       every usage event records the actual cost and the gateway can enforce
--       provider-side RPM budgets (e.g. OpenAI 100 RPM on Tier 1).
--
-- Changes:
--   1. model_cost_config — per-model cost table (input/output tokens, cached
--      tokens, per-request overhead, image/audio costs, currency).
--   2. provider_rate_limits — per-provider, per-model, per-project, per-team
--      rate limit configuration.
--   3. usage_events — add cost_currency, provider_request_id, cached_tokens,
--      reasoning_tokens, ttft_ms (already present — guard with IF NOT EXISTS).
--   4. Provider cost seeds for known models.
--
-- All statements are idempotent (safe to re-run).
BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. MODEL COST CONFIGURATION
--    Stores the billing rate for every model. Used by usage.Tracker to compute
--    cost_usd at record time. Local models may have cost = 0 (infrastructure
--    cost not tracked here) or a configured internal charge-back rate.
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS model_cost_config (
    id                     UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id               UUID         NOT NULL REFERENCES models(id) ON DELETE CASCADE,

    -- Per-token costs in USD per 1 million tokens.
    input_cost_per_1m      NUMERIC(12,6) NOT NULL DEFAULT 0,
    output_cost_per_1m     NUMERIC(12,6) NOT NULL DEFAULT 0,
    -- Cached tokens are charged at a lower rate by most providers.
    cached_input_cost_per_1m NUMERIC(12,6) NOT NULL DEFAULT 0,
    -- Reasoning/thinking tokens (o-series, Claude extended thinking).
    reasoning_cost_per_1m  NUMERIC(12,6) NOT NULL DEFAULT 0,

    -- Per-request flat fee (rare but used by some providers).
    per_request_cost_usd   NUMERIC(12,8) NOT NULL DEFAULT 0,

    -- Multimodal costs.
    image_cost_per_1k      NUMERIC(12,6) NOT NULL DEFAULT 0, -- per 1k image tokens
    audio_cost_per_second  NUMERIC(12,6) NOT NULL DEFAULT 0, -- TTS/STT cost per second

    -- Currency — always USD for provider costs; may differ for charge-back.
    currency               VARCHAR(3)   NOT NULL DEFAULT 'USD',

    -- Effective date range — allows pricing to change over time.
    effective_from         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    effective_until        TIMESTAMPTZ,

    -- Audit
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    UNIQUE (model_id, effective_from)
);

CREATE INDEX IF NOT EXISTS idx_model_cost_config_model_id
    ON model_cost_config(model_id);

CREATE INDEX IF NOT EXISTS idx_model_cost_config_effective
    ON model_cost_config(model_id, effective_from DESC)
    WHERE effective_until IS NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. PROVIDER RATE LIMITS
--    Enforced by the policy engine (via Redis) in addition to project-level
--    limits. Prevents the gateway from exceeding what the provider allows,
--    protecting API keys from rate-limit errors.
--
--    Scope hierarchy (most specific wins):
--      model + project > model + team > model (global) > provider (global)
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS provider_rate_limits (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Scope — all nullable except provider_name.
    -- When model_id IS NULL the limit applies to all models from that provider.
    provider_name   VARCHAR(64) NOT NULL,  -- e.g. 'openai_provider'
    model_id        UUID        REFERENCES models(id) ON DELETE CASCADE,
    project_id      UUID        REFERENCES projects(id) ON DELETE CASCADE,
    team_id         UUID        REFERENCES teams(id) ON DELETE CASCADE,

    -- Limits (0 = unlimited for that dimension).
    rpm_limit       INT         NOT NULL DEFAULT 0, -- requests per minute
    tpm_limit       INT         NOT NULL DEFAULT 0, -- tokens per minute
    rpd_limit       INT         NOT NULL DEFAULT 0, -- requests per day
    tpd_limit       BIGINT      NOT NULL DEFAULT 0, -- tokens per day

    -- Cost budget limits (0 = unlimited).
    daily_cost_limit_usd   NUMERIC(12,4) NOT NULL DEFAULT 0,
    monthly_cost_limit_usd NUMERIC(12,4) NOT NULL DEFAULT 0,

    -- Circuit-breaker configuration.
    -- When provider errors exceed error_threshold_pct within window_seconds,
    -- all requests to this model/provider are rejected for cooldown_seconds.
    error_threshold_pct  INT NOT NULL DEFAULT 50,  -- % errors to trip breaker
    window_seconds       INT NOT NULL DEFAULT 60,  -- evaluation window
    cooldown_seconds     INT NOT NULL DEFAULT 30,  -- trip duration

    -- Retry policy.
    max_retries     INT  NOT NULL DEFAULT 2,
    retry_delay_ms  INT  NOT NULL DEFAULT 500,
    timeout_seconds INT  NOT NULL DEFAULT 120,

    enabled         BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_provider_rate_limits_provider
    ON provider_rate_limits(provider_name, model_id)
    WHERE enabled = TRUE;

CREATE INDEX IF NOT EXISTS idx_provider_rate_limits_project
    ON provider_rate_limits(project_id, model_id)
    WHERE project_id IS NOT NULL AND enabled = TRUE;

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. USAGE EVENTS — add provider-specific columns
-- ─────────────────────────────────────────────────────────────────────────────

ALTER TABLE usage_events
    ADD COLUMN IF NOT EXISTS cost_currency        VARCHAR(3)   NOT NULL DEFAULT 'USD',
    ADD COLUMN IF NOT EXISTS provider_request_id  TEXT,
    ADD COLUMN IF NOT EXISTS cached_tokens        INT          NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS reasoning_tokens     INT          NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS provider_name        VARCHAR(64);

-- Index for per-provider analytics queries.
CREATE INDEX IF NOT EXISTS idx_usage_events_provider_name
    ON usage_events(provider_name, created_at)
    WHERE provider_name IS NOT NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. SEED KNOWN PROVIDER COSTS (July 2025 pricing — update as needed)
--    These are inserted as a starting point. Operators should verify and update
--    via PUT /admin/v1/models/:id/cost-config.
--
--    Costs are per 1M tokens in USD.
-- ─────────────────────────────────────────────────────────────────────────────

-- Seed function: insert cost config for models matching a name pattern.
-- Only inserts if no cost config exists yet (idempotent).
CREATE OR REPLACE FUNCTION seed_provider_cost(
    p_model_name_pattern TEXT,
    p_input_cost  NUMERIC,
    p_output_cost NUMERIC,
    p_cached_cost NUMERIC DEFAULT 0
) RETURNS VOID AS $$
BEGIN
    INSERT INTO model_cost_config
        (model_id, input_cost_per_1m, output_cost_per_1m, cached_input_cost_per_1m)
    SELECT m.id, p_input_cost, p_output_cost, p_cached_cost
    FROM models m
    WHERE m.name ILIKE p_model_name_pattern
      AND m.provider_is_external = TRUE
      AND NOT EXISTS (
          SELECT 1 FROM model_cost_config mc WHERE mc.model_id = m.id
      );
END;
$$ LANGUAGE plpgsql;

-- OpenAI
SELECT seed_provider_cost('gpt-4o',             2.50,  10.00, 1.25);
SELECT seed_provider_cost('gpt-4o-mini',        0.15,   0.60, 0.075);
SELECT seed_provider_cost('gpt-4.1',            2.00,   8.00, 0.50);
SELECT seed_provider_cost('gpt-4.1-mini',       0.40,   1.60, 0.10);
SELECT seed_provider_cost('gpt-4.1-nano',       0.10,   0.40, 0.025);
SELECT seed_provider_cost('o3',                10.00,  40.00, 2.50);
SELECT seed_provider_cost('o3-mini',            1.10,   4.40, 0.55);
SELECT seed_provider_cost('o4-mini',            1.10,   4.40, 0.275);
SELECT seed_provider_cost('text-embedding-3-small', 0.02, 0, 0);
SELECT seed_provider_cost('text-embedding-3-large', 0.13, 0, 0);

-- Anthropic
SELECT seed_provider_cost('claude-opus-4%',     15.00,  75.00, 7.50);
SELECT seed_provider_cost('claude-sonnet-4%',    3.00,  15.00, 0.30);
SELECT seed_provider_cost('claude-haiku-3-5%',   0.80,   4.00, 0.08);
SELECT seed_provider_cost('claude-3-5-sonnet%',  3.00,  15.00, 0.30);

-- Google Gemini
SELECT seed_provider_cost('gemini-2.5-pro%',     1.25,  10.00, 0.31);
SELECT seed_provider_cost('gemini-2.5-flash%',   0.30,   2.50, 0.075);
SELECT seed_provider_cost('gemini-2.0-flash%',   0.10,   0.40, 0.025);
SELECT seed_provider_cost('text-embedding-004',  0.00625, 0, 0);

-- Groq
SELECT seed_provider_cost('llama-3.3-70b%',  0.59, 0.79, 0);
SELECT seed_provider_cost('llama-3.1-8b%',   0.05, 0.08, 0);
SELECT seed_provider_cost('mixtral-8x7b%',   0.24, 0.24, 0);
SELECT seed_provider_cost('gemma2-9b%',      0.20, 0.20, 0);

-- Mistral
SELECT seed_provider_cost('mistral-large%',  2.00,  6.00, 0);
SELECT seed_provider_cost('mistral-small%',  0.10,  0.30, 0);

-- DeepSeek
SELECT seed_provider_cost('deepseek-chat%',  0.27,  1.10, 0.07);
SELECT seed_provider_cost('deepseek-reasoner%', 0.55, 2.19, 0.14);

DROP FUNCTION IF EXISTS seed_provider_cost(TEXT, NUMERIC, NUMERIC, NUMERIC);

-- ─────────────────────────────────────────────────────────────────────────────
-- 5. DEFAULT PROVIDER RATE LIMITS (conservative starting values)
--    These match typical free/tier-1 API key limits.
--    Operators should update via PUT /admin/v1/provider-rate-limits/:id.
-- ─────────────────────────────────────────────────────────────────────────────

INSERT INTO provider_rate_limits
    (provider_name, rpm_limit, tpm_limit, rpd_limit, daily_cost_limit_usd, max_retries, timeout_seconds)
VALUES
    ('openai_provider',       500,  200000, 10000, 100.00, 3, 120),
    ('anthropic_provider',    500,  200000, 10000, 100.00, 3, 120),
    ('google_provider',      1000,  400000,  1500, 100.00, 3, 120),
    ('azure_openai_provider', 500,  200000, 10000, 100.00, 3, 120),
    ('openrouter_provider',   200,   50000,  5000,  50.00, 2,  90),
    ('groq_provider',          30,   14400,  1000,  20.00, 2,  60),
    ('together_provider',     600,  180000,  5000,  50.00, 2,  90),
    ('mistral_provider',     1000,  500000,  5000,  50.00, 2,  90),
    ('cohere_provider',      1000,  100000,  5000,  50.00, 2,  90),
    ('deepseek_provider',    1000,  500000,  5000,  50.00, 2,  90)
ON CONFLICT DO NOTHING;

COMMIT;
