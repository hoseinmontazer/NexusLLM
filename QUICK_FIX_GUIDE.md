# Quick Fix: Whisper "Invalid model size" Error

## The Problem You're Seeing

```
ERROR: ValueError: Invalid model size 'whisper', expected one of: tiny.en, tiny, base.en, 
base, small.en, small, medium.en, medium, large-v1, large-v2, large-v3, large...
```

## The Solution - Two Options!

### Option 1: Per-Request Control (Recommended! ⭐)

Let users specify the model size dynamically in each request:

```bash
# Use large-v3 for high quality
curl -X POST http://localhost:8080/v1/audio/transcriptions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -F "model=whisper" \
  -F "upstream_model=large-v3" \
  -F "file=@audio.mp3"

# Use small for faster processing
curl -X POST http://localhost:8080/v1/audio/transcriptions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -F "model=whisper" \
  -F "upstream_model=small" \
  -F "file=@audio.mp3"

# Use base for quick transcription
curl -X POST http://localhost:8080/v1/audio/transcriptions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -F "model=whisper" \
  -F "upstream_model=base" \
  -F "file=@audio.mp3"
```

**No configuration needed!** Just add the `upstream_model` field to your request.

### Option 2: Fixed Configuration

Set a default upstream model in the database (users can't change it per-request):

```bash
# Get your model ID
psql -d nexusllm -c "SELECT id FROM models WHERE name = 'whisper';"

# Set default upstream model
curl -X PUT http://localhost:8081/admin/v1/models/{model_id}/upstream \
  -H "Content-Type: application/json" \
  -d '{"upstream_model_name": "large-v3"}'

# Now all requests use large-v3 automatically
curl -X POST http://localhost:8080/v1/audio/transcriptions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -F "model=whisper" \
  -F "file=@audio.mp3"
```

## Priority Order

When determining which upstream model to use:

1. **`upstream_model` in request** (highest priority) ← User choice
2. **`upstream_model_name` in DB config** ← Admin default
3. **Forward as-is** ← No translation (will fail for faster-whisper)

## Examples

### Flexible Setup (Best for Multiple Model Sizes)

```bash
# No DB config needed - users choose per request

# User 1 wants quality
curl ... -F "model=whisper" -F "upstream_model=large-v3" -F "file=@audio.mp3"

# User 2 wants speed
curl ... -F "model=whisper" -F "upstream_model=base" -F "file=@audio.mp3"

# User 3 wants balance
curl ... -F "model=whisper" -F "upstream_model=medium" -F "file=@audio.mp3"
```

All work! The gateway routes to your faster-whisper-server and translates the model name.

### Fixed Setup (Enforce One Model Size)

```bash
# Admin sets large-v3 as default
curl -X PUT http://localhost:8081/admin/v1/models/{model_id}/upstream \
  -d '{"upstream_model_name": "large-v3"}'

# Users just send model=whisper (no upstream_model needed)
curl ... -F "model=whisper" -F "file=@audio.mp3"
# Gateway automatically uses large-v3
```

### Hybrid Setup (Default + Override)

```bash
# Admin sets a default
curl -X PUT http://localhost:8081/admin/v1/models/{model_id}/upstream \
  -d '{"upstream_model_name": "medium"}'

# Most users don't specify upstream_model (get medium)
curl ... -F "model=whisper" -F "file=@audio.mp3"

# Power users can override
curl ... -F "model=whisper" -F "upstream_model=large-v3" -F "file=@audio.mp3"
```

## Available faster-whisper Model Sizes

- `tiny`, `tiny.en` - Fastest, lowest quality
- `base`, `base.en` - Fast, decent quality
- `small`, `small.en` - Balanced
- `medium`, `medium.en` - High quality
- `large-v1`, `large-v2`, `large-v3`, `large` - Best quality
- `distil-large-v2`, `distil-large-v3` - Optimized versions
- `distil-medium.en`, `distil-small.en` - Distilled variants

## How It Works

```
Client Request:
  model=whisper
  upstream_model=large-v3
  file=audio.mp3
         ↓
Gateway extracts upstream_model field
         ↓
Rewrites multipart form:
  model=large-v3  ← Changed!
  file=audio.mp3  ← Unchanged
         ↓
Forwards to faster-whisper-server
         ↓
SUCCESS! ✓
```

The `upstream_model` field is:
- ✅ Extracted by the gateway
- ✅ Used to replace the model field
- ✅ **Stripped from the upstream request** (faster-whisper doesn't need it)

## Troubleshooting

### Still getting "Invalid model size" error?

Make sure you're sending the `upstream_model` field:

```bash
# ❌ Wrong - missing upstream_model
curl ... -F "model=whisper" -F "file=@audio.mp3"

# ✅ Correct - includes upstream_model
curl ... -F "model=whisper" -F "upstream_model=large-v3" -F "file=@audio.mp3"
```

Or set a default in the DB (see Option 2 above).

### Which model size should I use?

Test with a sample file:

```bash
# Try different sizes
for size in tiny base small medium large-v3; do
  echo "Testing $size..."
  time curl ... -F "model=whisper" -F "upstream_model=$size" -F "file=@test.mp3"
done
```

Pick based on your speed vs. quality needs.

### Can I use multiple faster-whisper servers?

Yes! Set up multiple model endpoints, each pointing to a different faster-whisper instance:

```bash
# faster-whisper-1 (small models): 192.168.0.200:8000
# faster-whisper-2 (large models): 192.168.0.201:8000

# Both registered under model="whisper"
# Gateway will load-balance between them
```

## Need More Help?

- `/docs/24-upstream-model-name.md` - Complete feature guide
- `/UPSTREAM_MODEL_NAME_FIX.md` - Technical implementation details
