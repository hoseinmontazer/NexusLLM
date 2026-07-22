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

## Design Principles

1. **Engine-independent** — validation runs in the gateway, before any backend adapter is selected.
2. **Single source of truth** — the `models.capabilities` JSONB column, populated and managed via the admin API.
3. **Fail-open for unknown models** — the gateway never silently blocks a model it doesn't know about; that is the policy engine's job.
4. **Extensible** — adding a new endpoint requires only one line in the `endpointCapability` map in `internal/proxy/capability.go`.
