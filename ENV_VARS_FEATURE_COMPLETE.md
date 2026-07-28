# Environment Variables for Lazy-Loaded Containers ✅

## What Was Added

Users can now configure environment variables that will be passed to lazy-loaded containers. This solves the port conflict issue and allows full customization of CPU-native services like faster-whisper.

## Files Modified

1. **migrations/043_model_runtime_env.sql** - New migration adding `env` JSONB column
2. **internal/runtimemgr/types.go** - Added `Env map[string]string` to ModelConfig
3. **internal/runtimemgr/activator.go** - Load env from DB and pass to task payload
4. **internal/admin/handlers/lazyruntime.go** - API support for env in SetLazyConfig/GetLazyConfig
5. **Makefile** - Added migration 043 to the MIGRATIONS list
6. **internal/nodeagent/executor.go** - Already had PORT override logic (from earlier fix)

## How to Use

### Step 1: Run the migrations

```bash
cd /home/hosein/Documents/p/llm-gateway/nexusllm

# Via docker compose
docker compose exec -T postgres psql -U nexus -d nexusllm < migrations/042_upstream_model_name.sql
docker compose exec -T postgres psql -U nexus -d nexusllm < migrations/043_model_runtime_env.sql

# Or via psql
psql "postgres://nexus:nexus@192.168.0.200:5540/nexusllm" -f migrations/042_upstream_model_name.sql
psql "postgres://nexus:nexus@192.168.0.200:5540/nexusllm" -f migrations/043_model_runtime_env.sql
```

### Step 2: Build and deploy

```bash
go build -o bin/nodeagent ./cmd/nodeagent
go build -o bin/gateway ./cmd/gateway  
go build -o bin/admin ./cmd/admin

# Restart services
docker compose restart nexus-nodeagent nexus-gateway nexus-admin
```

### Step 3: Configure your Whisper model

```bash
curl -X PUT http://192.168.0.200:8081/admin/v1/models/YOUR_MODEL_ID/lazy-config \
  -H "Content-Type: application/json" \
  -d '{
    "env": {
      "WHISPER__MODEL": "Systran/faster-whisper-large-v3",
      "WHISPER__INFERENCE_DEVICE": "cpu",
      "UVICORN_PORT": "8100"
    },
    "workload_policy": "lazy_load",
    "idle_timeout_secs": 900
  }'
```

### Step 4: Test

When a request arrives:
1. Gateway lazy-loads the container
2. Agent scans for a free port (8100, 8101, 8102...)
3. Sets `PORT=8101` (or whatever port is free)
4. Passes your env vars: `WHISPER__MODEL`, `WHISPER__INFERENCE_DEVICE`, `UVICORN_PORT`
5. Container starts with: `-e PORT=8101 -e UVICORN_PORT=8100 -e WHISPER__MODEL=...`
6. faster-whisper respects PORT and binds successfully

```bash
curl -X POST http://192.168.0.200:8880/v1/audio/transcriptions \
  -H "Authorization: Bearer nxs_..." \
  -F "model=whisper" \
  -F "upstream_model=large-v3" \
  -F "file=@audio.mp3"
```

## API Reference

### Set Lazy Config

**PUT** `/admin/v1/models/:id/lazy-config`

```json
{
  "env": {
    "KEY": "value",
    "ANOTHER_KEY": "another_value"
  },
  "gguf_path": "/models/model.gguf",
  "ctx_size": 8192,
  "idle_timeout_secs": 900,
  "execution_mode": "cpu",
  "extra_args": ["--some-flag", "value"]
}
```

### Get Lazy Config

**GET** `/admin/v1/models/:id/lazy-config`

Returns:
```json
{
  "env": {"KEY": "value"},
  "gguf_path": "/models/model.gguf",
  "ctx_size": 8192,
  ...
}
```

## How It Works

```
1. User configures env via API
         ↓
2. Stored in model_runtime_configs.env JSONB column
         ↓
3. Lazy request arrives
         ↓
4. Activator loads ModelConfig (includes Env)
         ↓
5. Activator builds StartModelPayload (includes Env)
         ↓
6. Node agent receives task
         ↓
7. Agent scans for free port
         ↓
8. Agent sets p.Env["PORT"] = "8101" (allocated port)
         ↓
9. Agent builds docker run with all -e flags
         ↓
10. Container starts with correct env vars ✅
```

## Port Handling

The agent **always overrides** `PORT` after scanning, so:
- ✅ User sets `UVICORN_PORT=8100` (preferred starting port)
- ✅ Agent finds 8100 busy, scans forward, finds 8101 free
- ✅ Agent sets `PORT=8101` (overrides any user-provided PORT)
- ✅ Container binds to 8101 successfully

## Example: faster-whisper-server

```bash
# Configure the model
curl -X PUT http://localhost:8081/admin/v1/models/WHISPER_MODEL_ID/lazy-config \
  -H "Content-Type: application/json" \
  -d '{
    "env": {
      "WHISPER__MODEL": "Systran/faster-whisper-large-v3",
      "WHISPER__INFERENCE_DEVICE": "cpu",
      "WHISPER__COMPUTE_TYPE": "int8",
      "UVICORN_PORT": "8000"
    },
    "execution_mode": "cpu",
    "workload_policy": "lazy_load",
    "idle_timeout_secs": 900
  }'
```

Now the container will start with those env vars, and port conflicts are automatically resolved!

## Migration Details

**042_upstream_model_name.sql:**
- Adds `upstream_model_name` column for dynamic model name translation

**043_model_runtime_env.sql:**
- Adds `env` JSONB column for environment variables
- Default value: `{}`
- Constraint: must be a JSON object

## Benefits

1. ✅ **No more port conflicts** - agent scans and allocates free ports
2. ✅ **Per-model customization** - each model can have different env vars
3. ✅ **Dynamic model selection** - users choose model size via `upstream_model` in requests
4. ✅ **Clean separation** - env config stored in DB, not hardcoded
5. ✅ **Backward compatible** - existing models work unchanged

## Troubleshooting

### Container still fails with port conflict

- Check that you rebuilt and restarted the nodeagent
- Verify the env vars are in the DB: `SELECT env FROM model_runtime_configs WHERE model_id = '...'`
- Check agent logs for "port scanning" messages

### Env vars not being applied

```bash
# Check the DB
psql -c "SELECT env FROM model_runtime_configs WHERE model_id = 'YOUR_ID';"

# Should show: {"WHISPER__MODEL":"...","UVICORN_PORT":"8100"}
```

### Want to clear env vars

```bash
curl -X PUT http://localhost:8081/admin/v1/models/MODEL_ID/lazy-config \
  -d '{"env": {}}'
```

## Related Features

- **upstream_model in requests** - Dynamic model size selection per-request
- **upstream_model_name in DB** - Default model name mapping
- **Port scanning** - Automatic free port allocation
- **Workload policies** - lazy_load vs always_on

All three features work together to provide a complete, flexible lazy-loading system!
