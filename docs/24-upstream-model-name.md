# Upstream Model Name Mapping

## Overview

The `upstream_model_name` feature allows NexusLLM to translate model names before forwarding requests to backend services. This is particularly useful when the backend service expects specific model identifiers that differ from your user-facing model names.

## Use Cases

### Faster-Whisper Server

faster-whisper-server expects exact model size names like:
- `tiny`, `tiny.en`
- `base`, `base.en`
- `small`, `small.en`
- `medium`, `medium.en`
- `large-v1`, `large-v2`, `large-v3`, `large`
- `distil-large-v2`, `distil-large-v3`, `distil-medium.en`, `distil-small.en`

If your NexusLLM model is named `whisper` or `whisper-transcription`, you need to map it to one of these specific sizes.

### OpenRouter / Cloud Providers

Cloud providers often use different model identifiers:
- Your NexusLLM model: `gpt-4`
- OpenRouter expects: `openai/gpt-4-turbo-preview`

## Configuration

### Via Admin API

Update the upstream model name for an existing model endpoint:

```bash
curl -X PUT http://localhost:8081/admin/v1/models/{model_id}/upstream \
  -H "Content-Type: application/json" \
  -d '{
    "upstream_model_name": "large-v3"
  }'
```

### Via Database

Update directly in PostgreSQL:

```sql
UPDATE model_endpoints 
SET upstream_model_name = 'large-v3', 
    updated_at = NOW()
WHERE model_id = (SELECT id FROM models WHERE name = 'whisper');
```

### Example: Configuring Whisper

1. **Create the model** (if not already exists):
```bash
curl -X POST http://localhost:8081/admin/v1/models \
  -H "Content-Type: application/json" \
  -d '{
    "name": "whisper",
    "display_name": "Whisper Transcription",
    "backend_type": "cpu_native",
    "service_type": "transcription",
    "capabilities": ["transcription"]
  }'
```

2. **Register the endpoint**:
```bash
curl -X POST http://localhost:8081/admin/v1/models/{model_id}/endpoints \
  -H "Content-Type: application/json" \
  -d '{
    "host": "192.168.0.200",
    "port": 8000,
    "base_path": "/v1"
  }'
```

3. **Set the upstream model name**:
```bash
curl -X PUT http://localhost:8081/admin/v1/models/{model_id}/upstream \
  -H "Content-Type: application/json" \
  -d '{
    "upstream_model_name": "large-v3"
  }'
```

## How It Works

When a client sends a transcription request:

```bash
curl -X POST http://localhost:8080/v1/audio/transcriptions \
  -F "model=whisper" \
  -F "file=@audio.mp3"
```

The gateway:
1. Authenticates and validates the request
2. Resolves `whisper` through aliases and policies
3. Looks up the endpoint configuration
4. Finds `upstream_model_name = "large-v3"`
5. **Rewrites the multipart form** to replace `model=whisper` with `model=large-v3`
6. Forwards the modified request to faster-whisper-server

The faster-whisper-server receives:
```
model=large-v3
file=<audio data>
```

## Empty Value Behavior

If `upstream_model_name` is empty (the default), the gateway forwards the NexusLLM model name unchanged. This is the standard behavior for most backends.

## Supported Endpoints

Currently, `upstream_model_name` is supported for:
- ✅ **Audio Transcriptions** (`/v1/audio/transcriptions`) - multipart form rewriting
- ❌ Chat Completions - planned for future release
- ❌ Embeddings - planned for future release
- ❌ TTS - not needed (TTS models typically accept any identifier)

## Troubleshooting

### Error: "Invalid model size 'whisper'"

This error means:
1. Your endpoint doesn't have `upstream_model_name` configured
2. Or the gateway hasn't reloaded the configuration yet

**Solution:**
```bash
# Set the upstream model name
curl -X PUT http://localhost:8081/admin/v1/models/{model_id}/upstream \
  -H "Content-Type: application/json" \
  -d '{
    "upstream_model_name": "large-v3"
  }'

# Force registry reload (or wait up to 10 seconds for auto-reload)
# The gateway reloads every 10 seconds automatically
```

### Verify Configuration

Check the current configuration:

```sql
SELECT 
    m.name AS model_name,
    me.host,
    me.port,
    me.upstream_model_name
FROM model_endpoints me
JOIN models m ON m.id = me.model_id
WHERE m.name = 'whisper';
```

## Migration Note

The `upstream_model_name` column was added in migration 042. If you're upgrading from an older version, ensure migrations have run:

```bash
# Check if the column exists
psql -d nexusllm -c "\d model_endpoints" | grep upstream_model_name

# If missing, run migrations
make migrate
```

## Related Configuration

- `upstream_api_key` - Authorization token for cloud providers
- `upstream_base_url` - Override endpoint URL for cloud services
- `upstream_proxy` - HTTP/SOCKS5 proxy for outbound requests

All four fields can be set via the same API endpoint:

```bash
curl -X PUT http://localhost:8081/admin/v1/models/{model_id}/upstream \
  -H "Content-Type: application/json" \
  -d '{
    "upstream_model_name": "large-v3",
    "upstream_base_url": "https://api.example.com",
    "upstream_api_key": "sk-...",
    "upstream_proxy": "http://proxy:3128"
  }'
```
