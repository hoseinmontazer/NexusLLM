# Troubleshooting

---

## Gateway / inference issues

### `401 — Token is invalid or expired`

**Cause:** API key is wrong, expired, or revoked.

**Fix:**
1. Create a new key: web UI → Teams → your team → API Keys → Create Key
2. Make sure you're copying the full key including `nxs_` prefix
3. Check the key isn't expired: `curl http://localhost:8081/admin/v1/teams/TEAM_ID/api-keys`

---

### `403 — model_not_allowed`

**Cause:** The team doesn't have permission for this model.

**Fix:**
```bash
curl -X POST http://localhost:8081/admin/v1/teams/TEAM_ID/models \
  -H "Content-Type: application/json" \
  -d '{"model_name": "gemma2:2b"}'
```

Or: web UI → Teams → your team → click **Add Model** → select the model.

---

### `429 — rate_limit_exceeded`

**Cause:** Team hit its RPM limit.

**Fix:** Increase the limit:
```bash
curl -X PUT http://localhost:8081/admin/v1/teams/TEAM_ID/policy \
  -d '{"rpm": 1000}'
```

Or wait 60 seconds for the sliding window to clear.

---

### `429 — daily_quota_exceeded`

**Cause:** Team used all its daily tokens.

**Fix:** Increase the TPD limit, or wait until midnight UTC for the counter to reset.

```bash
curl -X PUT http://localhost:8081/admin/v1/teams/TEAM_ID/policy \
  -d '{"tpd": 50000000}'
```

---

### `503 — no_healthy_endpoint`

**Cause:** All endpoints for this model are down or unknown.

**Fix:**
1. Check model health: `curl http://localhost:8081/admin/v1/models/MODEL_ID/health`
2. Is the backend running? For Ollama: `ollama list && curl http://localhost:11434/`
3. Reset health state: `curl -X POST http://localhost:8081/admin/v1/models/MODEL_ID/reset-health`
4. Wait 5–10 seconds for the watcher to re-check

---

### Model shows `health: unknown` in the UI

**Cause:** Watcher hasn't checked this endpoint yet, or the gateway just restarted.

**Fix:** Wait 5–10 seconds. The watcher runs every 5 seconds. If it stays `unknown`, call reset-health.

---

### Model shows `health: failed` after being healthy

**Cause:** 3+ consecutive health check failures (circuit breaker triggered).

**Fix:**
1. Check the backend is still running
2. Call reset-health to clear the circuit breaker
3. The watcher will re-check within 5 seconds

---

## Model deployment issues

### vLLM container is `Created` but not `Up`

**Cause:** No NVIDIA GPU or `nvidia-container-toolkit` not installed.

**Error:** `could not select device driver "" with capabilities: [[gpu]]`

**Fix for dev machines:** Use Ollama instead. vLLM requires a physical NVIDIA GPU.

**Fix for GPU servers:**
```bash
# Install nvidia-container-toolkit
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | sudo gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
# ... follow NVIDIA installation guide for your distro

# Test GPU access
docker run --rm --gpus all nvidia/cuda:12.0-base nvidia-smi
```

---

### vLLM container starts but model shows `loading` for a long time

**Cause:** Model weights are being downloaded from HuggingFace (can take 10–60 minutes for large models).

**Check container logs:**
```bash
curl "http://localhost:8081/admin/v1/models/MODEL_ID/logs?endpoint_id=EP_ID"
# or directly:
docker logs nexus-MODELNAME
```

Look for `Loading model weights...` or download progress.

**Fix:** Set `HF_HUB_OFFLINE=1` and pre-download weights to a volume if you need faster startup.

---

### `HF_TOKEN` error — model requires authentication

**Cause:** The HuggingFace model requires accepting a license and using an auth token.

**Fix:** Go to huggingface.co, accept the model's license, then:
```json
{"hf_token": "hf_YOUR_TOKEN"}
```

---

### Ollama model not found after `import-ollama`

**Cause:** Ollama is running but the model hasn't been pulled yet.

**Fix:**
```bash
ollama pull gemma2:2b
ollama list    # verify it's there
```

Then retry import.

---

## Admin server issues

### Admin starts but crashes immediately

Check logs for the panic message. Common causes:

**Duplicate route registration:**
```
panic: handlers are already registered for path '/admin/v1/...'
```
Fix: Check `cmd/admin/main.go` for duplicate route lines.

**Database not reachable:**
```
postgres unreachable
```
Fix: `make dev-up` to start postgres + redis.

---

### Migration errors

```bash
# Run all migrations manually
make migrate

# Or run a specific migration
docker compose exec postgres psql -U nexus -d nexusllm -f /migrations/005_ai_platform.sql
```

Migrations are idempotent — safe to run multiple times.

**Column already exists error:** This is expected and harmless. Migrations use `ADD COLUMN IF NOT EXISTS`.

---

## Redis issues

### Auth key not working immediately after creation

**Cause:** If a key was recently revoked, there may be a cached version still in Redis (TTL up to 5 minutes).

**Fix:** The revoke endpoint purges the Redis cache immediately. If you're still seeing issues, restart the gateway.

---

### Rate limits seem wrong

**Check current state:**
```bash
# RPM sliding window size
redis-cli ZCARD "nexus:ratelimit:TEAM_ID:rpm"

# Today's token count
redis-cli GET "nexus:quota:TEAM_ID:daily:$(date +%Y-%m-%d)"

# Active requests
redis-cli GET "nexus:inflight:TEAM_ID"
```

**Reset a counter manually (emergency only):**
```bash
redis-cli DEL "nexus:quota:TEAM_ID:daily:2026-06-21"
redis-cli DEL "nexus:ratelimit:TEAM_ID:rpm"
```

---

## Database issues

### `pq: column "service_type" does not exist`

**Cause:** Migration 005 hasn't been run.

**Fix:**
```bash
make migrate
# or specifically:
docker compose exec postgres psql -U nexus -d nexusllm -f /migrations/005_ai_platform.sql
```

---

### `pq: column "runtime_type" does not exist`

Same fix: run migration 005.

---

## Web UI issues

### UI shows error on every page

Check if the admin API is running:
```bash
curl http://localhost:8081/healthz
```

Check if the Next.js dev server is proxying correctly — open browser DevTools → Network → look at the failing `/api/admin/*` requests.

### Import from Ollama returns empty results

```bash
# Test Ollama directly
curl http://localhost:11434/api/tags
```

If this returns an error, Ollama isn't running. Start it with `ollama serve`.

---

## Checking all service status

Quick health check script:

```bash
echo "=== Services ==="
curl -s http://localhost:8080/healthz && echo " Gateway OK"
curl -s http://localhost:8081/healthz && echo " Admin OK"

echo ""
echo "=== Models ==="
curl -s http://localhost:8081/admin/v1/models | python3 -c \
  "import sys,json; [print(f'  {m[\"name\"]:30} {m[\"backend_type\"]:15} {m[\"healthy_count\"]}/{m[\"endpoint_count\"]} healthy') for m in json.load(sys.stdin)['data']]"

echo ""
echo "=== Ollama ==="
curl -s http://localhost:11434/ && echo " Running"
ollama list 2>/dev/null || echo " Not available"
```
