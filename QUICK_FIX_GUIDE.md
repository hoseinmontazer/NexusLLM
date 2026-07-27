# Quick Fix: Whisper "Invalid model size" Error

## The Problem You're Seeing

```
ERROR: ValueError: Invalid model size 'whisper', expected one of: tiny.en, tiny, base.en, 
base, small.en, small, medium.en, medium, large-v1, large-v2, large-v3, large...
```

## The Solution (3 steps)

### Step 1: Find Your Model ID

```bash
psql -d nexusllm -c "SELECT id, name FROM models WHERE name = 'whisper';"
```

Or if you named it something else:
```bash
psql -d nexusllm -c "SELECT id, name FROM models WHERE enabled = TRUE;"
```

### Step 2: Set the Upstream Model Name

Replace `{model_id}` with the UUID from Step 1, and choose the correct model size for your faster-whisper-server:

**Option A: Using the Admin API**
```bash
curl -X PUT http://localhost:8081/admin/v1/models/{model_id}/upstream \
  -H "Content-Type: application/json" \
  -d '{"upstream_model_name": "large-v3"}'
```

**Option B: Using SQL**
```bash
psql -d nexusllm -c "UPDATE model_endpoints 
SET upstream_model_name = 'large-v3', updated_at = NOW()
WHERE model_id = '{model_id}';"
```

### Step 3: Wait & Test (10 seconds)

The gateway auto-reloads every 10 seconds. Then test:

```bash
curl -X POST http://localhost:8080/v1/audio/transcriptions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -F "model=whisper" \
  -F "file=@test_audio.mp3"
```

## Which Model Size to Use?

Check what model your faster-whisper-server is running:

```bash
# SSH into the server or check docker logs
docker logs faster-whisper-container 2>&1 | grep -i "model"
```

Common choices:
- `base` - Fast, lower quality (~145 MB)
- `medium` - Balanced (~1.5 GB)  
- `large-v3` - Best quality (~3 GB)

## Verify It's Working

```sql
-- Check the configuration
SELECT 
    m.name,
    me.host,
    me.port,
    me.upstream_model_name,
    me.health_status
FROM model_endpoints me
JOIN models m ON m.id = me.model_id
WHERE m.name = 'whisper';
```

You should see something like:
```
 name    | host           | port | upstream_model_name | health_status
---------+----------------+------+---------------------+--------------
 whisper | 192.168.0.200 | 8000 | large-v3           | healthy
```

## What This Fix Does

**Before:**
```
Client → Gateway → faster-whisper (receives "whisper") → ERROR
```

**After:**
```
Client sends "whisper" → Gateway translates to "large-v3" → faster-whisper → SUCCESS
```

The gateway now automatically rewrites the model field in multipart form requests before forwarding them.

## Troubleshooting

### Still getting the error after 10 seconds?

Manually reload the gateway:
```bash
# Restart the gateway service
docker restart nexus-gateway
# OR if running directly
systemctl restart nexus-gateway
```

### Want to verify the gateway has the new code?

```bash
# Check if the column exists
psql -d nexusllm -c "\d model_endpoints" | grep upstream_model_name

# Should show:
# upstream_model_name | character varying(512) | not null | default ''::character varying
```

### Multiple Whisper models?

If you have different models for different sizes:

```bash
# Create separate models
curl -X POST http://localhost:8081/admin/v1/models \
  -H "Content-Type: application/json" \
  -d '{
    "name": "whisper-base",
    "display_name": "Whisper Base",
    "backend_type": "cpu_native",
    "service_type": "transcription",
    "capabilities": ["transcription"]
  }'

# Then set upstream name
curl -X PUT http://localhost:8081/admin/v1/models/{model_id}/upstream \
  -H "Content-Type: application/json" \
  -d '{"upstream_model_name": "base"}'
```

## Need More Help?

See the full documentation:
- `/docs/24-upstream-model-name.md` - Complete feature guide
- `/examples/configure-whisper-upstream.sql` - More SQL examples
- `/UPSTREAM_MODEL_NAME_FIX.md` - Technical implementation details

## Available faster-whisper Model Sizes

tiny, tiny.en, base, base.en, small, small.en, medium, medium.en, large-v1, large-v2, **large-v3**, large, distil-large-v2, distil-large-v3, distil-medium.en, distil-small.en

Choose the one that matches your faster-whisper-server deployment.
