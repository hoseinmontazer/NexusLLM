# Model Capabilities & Endpoint Validation

NexusLLM validates every inference request against the model's declared capabilities before routing it to any runtime backend (vLLM, llama.cpp, Ollama, etc.). This prevents misrouted requests — for example, sending a chat completion to a Whisper transcription model.

## How It Works

```
Incoming Request
      │
      ▼
Auth + Alias Resolution
      │
      ▼
Capability Validation  ◄── checks models.capabilities (DB)
      │
      ├── Not supported → HTTP 400 (never reaches backend)
      │
      └── Supported ──► Policy Checks → Backend Routing
```

Validation is **engine-independent**: it reads the `capabilities` JSONB column from the `models` table — the single source of truth — and never adds logic to vLLM, llama.cpp, or Ollama adapters.

---

## Endpoint → Capability Mapping

| API Endpoint | Required Capability |
|---|---|
| `POST /v1/chat/completions` | `chat` |
| `POST /v1/completions` | `completion` |
| `POST /v1/responses` | `responses` |
| `POST /v1/embeddings` | `embedding` |
| `POST /v1/rerank` | `rerank` |
| `POST /v1/audio/transcriptions` | `transcription` |
| `POST /v1/audio/speech` | `speech` |
| `POST /v1/images/generations` | `image_generation` |
| `POST /v1/moderations` | `moderation` |
| `POST /v1/ocr` | `ocr` |
| `GET /v1/models` | _(none — always allowed)_ |
| `GET /v1/models/:id` | _(none — always allowed)_ |

---

## Model Capabilities by Service Type

When registering a model, capabilities are derived automatically from `service_type` unless explicitly provided.

| `service_type` | Default Capabilities |
|---|---|
| `CHAT` | `chat`, `completion` |
| `EMBEDDING` | `embedding` |
| `RERANK` | `rerank` |
| `STT` | `transcription` |
| `TTS` | `speech` |
| `OCR` | `ocr` |
| `VISION` | `chat`, `vision` |
| `IMAGE_GENERATION` | `image_generation` |
| `MODERATION` | `moderation` |
| `AGENT` | `chat`, `completion` |
| `MCP` | `chat`, `completion` |

---

## Error Response Format

When a model does not support the requested endpoint, the gateway returns **HTTP 400** with a structured error body:

```json
{
  "error": {
    "type": "invalid_model",
    "message": "Model 'whisper-large-v3' does not support Chat Completions.",
    "required_capability": "chat",
    "model_capabilities": ["transcription"]
  }
}
```

### Fields

| Field | Type | Description |
|---|---|---|
| `type` | string | Always `"invalid_model"` |
| `message` | string | Human-readable description |
| `required_capability` | string | The capability required by the endpoint |
| `model_capabilities` | string[] | All capabilities the model actually has |

---

## Examples

### Whisper model → chat endpoint (rejected)

```http
POST /v1/chat/completions
Authorization: Bearer <key>

{
  "model": "whisper-large-v3",
  "messages": [{"role": "user", "content": "Hello"}]
}
```

**Response — HTTP 400**

```json
{
  "error": {
    "type": "invalid_model",
    "message": "Model 'whisper-large-v3' does not support Chat Completions.",
    "required_capability": "chat",
    "model_capabilities": ["transcription"]
  }
}
```

### LLM → transcription endpoint (rejected)

```http
POST /v1/audio/transcriptions
Authorization: Bearer <key>
Content-Type: multipart/form-data

model=llama-3.3-70b
file=@audio.mp3
```

**Response — HTTP 400**

```json
{
  "error": {
    "type": "invalid_model",
    "message": "Model 'llama-3.3-70b' does not support Audio Transcription.",
    "required_capability": "transcription",
    "model_capabilities": ["chat", "completion"]
  }
}
```

### LLM → chat endpoint (allowed, proceeds normally)

```http
POST /v1/chat/completions
Authorization: Bearer <key>

{
  "model": "llama-3.3-70b",
  "messages": [{"role": "user", "content": "Hello"}]
}
```

**Response — HTTP 200** _(normal chat completion response)_

---

## Managing Capabilities

### Deploy a new model with explicit capabilities

Pass the optional `capabilities` array when deploying. When omitted, capabilities are derived from `service_type`.

```http
POST /admin/v1/models/deploy
Content-Type: application/json

{
  "name": "whisper-large-v3",
  "display_name": "Whisper Large v3",
  "service_type": "STT",
  "backend_type": "openai_compat",
  "host": "localhost",
  "port": 8100,
  "capabilities": ["transcription"]
}
```

### Register an external model with custom capabilities

```http
POST /admin/v1/models
Content-Type: application/json

{
  "name": "my-vision-model",
  "display_name": "My Vision LLM",
  "backend_type": "openai_compat",
  "service_type": "VISION",
  "host": "vision-host",
  "port": 8200,
  "capabilities": ["chat", "vision", "completion"]
}
```

### Update capabilities for an existing model

No restart required — the gateway reads capabilities on every request from the DB (with in-flight requests completing normally).

```http
PUT /admin/v1/models/{model_id}/capabilities
Content-Type: application/json

{
  "capabilities": ["chat", "completion", "vision"]
}
```

**Response — HTTP 200**

```json
{
  "model_id": "...",
  "capabilities": ["chat", "completion", "vision"],
  "message": "capabilities updated — gateway is now enforcing the new list"
}
```

---

## Backward Compatibility

- Models created before migration 033 have their `capabilities` column backfilled from `service_type` automatically by **migration 037**.
- If `capabilities` is empty for any reason, the gateway falls back to `DefaultCapabilities(service_type)` — existing models continue to work without any manual intervention.
- Unknown models (not found in the DB) are not rejected by capability validation — the downstream pipeline produces the appropriate "model not found" error instead.

---

## curl Samples — Register Every Model Type

Replace `GATEWAY_URL`, `ADMIN_URL`, and `YOUR_API_KEY` with your actual values.

### LLM / Chat model (`CHAT`)

**Register:**
```bash
curl -X POST $ADMIN_URL/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name":         "llama-3.3-70b",
    "display_name": "Llama 3.3 70B",
    "service_type": "CHAT",
    "backend_type": "llamacpp",
    "capabilities": ["chat", "completion"],
    "host":         "localhost",
    "port":         8080,
    "start_now":    false
  }'
```

**Use:**
```bash
curl -X POST $GATEWAY_URL/v1/chat/completions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama-3.3-70b",
    "messages": [{"role": "user", "content": "Hello, how are you?"}]
  }'
```

**Wrong endpoint → HTTP 400:**
```bash
curl -X POST $GATEWAY_URL/v1/audio/transcriptions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -F "model=llama-3.3-70b" \
  -F "file=@audio.mp3"
# → {"error":{"type":"invalid_model","message":"Model 'llama-3.3-70b' does not support Audio Transcription.","required_capability":"transcription","model_capabilities":["chat","completion"]}}
```

---

### Embedding model (`EMBEDDING`)

**Register:**
```bash
curl -X POST $ADMIN_URL/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name":         "bge-m3",
    "display_name": "BGE M3",
    "service_type": "EMBEDDING",
    "backend_type": "openai_compat",
    "capabilities": ["embedding"],
    "host":         "localhost",
    "port":         8081,
    "start_now":    false
  }'
```

**Use:**
```bash
curl -X POST $GATEWAY_URL/v1/embeddings \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "bge-m3",
    "input": "The quick brown fox"
  }'
```

**Wrong endpoint → HTTP 400:**
```bash
curl -X POST $GATEWAY_URL/v1/chat/completions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"bge-m3","messages":[{"role":"user","content":"Hello"}]}'
# → {"error":{"type":"invalid_model","message":"Model 'bge-m3' does not support Chat Completions.","required_capability":"chat","model_capabilities":["embedding"]}}
```

---

### Speech-to-Text / Transcription (`STT`)

**Register:**
```bash
curl -X POST $ADMIN_URL/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name":         "whisper-large-v3",
    "display_name": "Whisper Large v3",
    "service_type": "STT",
    "backend_type": "openai_compat",
    "capabilities": ["transcription"],
    "host":         "localhost",
    "port":         8082,
    "start_now":    false
  }'
```

**Use:**
```bash
curl -X POST $GATEWAY_URL/v1/audio/transcriptions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -F "model=whisper-large-v3" \
  -F "file=@audio.mp3" \
  -F "response_format=json"
```

**Wrong endpoint → HTTP 400:**
```bash
curl -X POST $GATEWAY_URL/v1/chat/completions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"whisper-large-v3","messages":[{"role":"user","content":"Hello"}]}'
# → {"error":{"type":"invalid_model","message":"Model 'whisper-large-v3' does not support Chat Completions.","required_capability":"chat","model_capabilities":["transcription"]}}
```

---

### Text-to-Speech (`TTS`)

**Register:**
```bash
curl -X POST $ADMIN_URL/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name":         "kokoro-en",
    "display_name": "Kokoro English",
    "service_type": "TTS",
    "backend_type": "openai_compat",
    "capabilities": ["speech"],
    "host":         "localhost",
    "port":         8083,
    "start_now":    false
  }'
```

**Use:**
```bash
curl -X POST $GATEWAY_URL/v1/audio/speech \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "kokoro-en",
    "input": "Hello, this is a test.",
    "voice": "af_heart",
    "response_format": "mp3"
  }' \
  --output speech.mp3
```

---

### Reranker (`RERANK`)

**Register:**
```bash
curl -X POST $ADMIN_URL/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name":         "bge-reranker-v2",
    "display_name": "BGE Reranker v2",
    "service_type": "RERANK",
    "backend_type": "openai_compat",
    "capabilities": ["rerank"],
    "host":         "localhost",
    "port":         8084,
    "start_now":    false
  }'
```

**Use:**
```bash
curl -X POST $GATEWAY_URL/v1/rerank \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "bge-reranker-v2",
    "query": "What is the capital of France?",
    "documents": ["Paris is the capital.", "London is in England.", "France is in Europe."],
    "top_n": 2
  }'
```

---

### OCR (`OCR`)

**Register:**
```bash
curl -X POST $ADMIN_URL/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name":         "surya-ocr",
    "display_name": "Surya OCR",
    "service_type": "OCR",
    "backend_type": "openai_compat",
    "capabilities": ["ocr"],
    "host":         "localhost",
    "port":         8085,
    "start_now":    false
  }'
```

**Use:**
```bash
curl -X POST $GATEWAY_URL/v1/ocr \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "surya-ocr",
    "image": "<base64-encoded-image>"
  }'
```

---

### Vision / Multimodal (`VISION`)

**Register:**
```bash
curl -X POST $ADMIN_URL/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name":         "qwen2-vl-7b",
    "display_name": "Qwen2 VL 7B",
    "service_type": "VISION",
    "backend_type": "llamacpp",
    "capabilities": ["chat", "vision"],
    "host":         "localhost",
    "port":         8086,
    "start_now":    false
  }'
```

**Use:**
```bash
curl -X POST $GATEWAY_URL/v1/chat/completions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen2-vl-7b",
    "messages": [{
      "role": "user",
      "content": [
        {"type": "image_url", "image_url": {"url": "https://example.com/image.jpg"}},
        {"type": "text", "text": "What is in this image?"}
      ]
    }]
  }'
```

---

### Image Generation (`IMAGE_GENERATION`)

**Register:**
```bash
curl -X POST $ADMIN_URL/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name":         "stable-diffusion-xl",
    "display_name": "Stable Diffusion XL",
    "service_type": "IMAGE_GENERATION",
    "backend_type": "openai_compat",
    "capabilities": ["image_generation"],
    "host":         "localhost",
    "port":         8087,
    "start_now":    false
  }'
```

**Use:**
```bash
curl -X POST $GATEWAY_URL/v1/images/generations \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "stable-diffusion-xl",
    "prompt": "A photorealistic cat sitting on a red velvet chair",
    "n": 1,
    "size": "1024x1024"
  }'
```

---

### Moderation (`MODERATION`)

**Register:**
```bash
curl -X POST $ADMIN_URL/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name":         "text-moderation-v1",
    "display_name": "Text Moderation v1",
    "service_type": "MODERATION",
    "backend_type": "openai_compat",
    "capabilities": ["moderation"],
    "host":         "localhost",
    "port":         8088,
    "start_now":    false
  }'
```

**Use:**
```bash
curl -X POST $GATEWAY_URL/v1/moderations \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "text-moderation-v1",
    "input": "I want to buy a new laptop"
  }'
```

---

### Update capabilities for an existing model

No restart required — takes effect on the next request.

```bash
curl -X PUT $ADMIN_URL/admin/v1/models/{model_id}/capabilities \
  -H "Content-Type: application/json" \
  -d '{"capabilities": ["chat", "completion", "vision"]}'
```

---

## Design Principles

1. **Engine-independent** — validation runs in the gateway, before any backend adapter is selected.
2. **Single source of truth** — the `models.capabilities` JSONB column, populated and managed via the admin API.
3. **Fail-open for unknown models** — the gateway never silently blocks a model it doesn't know about; that is the policy engine's job.
4. **Extensible** — adding a new endpoint requires only one line in the `endpointCapability` map in `internal/proxy/capability.go`.
