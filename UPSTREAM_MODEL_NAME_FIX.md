# Fix: Whisper Model Name Mapping for faster-whisper-server

## Problem

When forwarding audio transcription requests to faster-whisper-server, the gateway was sending the NexusLLM model name (e.g., "whisper") directly. However, faster-whisper-server expects specific model size identifiers like "base", "medium", "large-v3", etc.

This resulted in the error:
```
ValueError: Invalid model size 'whisper', expected one of: tiny.en, tiny, base.en, base, 
small.en, small, medium.en, medium, large-v1, large-v2, large-v3, large, distil-large-v2, 
distil-medium.en, distil-small.en, distil-large-v3
```

## Solution

Implemented the `upstream_model_name` feature (migration 042 column was present but unused) to allow model name translation before forwarding requests to backend services.

## Changes Made

### 1. Runtime Layer (`internal/runtime/`)

**pool.go:**
- Added `UpstreamModelName string` field to `Endpoint` struct
- Updated documentation to explain the field's purpose

**registry.go:**
- Added `UpstreamModelName string` to `RegistryEndpoint` struct with `db:"upstream_model_name"` tag
- Updated SQL query in `loadEndpoints()` to read `upstream_model_name` from database
- Added mapping in `Reload()` to populate `Endpoint.UpstreamModelName` from DB rows
- Added empty string default for agent_runtimes UNION query (they don't have upstream config)

### 2. Proxy Layer (`internal/proxy/multiservice.go`)

**New Function:**
- `forwardMultipartWithModelSubstitution()` - Rebuilds multipart form data with substituted model name

**Updated Function:**
- `Transcriptions()` - Now checks if `ep.UpstreamModelName` is set and calls the new substitution function instead of raw forward

### 3. Admin API (`internal/admin/handlers/runtime.go`)

**Updated Function:**
- `UpdateUpstream()` - Added support for `upstream_model_name` parameter alongside existing upstream config fields

### 4. Documentation

**Created:**
- `docs/24-upstream-model-name.md` - Complete guide with examples and troubleshooting
- `examples/configure-whisper-upstream.sql` - SQL scripts for quick configuration

## How to Use

### Quick Fix for Your Whisper Issue

1. Find your model ID:
```sql
SELECT id, name FROM models WHERE name = 'whisper';
```

2. Update the endpoint configuration:
```bash
curl -X PUT http://localhost:8081/admin/v1/models/{model_id}/upstream \
  -H "Content-Type: application/json" \
  -d '{
    "upstream_model_name": "large-v3"
  }'
```

Or via SQL:
```sql
UPDATE model_endpoints 
SET upstream_model_name = 'large-v3', 
    updated_at = NOW()
WHERE model_id = (SELECT id FROM models WHERE name = 'whisper' LIMIT 1);
```

3. The gateway will automatically reload the configuration within 10 seconds.

4. Test the transcription:
```bash
curl -X POST http://localhost:8080/v1/audio/transcriptions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -F "model=whisper" \
  -F "file=@audio.mp3"
```

## Technical Details

### Before (Broken):
```
Client sends:     model=whisper → Gateway forwards: model=whisper → faster-whisper ERROR
```

### After (Fixed):
```
Client sends:     model=whisper 
                       ↓
Gateway resolves:  ep.UpstreamModelName = "large-v3"
                       ↓
Rewrites form:     model=large-v3
                       ↓
Forwards to:       faster-whisper-server → SUCCESS
```

### Multipart Form Rewriting

The implementation handles multipart form data correctly:
1. Reads and buffers the original request body
2. Parses the multipart form to extract all parts
3. Rebuilds the form with the substituted model name
4. Preserves file uploads and other form fields unchanged
5. Updates Content-Type header with new boundary
6. Forwards to backend

## Backward Compatibility

- ✅ If `upstream_model_name` is empty (default), behavior is unchanged
- ✅ Existing endpoints continue working without configuration changes
- ✅ Only affects endpoints where `upstream_model_name` is explicitly set
- ✅ No database migration required (column already exists from migration 042)

## Future Enhancements

The framework is now in place to add model name translation for other endpoints:
- Chat completions (for cloud providers with different model IDs)
- Embeddings (for backend-specific model identifiers)
- Image generation (for Stable Diffusion model variants)

## Testing

Build verification:
```bash
cd /home/hosein/Documents/p/llm-gateway/nexusllm
go build ./cmd/gateway
go build ./cmd/admin
```

Both compile successfully with the changes.

## Files Modified

1. `/internal/runtime/pool.go` - Added UpstreamModelName field
2. `/internal/runtime/registry.go` - Load and map UpstreamModelName from DB
3. `/internal/proxy/multiservice.go` - Implement model name substitution
4. `/internal/admin/handlers/runtime.go` - API support for updating the field

## Files Created

1. `/docs/24-upstream-model-name.md` - User documentation
2. `/examples/configure-whisper-upstream.sql` - Configuration examples
3. `/UPSTREAM_MODEL_NAME_FIX.md` - This file

## Migration Note

The database column `model_endpoints.upstream_model_name` was added in migration 042 but was never implemented in the code. This fix completes the feature implementation.

Check if your database has the column:
```sql
\d model_endpoints
```

If the column is missing, run migrations:
```bash
make migrate
```
