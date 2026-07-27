# Fix: Whisper Port 8000 Already in Use

## Problem

Your lazy-loaded Whisper model is trying to start on port 8000, which is already used by your existing faster-whisper-server at 192.168.0.200:8000.

```
ERROR: [Errno 98] error while attempting to bind on address ('0.0.0.0', 8000): 
[errno 98] address already in use
```

## Root Cause

You have two faster-whisper setups:
1. **External faster-whisper-server** running at 192.168.0.200:8000 (the one that works)
2. **NexusLLM trying to lazy-load** a new faster-whisper container on port 8000 (conflicts!)

## Solution 1: Don't Use Lazy Loading for Whisper (Recommended!)

Your external faster-whisper-server is already running. Just register it as an endpoint and disable lazy loading:

```bash
# 1. Get your model ID
psql -d nexusllm -c "SELECT id, name FROM models WHERE name = 'whisper';"

# 2. Update the endpoint to point to your EXISTING server
psql -d nexusllm <<EOF
UPDATE model_endpoints 
SET 
  host = '192.168.0.200',
  port = 8000,
  is_enabled = TRUE,
  lifecycle_state = 'active',
  health_status = 'healthy',
  upstream_model_name = 'large-v3',  -- or whichever model size you have
  updated_at = NOW()
WHERE model_id = (SELECT id FROM models WHERE name = 'whisper' LIMIT 1);
EOF

# 3. Disable lazy loading for this model
psql -d nexusllm <<EOF
UPDATE model_runtime_configs
SET workload_policy = 'always_running',
    updated_at = NOW()
WHERE model_id = (SELECT id FROM models WHERE name = 'whisper' LIMIT 1);
EOF

# Or if the row doesn't exist, create it:
psql -d nexusllm <<EOF
INSERT INTO model_runtime_configs (model_id, workload_policy)
SELECT id, 'always_running' FROM models WHERE name = 'whisper'
ON CONFLICT (model_id) DO UPDATE SET workload_policy = 'always_running';
EOF
```

Now test:
```bash
curl -X POST http://192.168.0.200:8880/v1/audio/transcriptions \
  -H "Authorization: Bearer nxs_..." \
  -F "model=whisper" \
  -F "upstream_model=large-v3" \
  -F "file=@audio.mp3"
```

## Solution 2: Use a Different Port for Lazy Loading

If you want NexusLLM to manage the faster-whisper container:

```bash
# Update to use port 8001 instead
psql -d nexusllm <<EOF
UPDATE model_endpoints 
SET port = 8001,
    host = '0.0.0.0',  -- bind to all interfaces
    upstream_model_name = 'large-v3',
    updated_at = NOW()
WHERE model_id = (SELECT id FROM models WHERE name = 'whisper' LIMIT 1);
EOF
```

## Solution 3: Use Port 0 (Auto-Assign Free Port)

Let the OS pick an available port:

```bash
psql -d nexusllm <<EOF
UPDATE model_endpoints 
SET port = 0,  -- OS will pick a free port
    host = '0.0.0.0',
    upstream_model_name = 'large-v3',
    updated_at = NOW()
WHERE model_id = (SELECT id FROM models WHERE name = 'whisper' LIMIT 1);
EOF
```

## Verify Your Setup

```sql
-- Check current config
SELECT 
    m.name,
    me.host,
    me.port,
    me.upstream_model_name,
    me.health_status,
    me.is_enabled,
    mrc.workload_policy
FROM model_endpoints me
JOIN models m ON m.id = me.model_id
LEFT JOIN model_runtime_configs mrc ON mrc.model_id = m.id
WHERE m.name = 'whisper';
```

Should show:
```
 name    | host           | port | upstream_model_name | health_status | is_enabled | workload_policy
---------+----------------+------+---------------------+---------------+------------+------------------
 whisper | 192.168.0.200  | 8000 | large-v3           | healthy       | t          | always_running
```

## Why This Happens

- **Lazy loading** (default): Gateway spawns a new container when the first request arrives
- **Always running**: Gateway uses an already-running endpoint (your case)

Since you already have faster-whisper-server running externally, you should use `workload_policy='always_running'` to tell NexusLLM not to try starting it.

## Quick Command (All-in-One Fix)

```bash
psql -d nexusllm <<'EOF'
-- Point to your existing faster-whisper-server
UPDATE model_endpoints me
SET 
  host = '192.168.0.200',
  port = 8000,
  is_enabled = TRUE,
  lifecycle_state = 'active',
  health_status = 'healthy',
  upstream_model_name = 'large-v3',
  updated_at = NOW()
FROM models m
WHERE me.model_id = m.id AND m.name = 'whisper';

-- Disable lazy loading
INSERT INTO model_runtime_configs (model_id, workload_policy)
SELECT id, 'always_running' FROM models WHERE name = 'whisper'
ON CONFLICT (model_id) DO UPDATE 
SET workload_policy = 'always_running', updated_at = NOW();
EOF
```

Then restart the gateway to pick up the new config (or wait 10 seconds for auto-reload).
