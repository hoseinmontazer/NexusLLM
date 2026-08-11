-- admission_rollback.lua
-- Reverses admission counters when billing authorization fails.
-- OWNERSHIP CHECK: verifies the admission_token before touching any counter.
-- If the token does not match (expired or wrong request), this is a no-op.
--
-- Returns:
--   {'rolled_back', request_id}             -- counters reversed
--   {'noop', 'token_mismatch_or_expired'}   -- token check failed, nothing done

redis.replicate_commands()

local request_id      = ARGV[1]
local admission_token = ARGV[2]   -- must match stored token
local est_input       = tonumber(ARGV[3])
local max_reservation = tonumber(ARGV[4])

local model_rpm_key    = KEYS[1]
local model_itpm_key   = KEYS[2]
local project_rpm_key  = KEYS[3]
local project_itpm_key = KEYS[4]
local inflight_key     = KEYS[5]
local team_rpm_key     = KEYS[6]
local team_itpm_key    = KEYS[7]
local daily_key        = KEYS[8]
local monthly_key      = KEYS[9]
local token_key        = KEYS[10]

-- ── Ownership check ───────────────────────────────────────────────────────────
-- A late rollback from a crashed/stale gateway instance must not corrupt
-- counters that now belong to a different request.
local stored_token = redis.call('GET', token_key)
if not stored_token or stored_token ~= admission_token then
    return {'noop', 'token_mismatch_or_expired'}
end

-- ── Safe decrement (clamp at 0) ───────────────────────────────────────────────
local function safe_decr(key, amount)
    local v = tonumber(redis.call('DECRBY', key, amount))
    if v < 0 then
        redis.call('SET', key, '0')
    end
end

-- RPM: remove specific request_id member (idempotent ZREM)
redis.call('ZREM', model_rpm_key,   request_id)
redis.call('ZREM', project_rpm_key, request_id)
redis.call('ZREM', team_rpm_key,    request_id)
redis.call('ZREM', inflight_key,    request_id)

-- TPM: decrement estimated input reservation
safe_decr(model_itpm_key,   est_input)
safe_decr(project_itpm_key, est_input)
safe_decr(team_itpm_key,    est_input)

-- Quota: release max_reservation
safe_decr(daily_key,   max_reservation)
safe_decr(monthly_key, max_reservation)

-- Delete admission token (prevents double-rollback)
redis.call('DEL', token_key)

return {'rolled_back', request_id}
