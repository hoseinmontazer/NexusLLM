# Model Capabilities & Endpoint Validation

NexusLLM validates every inference request against the model's declared capabilities before routing it to any runtime backend (vLLM, llama.cpp, TGI, etc.). This prevents misrouted requests — for example, sending a chat completion to a Whisper transcription model.

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

Validation is **engine-independent**: it reads the `capabilities` JSONB column from the `models` table — the single source of truth — and never adds logic to vLLM, llama.cpp, or TGI adapters.

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

---

## Backend Compatibility by Model Type

This answers the question: **which `backend_type` can I use for each model type?**

### Can I use llama.cpp or vLLM for STT (Whisper)?

**No.** Neither llama.cpp nor vLLM can serve audio transcription.

- **llama.cpp** only handles GGUF text-generation models. It has no audio pipeline whatsoever.
- **vLLM** has experimental Whisper support in recent versions, but it is not production-stable and does not expose the `/v1/audio/transcriptions` endpoint in OpenAI-compatible form.

For STT you must use a dedicated audio server (`backend_type: openai_compat`). See the options below.

---

### Backend Support Matrix

| Model Type | `llamacpp` | `vllm` | `tgi` | `openai_compat` (recommended) |
|---|:---:|:---:|:---:|:---:|
| **CHAT** (text LLM) | ✅ best for GGUF | ✅ best for large GPU | ✅ | ✅ |
| **VISION** (multimodal) | ✅ llava/qwen-vl GGUF | ✅ | ⚠️ limited | ✅ |
| **EMBEDDING** | ❌ | ⚠️ limited | ✅ TEI | ✅ infinity/TEI |
| **RERANK** | ❌ | ❌ | ✅ TEI | ✅ TEI/infinity |
| **STT** | ❌ no audio | ❌ not stable | ❌ | ✅ **only option** |
| **TTS** | ❌ | ❌ | ❌ | ✅ **only option** |
| **OCR** | ❌ | ❌ | ❌ | ✅ **only option** |
| **IMAGE_GENERATION** | ❌ | ❌ | ❌ | ✅ **only option** |
| **MODERATION** | ❌ | ⚠️ | ❌ | ✅ |

✅ = supported and production-tested  ⚠️ = works but not recommended  ❌ = not supported

---

## Real-World Model Catalog

Concrete Docker images and register commands for every model type. All use `backend_type: openai_compat` unless noted.

---

### Chat / LLM

These are served by llama.cpp or vLLM — both expose the OpenAI chat API.

| Model | Size | Backend | Docker Image | Notes |
|---|---|---|---|---|
| `llama-3.3-70b` | 70B | `llamacpp` | `ghcr.io/ggml-org/llama.cpp:server-cuda` | Best GGUF option for 70B |
| `qwen3-32b` | 32B | `llamacpp` | `ghcr.io/ggml-org/llama.cpp:server-cuda` | Supports thinking mode |
| `gemma-2-2b` | 2B | `llamacpp` | `ghcr.io/ggml-org/llama.cpp:server` | CPU-capable |
| `deepseek-r1-70b` | 70B | `vllm` | `vllm/vllm-openai:latest` | Reasoning model, needs GPU |

```bash
# llama.cpp chat model
curl -X POST $ADMIN_URL/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name": "llama-3.3-70b", "display_name": "Llama 3.3 70B",
    "service_type": "CHAT", "backend_type": "llamacpp",
    "capabilities": ["chat", "completion"],
    "image": "ghcr.io/ggml-org/llama.cpp:server-cuda",
    "host": "localhost", "port": 8080,
    "llamacpp_hf_repo": "bartowski/Llama-3.3-70B-Instruct-GGUF",
    "llamacpp_hf_file": "Llama-3.3-70B-Instruct-Q4_K_M.gguf",
    "llamacpp_ctx_size": 8192, "llamacpp_n_gpu_layers": -1,
    "start_now": false
  }'

# vLLM chat model
curl -X POST $ADMIN_URL/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name": "deepseek-r1-70b", "display_name": "DeepSeek R1 70B",
    "service_type": "CHAT", "backend_type": "vllm",
    "capabilities": ["chat", "completion"],
    "image": "vllm/vllm-openai:latest",
    "hf_model_id": "deepseek-ai/DeepSeek-R1-Distill-Llama-70B",
    "tensor_parallel": 2, "gpu_memory_util": 0.9,
    "host": "localhost", "port": 8090,
    "start_now": false
  }'
```

---

### Speech-to-Text (STT)

**Required backend:** `openai_compat` — dedicated audio server only. llama.cpp and vLLM cannot do STT.

| Server | Docker Image | GPU | Notes |
|---|---|---|---|
| `faster-whisper-server` | `fedirz/faster-whisper-server:latest-cuda` | optional | Best performance, OpenAI-compatible |
| `faster-whisper-server` (CPU) | `fedirz/faster-whisper-server:latest-cpu` | ❌ | No GPU needed |
| `whisper.cpp` server | `ghcr.io/ggml-org/whisper.cpp:server` | optional | Lightweight, GGML-based |
| `openai-whisper` REST | `onerahmet/openai-whisper-asr-webservice:latest` | optional | Simple but slower |

```bash
# faster-whisper (GPU) — recommended
curl -X POST $ADMIN_URL/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name": "whisper-large-v3", "display_name": "Whisper Large v3",
    "service_type": "STT", "backend_type": "openai_compat",
    "capabilities": ["transcription"],
    "image": "fedirz/faster-whisper-server:latest-cuda",
    "host": "localhost", "port": 8100,
    "extra_args": ["--model", "Systran/faster-whisper-large-v3"],
    "start_now": false
  }'

# faster-whisper (CPU only — no GPU needed)
curl -X POST $ADMIN_URL/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name": "whisper-medium", "display_name": "Whisper Medium (CPU)",
    "service_type": "STT", "backend_type": "openai_compat",
    "capabilities": ["transcription"],
    "image": "fedirz/faster-whisper-server:latest-cpu",
    "host": "localhost", "port": 8101,
    "extra_args": ["--model", "Systran/faster-whisper-medium"],
    "start_now": false
  }'

# Use — multipart form upload
curl -X POST $GATEWAY_URL/v1/audio/transcriptions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -F "model=whisper-large-v3" \
  -F "file=@recording.mp3" \
  -F "language=en" \
  -F "response_format=json"
```

---

### Text-to-Speech (TTS)

**Required backend:** `openai_compat` only.

| Server | Docker Image | Voices | Notes |
|---|---|---|---|
| `kokoro-fastapi` | `ghcr.io/remsky/kokoro-fastapi-cpu:latest` | 20+ voices | Lightweight, CPU |
| `kokoro-fastapi` (GPU) | `ghcr.io/remsky/kokoro-fastapi-gpu:latest` | 20+ voices | Faster on GPU |
| `piper-tts` | `rhasspy/piper:latest` | 900+ | Many languages |
| `xtts-v2` | `ghcr.io/coqui-tts/tts-server:latest` | clone voices | Voice cloning |

```bash
# Kokoro TTS (CPU)
curl -X POST $ADMIN_URL/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name": "kokoro-en", "display_name": "Kokoro TTS",
    "service_type": "TTS", "backend_type": "openai_compat",
    "capabilities": ["speech"],
    "image": "ghcr.io/remsky/kokoro-fastapi-cpu:latest",
    "host": "localhost", "port": 8200,
    "start_now": false
  }'

# Use
curl -X POST $GATEWAY_URL/v1/audio/speech \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"kokoro-en","input":"Hello world","voice":"af_heart","response_format":"mp3"}' \
  --output out.mp3
```

---

### Embeddings

`openai_compat` is recommended for embeddings.

| Server | Docker Image | Notes |
|---|---|---|
| `infinity` | `michaelf34/infinity:latest` | Best for BGE, E5, Jina |
| `text-embeddings-inference` (TEI) | `ghcr.io/huggingface/text-embeddings-inference:cpu-1.5` | HuggingFace official |

```bash
# infinity embedding server
curl -X POST $ADMIN_URL/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name": "bge-m3", "display_name": "BGE M3",
    "service_type": "EMBEDDING", "backend_type": "openai_compat",
    "capabilities": ["embedding"],
    "image": "michaelf34/infinity:latest",
    "host": "localhost", "port": 8300,
    "extra_args": ["v2", "--model-name-or-path", "BAAI/bge-m3"],
    "start_now": false
  }'

# Use
curl -X POST $GATEWAY_URL/v1/embeddings \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"bge-m3","input":"The quick brown fox"}'
```

---

### Reranker

`openai_compat` only — TEI or infinity.

```bash
# TEI reranker
curl -X POST $ADMIN_URL/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name": "bge-reranker-v2-m3", "display_name": "BGE Reranker v2 M3",
    "service_type": "RERANK", "backend_type": "openai_compat",
    "capabilities": ["rerank"],
    "image": "ghcr.io/huggingface/text-embeddings-inference:cpu-1.5",
    "host": "localhost", "port": 8400,
    "extra_args": ["--model-id", "BAAI/bge-reranker-v2-m3"],
    "start_now": false
  }'

# Use
curl -X POST $GATEWAY_URL/v1/rerank \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "bge-reranker-v2-m3",
    "query": "What is the capital of France?",
    "documents": ["Paris is the capital.", "London is in England."],
    "top_n": 1
  }'
```

---

### Vision / Multimodal

llama.cpp supports vision via LLaVA/Qwen-VL GGUF. vLLM supports it natively.

```bash
# Qwen2-VL via llama.cpp
curl -X POST $ADMIN_URL/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name": "qwen2-vl-7b", "display_name": "Qwen2 VL 7B",
    "service_type": "VISION", "backend_type": "llamacpp",
    "capabilities": ["chat", "vision"],
    "image": "ghcr.io/ggml-org/llama.cpp:server-cuda",
    "host": "localhost", "port": 8500,
    "llamacpp_hf_repo": "bartowski/Qwen2-VL-7B-Instruct-GGUF",
    "llamacpp_hf_file": "Qwen2-VL-7B-Instruct-Q4_K_M.gguf",
    "llamacpp_n_gpu_layers": -1,
    "start_now": false
  }'

# Use — image + text
curl -X POST $GATEWAY_URL/v1/chat/completions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen2-vl-7b",
    "messages": [{
      "role": "user",
      "content": [
        {"type":"image_url","image_url":{"url":"https://example.com/photo.jpg"}},
        {"type":"text","text":"Describe this image in detail."}
      ]
    }]
  }'
```

---

### Image Generation

`openai_compat` only — any server that exposes `/v1/images/generations`.

| Server | Docker Image | Notes |
|---|---|---|
| `stable-diffusion-webui` | `siutin/stable-diffusion-webui-docker:latest` | A1111 with API |
| `invokeai` | `ghcr.io/invoke-ai/invokeai:latest` | Feature-rich |
| `diffusers-api` | `benzin/diffusers-api:latest` | HuggingFace diffusers |

```bash
curl -X POST $ADMIN_URL/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name": "sdxl", "display_name": "Stable Diffusion XL",
    "service_type": "IMAGE_GENERATION", "backend_type": "openai_compat",
    "capabilities": ["image_generation"],
    "image": "ghcr.io/invoke-ai/invokeai:latest",
    "host": "localhost", "port": 8600,
    "start_now": false
  }'
```

---

### OCR

`openai_compat` only.

| Server | Docker Image | Notes |
|---|---|---|
| `surya` | `vikparuchuri/surya:latest` | State-of-the-art, multilingual |
| `paddleocr` | `ghcr.io/paddlepaddle/paddleocr:latest` | Fast, 80+ languages |
| `got-ocr2` | custom build | General OCR with layout |

```bash
curl -X POST $ADMIN_URL/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name": "surya-ocr", "display_name": "Surya OCR",
    "service_type": "OCR", "backend_type": "openai_compat",
    "capabilities": ["ocr"],
    "image": "vikparuchuri/surya:latest",
    "host": "localhost", "port": 8700,
    "start_now": false
  }'
```

---

### Quick Backend Decision Guide

```
What do you want to run?
│
├── Text LLM (chat, completion)
│   ├── GGUF file on disk / small-medium model → llamacpp
│   ├── Large model (70B+), multi-GPU → vllm
│   └── Quick local test → llamacpp (cpu mode)
│
├── Vision (image + text) → llamacpp (LLaVA/Qwen-VL GGUF) or vllm
│
├── Audio transcription (STT) → openai_compat (faster-whisper-server)
│
├── Text-to-Speech (TTS) → openai_compat (kokoro-fastapi or piper)
│
├── Embeddings → openai_compat (infinity or TEI)
│
├── Reranker → openai_compat (TEI or infinity)
│
├── Image generation → openai_compat (InvokeAI or A1111)
│
└── OCR → openai_compat (surya or paddleocr)
```

**Rule of thumb:** if it's not a text LLM or vision model, the answer is always `openai_compat` with a dedicated server for that task.

---

## Deploying Multiple STT Models (and Cloud Models)

NexusLLM is a gateway — it routes requests to any OpenAI-compatible HTTP endpoint,
whether that endpoint is a local container you manage or a cloud API running externally.
The pattern is identical for both.

```
Client → NexusLLM Gateway → (local container | cloud API | any HTTP endpoint)
                 ↑
         same ACL, policies,
         audit log for all
```

---

### Local: Multiple Self-Hosted STT Models

Each STT model runs in its own container on its own port. Different models need
different Docker images because they use different inference engines.

| Model name | Docker image | Port | Engine |
|---|---|---|---|
| `whisper-large-v3` | `fedirz/faster-whisper-server:latest-cuda` | 8100 | faster-whisper (GPU) |
| `whisper-medium-cpu` | `fedirz/faster-whisper-server:latest-cpu` | 8101 | faster-whisper (CPU) |
| `whisper-turbo` | `fedirz/faster-whisper-server:latest-cuda` | 8102 | faster-whisper (GPU), turbo model |
| `moonshine-base` | `usefulsensors/moonshine:latest` | 8103 | Moonshine (ultra-fast, English) |
| `whisper-cpp` | `ghcr.io/ggml-org/whisper.cpp:server` | 8104 | whisper.cpp (GGML, lightweight) |

**Register all of them:**

```bash
ADMIN="http://localhost:8881/admin/v1"

# whisper-large-v3 (GPU)
curl -s -X POST $ADMIN/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name": "whisper-large-v3",
    "display_name": "Whisper Large v3 (GPU)",
    "service_type": "STT",
    "backend_type": "openai_compat",
    "capabilities": ["transcription"],
    "image": "fedirz/faster-whisper-server:latest-cuda",
    "host": "localhost", "port": 8100,
    "extra_args": ["--model", "Systran/faster-whisper-large-v3"],
    "start_now": false
  }'

# whisper-medium (CPU, no GPU needed)
curl -s -X POST $ADMIN/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name": "whisper-medium-cpu",
    "display_name": "Whisper Medium (CPU)",
    "service_type": "STT",
    "backend_type": "openai_compat",
    "capabilities": ["transcription"],
    "image": "fedirz/faster-whisper-server:latest-cpu",
    "host": "localhost", "port": 8101,
    "extra_args": ["--model", "Systran/faster-whisper-medium"],
    "start_now": false
  }'

# whisper-turbo (GPU, fastest large model)
curl -s -X POST $ADMIN/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name": "whisper-turbo",
    "display_name": "Whisper Turbo (GPU)",
    "service_type": "STT",
    "backend_type": "openai_compat",
    "capabilities": ["transcription"],
    "image": "fedirz/faster-whisper-server:latest-cuda",
    "host": "localhost", "port": 8102,
    "extra_args": ["--model", "Systran/faster-whisper-large-v3-turbo"],
    "start_now": false
  }'
```

**Grant to team and use:**

```bash
TEAM_ID="03a48733-be91-41da-97ee-1cd9de2ba237"

# Grant all three STT models to the team
for model in whisper-large-v3 whisper-medium-cpu whisper-turbo; do
  curl -s -X POST $ADMIN/teams/$TEAM_ID/models \
    -H "Content-Type: application/json" \
    -d "{\"model_name\":\"$model\"}"
done

# Use any of them — client picks by model name
curl -X POST http://localhost:8880/v1/audio/transcriptions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -F "model=whisper-large-v3" \
  -F "file=@recording.mp3"

curl -X POST http://localhost:8880/v1/audio/transcriptions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -F "model=whisper-turbo" \
  -F "file=@recording.mp3"
```

---

### Cloud: Register External / Cloud Models

Cloud STT APIs (Google Chirp, OpenAI Whisper API, AWS Transcribe adapter, etc.)
are already running externally. You do **not** deploy them — you **register** them
as external endpoints. No Docker image, no container management.

The gateway treats them identically to local models: same ACL, same capability
validation, same audit log, same `/v1/audio/transcriptions` endpoint for clients.

#### OpenAI Whisper API (direct passthrough)

The simplest case — the gateway proxies directly to `api.openai.com`.
Register it pointing at an OpenAI-compatible proxy that adds your API key:

```bash
# Option: use litellm as an OpenAI key proxy
# litellm proxies /v1/audio/transcriptions to OpenAI and handles auth

curl -s -X POST $ADMIN/models \
  -H "Content-Type: application/json" \
  -d '{
    "name": "whisper-openai",
    "display_name": "OpenAI Whisper API",
    "service_type": "STT",
    "backend_type": "openai_compat",
    "capabilities": ["transcription"],
    "host": "your-litellm-proxy.internal",
    "port": 4000
  }'
```

#### Google Chirp via LiteLLM Proxy

Google Chirp 3 is Google Cloud Speech-to-Text v2. LiteLLM can proxy it as an
OpenAI-compatible endpoint:

```bash
# 1. Run litellm proxy (once, as a service)
#    litellm --model vertex_ai/chirp-3 --port 4001
#    Or in Docker:
docker run -d \
  -e GOOGLE_APPLICATION_CREDENTIALS=/tmp/key.json \
  -v /path/to/gcp-key.json:/tmp/key.json \
  -p 4001:4000 \
  ghcr.io/berriai/litellm:main-latest \
  --model vertex_ai/chirp-3

# 2. Register in NexusLLM — no image, no container
curl -s -X POST $ADMIN/models \
  -H "Content-Type: application/json" \
  -d '{
    "name": "chirp-3",
    "display_name": "Google Chirp 3",
    "service_type": "STT",
    "backend_type": "openai_compat",
    "capabilities": ["transcription"],
    "host": "localhost",
    "port": 4001
  }'

# 3. Grant to team
curl -s -X POST $ADMIN/teams/$TEAM_ID/models \
  -H "Content-Type: application/json" \
  -d '{"model_name":"chirp-3"}'

# 4. Use — exactly the same as any local model
curl -X POST http://localhost:8880/v1/audio/transcriptions \
  -H "Authorization: Bearer YOUR_NEXUS_KEY" \
  -F "model=chirp-3" \
  -F "file=@recording.mp3"
```

#### Any Cloud LLM (GPT-4, Claude, Gemini, etc.)

Same pattern — register any cloud LLM via a LiteLLM proxy as an external model.
The gateway applies your org's ACL, rate limits, and audit logging on top:

```bash
# GPT-4o via litellm
curl -s -X POST $ADMIN/models \
  -H "Content-Type: application/json" \
  -d '{
    "name": "gpt-4o",
    "display_name": "OpenAI GPT-4o",
    "service_type": "CHAT",
    "backend_type": "openai_compat",
    "capabilities": ["chat", "completion"],
    "host": "your-litellm-proxy.internal",
    "port": 4000
  }'

# Claude 3.5 Sonnet via litellm
curl -s -X POST $ADMIN/models \
  -H "Content-Type: application/json" \
  -d '{
    "name": "claude-3-5-sonnet",
    "display_name": "Anthropic Claude 3.5 Sonnet",
    "service_type": "CHAT",
    "backend_type": "openai_compat",
    "capabilities": ["chat", "completion"],
    "host": "your-litellm-proxy.internal",
    "port": 4000
  }'
```

---

### The Pattern — Unified Workflow for All Models

Whether local or cloud, the steps are always the same:

```
1. Make the model available as an OpenAI-compatible HTTP endpoint
   ├── Local:  deploy with Docker image via POST /admin/v1/models/deploy
   └── Cloud:  run litellm proxy + register via POST /admin/v1/models

2. Grant to team
   POST /admin/v1/teams/:team_id/models  {"model_name": "..."}

3. (Optional) Create project-scoped API key with rate limits
   POST /admin/v1/projects  → POST /admin/v1/teams/:id/api-keys

4. Use — client sends to gateway, gateway routes to whatever backend
   POST /v1/audio/transcriptions  (or /v1/chat/completions, /v1/embeddings, ...)
   with "model": "your-model-name"
```

NexusLLM never knows or cares whether the backend is a local Docker container
or a Google Cloud API — it just proxies the request, enforces your policies,
and records usage.
