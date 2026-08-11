-- tpm_correction.lua
-- Post-response correction of admission counters.
-- Adjusts TPM counters for actual vs estimated input.
-- Records actual output in advisory output-TPM counters.
-- Releases unused daily/monthly quota reservation.
-- Increments org monthly counter with actual total.
-- Removes request from inflight ZSET.
-- Deletes admission token.
--
-- IMPORTANT: Keys are built by the caller using the ADMISSION date (started_at::date),
-- NOT the completion date. This prevents late-completing requests from applying
-- corrections to the wrong day's quota key.
--
-- Returns: {'corrected', input_delta_string, actual_output_string}

redis.replicate_commands()

local request_id      = ARGV[1]
local est_input       = tonumber(ARGV[2])
local actual_input    = tonumber(ARGV[3])
local actual_output   = tonumber(ARGV[4])
local max_reservation = tonumber(ARGV[5])
-- ARGV[6] reserved for admission_date (used by caller to build keys)

local model_itpm_key   = KEYS[1]
local project_itpm_key = KEYS[2]
local team_itpm_key    = KEYS[3]
local model_otpm_key   = KEYS[4]   -- advisory output TPM
local project_otpm_key = KEYS[5]
local team_otpm_key    = KEYS[6]
local daily_key        = KEYS[7]   -- key already encodes admission_date
local monthly_key      = KEYS[8]
local org_monthly_key  = KEYS[9]
local inflight_key     = KEYS[10]
local token_key        = KEYS[11]

local tpm_ttl = 70

-- ── Adjust input TPM (actual_input vs estimated_input) ───────────────────────
local input_delta = actual_input - est_input

local function safe_adjust(key, delta, ttl)
    if delta > 0 then
        redis.call('INCRBY', key, delta)
        if ttl > 0 then redis.call('EXPIRE', key, ttl) end
    elseif delta < 0 then
        local v = tonumber(redis.call('DECRBY', key, -delta))
        if v < 0 then redis.call('SET', key, '0') end
    end
    -- delta == 0: no-op
end

safe_adjust(model_itpm_key,   input_delta, tpm_ttl)
safe_adjust(project_itpm_key, input_delta, tpm_ttl)
safe_adjust(team_itpm_key,    input_delta, tpm_ttl)

-- ── Record actual output in advisory output-TPM counters ─────────────────────
if actual_output > 0 then
    redis.call('INCRBY', model_otpm_key,   actual_output)
    redis.call('INCRBY', project_otpm_key, actual_output)
    redis.call('INCRBY', team_otpm_key,    actual_output)
    redis.call('EXPIRE', model_otpm_key,   tpm_ttl)
    redis.call('EXPIRE', project_otpm_key, tpm_ttl)
    redis.call('EXPIRE', team_otpm_key,    tpm_ttl)
end

-- ── Daily/monthly quota: release unused reservation ──────────────────────────
-- actual_total may be less than max_reservation (expected case).
-- quota_delta is negative → releases capacity for new requests.
-- quota_delta is positive → request overran max_reservation (provider bug).
local actual_total = actual_input + actual_output
local quota_delta  = actual_total - max_reservation

safe_adjust(daily_key,   quota_delta, 0)   -- EXPIREAT already set at admission
safe_adjust(monthly_key, quota_delta, 0)

-- ── Org monthly: increment with actual total ──────────────────────────────────
-- This is the only place org_monthly is incremented.
-- Read-only during admission (guards against org budget exhaustion).
redis.call('INCRBY', org_monthly_key, actual_total)
redis.call('EXPIRE',  org_monthly_key, 33 * 86400)

-- ── Concurrency: release this request ────────────────────────────────────────
redis.call('ZREM', inflight_key, request_id)

-- ── Delete admission token ────────────────────────────────────────────────────
redis.call('DEL', token_key)

return {'corrected', tostring(input_delta), tostring(actual_output)}
