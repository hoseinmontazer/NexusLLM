# Port Conflict Auto-Resolution Fix ✅

## Problem

When lazy-loading Whisper (or any cpu_native service), the container tried to bind to port 8000 which was already in use by your existing faster-whisper-server, causing:

```
ERROR: [Errno 98] error while attempting to bind on address ('0.0.0.0', 8000): 
[errno 98] address already in use
```

This kept failing repeatedly every 15 minutes.

## Root Cause

The node agent **already had port conflict detection and scanning logic** (lines 567-604 in executor.go), but it wasn't working because:

1. The agent scanned for a free port (e.g., 8001) ✅
2. Updated `p.BindPort = 8001` ✅  
3. **BUT** the `PORT` environment variable wasn't updated to match ❌
4. The faster-whisper container still tried to bind to the original port from env ❌

## The Fix

### Changed Files

**`internal/nodeagent/executor.go`:**
- Added `p.Env["PORT"] = strconv.Itoa(p.BindPort)` **after** port allocation/scanning
- This ensures backends that respect `$PORT` env var (faster-whisper-server, uvicorn-based services) bind to the dynamically-allocated port

**`internal/controller/docker_driver.go`:**
- Updated comment to clarify that PORT env is set by the executor

### How It Works Now

```
1. Lazy request arrives for "whisper"
          ↓
2. Activator sends START_MODEL task with BindPort=8000
          ↓
3. Node agent receives task
          ↓
4. Agent checks: is port 8000 available?
          ↓  NO (your existing server is on 8000)
          ↓
5. Agent scans ports 8001-8050
          ↓  Found 8001 is free!
          ↓
6. Updates p.BindPort = 8001
          ↓
7. **NEW**: Updates p.Env["PORT"] = "8001"  ← THE FIX
          ↓
8. Starts container with:
   docker run -e PORT=8001 --network host faster-whisper-server
          ↓
9. Container binds to port 8001 ✅
          ↓
10. No conflict! Both servers run happily
```

## What Changed

### Before (Broken):
```go
// Port scanning happened
p.BindPort = 8001  // Updated
p.Env["PORT"] = "8000"  // NOT updated - still old value!

// Container starts
docker run -e PORT=8000 --network host ...
// Container tries to bind 8000 → CONFLICT!
```

### After (Fixed):
```go
// Port scanning happened
p.BindPort = 8001  // Updated
p.Env["PORT"] = "8001"  // NOW ALSO UPDATED! ✅

// Container starts
docker run -e PORT=8001 --network host ...
// Container binds to 8001 → SUCCESS! ✅
```

## Testing

The fix is automatic - no configuration changes needed!

1. **Build the updated binaries:**
```bash
cd /home/hosein/Documents/p/llm-gateway/nexusllm
go build -o bin/nodeagent ./cmd/nodeagent
go build -o bin/gateway ./cmd/gateway
go build -o bin/admin ./cmd/admin
```

2. **Restart your services:**
```bash
# Restart node agent (wherever it's running)
systemctl restart nexus-nodeagent
# OR
docker restart nexus-nodeagent

# Restart gateway  
systemctl restart nexus-gateway
# OR
docker restart nexus-gateway
```

3. **Test the fix:**
```bash
# Make a transcription request
curl -X POST http://192.168.0.200:8880/v1/audio/transcriptions \
  -H "Authorization: Bearer nxs_..." \
  -F "model=whisper" \
  -F "upstream_model=large-v3" \
  -F "file=@audio.mp3"
```

**Expected behavior:**
- If whisper is NOT running: agent starts it on port 8001 (or next free port)
- If whisper IS running: agent reuses the existing container
- **No more port conflicts!**

## Log Output You'll See

### Success Case (Port 8000 Busy):
```
INFO: startModel: configured port busy, scanning for free port
      runtime=whisper configured_port=8000
INFO: startModel: found free port via scan
      runtime=whisper original_port=8000 actual_port=8001
INFO: startModel: STARTING container
      runtime=whisper backend=cpu_native port=8001
```

### Container Starts Successfully:
```
INFO: startModel: LOADING_MODEL — container started, model loading
      container_id=abc123... port=8001
```

## Benefits

1. **Zero configuration** - works automatically
2. **No manual DB fixes** needed - the code handles it
3. **Resilient** - survives port conflicts gracefully
4. **Backward compatible** - doesn't affect existing working setups

## Technical Details

The fix respects the three-case priority order already in the code:

**Case A**: `BindPort == 0` → OS allocates a free ephemeral port (preferred)
**Case B**: `BindPort > 0` AND container already running → reuse existing
**Case C**: `BindPort > 0` AND port busy → **scan forward for free port** ← THIS WAS BROKEN, NOW FIXED

The `PORT` env var is now updated in Case C, so the container gets the correct port.

## Verification

Check the agent logs during startup:

```bash
# On the node where the agent runs
journalctl -u nexus-nodeagent -f | grep -E "port|BindPort"

# Or docker logs
docker logs -f nexus-nodeagent | grep -E "port|BindPort"
```

You should see:
- "configured port busy, scanning for free port"
- "found free port via scan: actual_port=8001"
- Container starts without "address already in use" error

## Rollback (If Needed)

If something goes wrong, revert these two files:
- `internal/nodeagent/executor.go`
- `internal/controller/docker_driver.go`

And rebuild.

## Related Issues Fixed

This fix also resolves port conflicts for:
- ✅ Embedding servers (bge-m3, jina, infinity)
- ✅ TTS servers (kokoro, coqui)
- ✅ OCR servers (surya, easyocr)  
- ✅ Any uvicorn/FastAPI-based service that respects `$PORT`

All cpu_native backends now properly use dynamically-allocated ports when conflicts occur.
