# Gateway API — Inference

The gateway is a drop-in replacement for the OpenAI API. Any code using `openai` SDK works without changes — just point `base_url` at the gateway.

**Base URL:** `http://localhost:8080`  
**Auth:** `Authorization: Bearer nxs_...`

---

## Chat completions

`POST /v1/chat/completions`

Identical to OpenAI's chat completions API. Supports:
- Streaming (`"stream": true`) via SSE
- Tool use / function calling
- Response format (JSON mode)
- All standard parameters

```bash
# Non-streaming
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer nxs_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model":       "gemma2:2b",
    "messages":    [{"role": "user", "content": "Explain gravity briefly."}],
    "max_tokens":  200,
    "temperature": 0.7
  }'
```

```bash
# Streaming
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer nxs_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model":    "gemma2:2b",
    "messages": [{"role": "user", "content": "Count to 5."}],
    "stream":   true
  }'
```

**Python:**
```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="nxs_...")

# Non-streaming
response = client.chat.completions.create(
    model="gemma2:2b",
    messages=[{"role": "user", "content": "Hello!"}]
)
print(response.choices[0].message.content)

# Streaming
for chunk in client.chat.completions.create(
    model="gemma2:2b",
    messages=[{"role": "user", "content": "Tell me a story."}],
    stream=True
):
    print(chunk.choices[0].delta.content or "", end="")
```

---

## Embeddings

`POST /v1/embeddings`

```bash
curl http://localhost:8080/v1/embeddings \
  -H "Authorization: Bearer nxs_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "bge-m3",
    "input": "The quick brown fox"
  }'
```

Batch input:
```json
{
  "model": "bge-m3",
  "input": ["sentence one", "sentence two", "sentence three"]
}
```

**Python:**
```python
response = client.embeddings.create(
    model="bge-m3",
    input=["Hello world", "How are you?"]
)
vectors = [item.embedding for item in response.data]
```

---

## Reranking

`POST /v1/rerank`

Compatible with Cohere and Jina rerank API format.

```bash
curl http://localhost:8080/v1/rerank \
  -H "Authorization: Bearer nxs_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "reranker",
    "query": "What is machine learning?",
    "documents": [
      "Machine learning is a type of AI.",
      "The weather is nice today.",
      "Deep learning uses neural networks."
    ],
    "top_n": 2
  }'
```

Response:
```json
{
  "model": "reranker",
  "results": [
    {"index": 0, "relevance_score": 0.95},
    {"index": 2, "relevance_score": 0.87}
  ]
}
```

---

## Audio transcription (STT)

`POST /v1/audio/transcriptions`

Multipart form upload, identical to OpenAI's transcriptions API.

```bash
curl http://localhost:8080/v1/audio/transcriptions \
  -H "Authorization: Bearer nxs_..." \
  -F "model=whisper" \
  -F "file=@recording.mp3" \
  -F "language=en"
```

Response:
```json
{"text": "Hello, this is a test recording."}
```

**Python:**
```python
with open("audio.mp3", "rb") as f:
    transcript = client.audio.transcriptions.create(
        model="whisper",
        file=f,
        language="en"
    )
print(transcript.text)
```

---

## Text to speech (TTS)

`POST /v1/audio/speech`

Returns binary audio data (MP3 by default).

```bash
curl http://localhost:8080/v1/audio/speech \
  -H "Authorization: Bearer nxs_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model":           "tts",
    "input":           "Hello from NexusLLM!",
    "voice":           "af_heart",
    "response_format": "mp3"
  }' \
  --output output.mp3
```

**Python:**
```python
response = client.audio.speech.create(
    model="tts",
    input="Hello from NexusLLM!",
    voice="af_heart"
)
response.stream_to_file("output.mp3")
```

---

## OCR

`POST /v1/ocr`

```bash
curl http://localhost:8080/v1/ocr \
  -H "Authorization: Bearer nxs_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model":     "ocr",
    "image_url": "https://example.com/document.png",
    "language":  "en"
  }'
```

Or with base64:
```json
{
  "model":     "ocr",
  "image_url": "data:image/png;base64,iVBORw0KGgo..."
}
```

Response:
```json
{
  "text":  "The extracted text from the image...",
  "model": "ocr"
}
```

---

## List available models

`GET /v1/models`

Returns models the API key's team has permission to use:

```bash
curl http://localhost:8080/v1/models \
  -H "Authorization: Bearer nxs_..."
```

```json
{
  "object": "list",
  "data": [
    {"id": "gemma2:2b",  "object": "model", "created": 1782000000, "owned_by": "nexusllm"},
    {"id": "phi3:mini",  "object": "model", "created": 1782000000, "owned_by": "nexusllm"},
    {"id": "bge-m3",     "object": "model", "created": 1782000000, "owned_by": "nexusllm"}
  ]
}
```

---

## Error responses

All errors follow OpenAI's error format:

```json
{
  "error": {
    "message": "Request rejected by policy engine",
    "type":    "gateway_error",
    "code":    "rate_limit_exceeded"
  }
}
```

| HTTP status | Code | Meaning |
|---|---|---|
| 401 | `missing_auth` | No Authorization header |
| 401 | `invalid_token` | Key is invalid or expired |
| 403 | `model_not_allowed` | Team doesn't have permission |
| 403 | `context_too_long` | Prompt exceeds `max_context_tokens` |
| 429 | `rate_limit_exceeded` | Too many requests per minute |
| 429 | `daily_quota_exceeded` | Daily token limit reached |
| 503 | `no_healthy_endpoint` | All backends are down |
| 502 | `upstream_error` | Backend returned an error |

---

## Response headers

Every successful inference response includes:

| Header | Value |
|---|---|
| `X-Nexus-Request-ID` | Unique request ID (UUID) |
| `X-Nexus-Team-ID` | The team that made the request |
| `X-Nexus-Model` | The resolved model name (after alias) |
| `X-Nexus-Endpoint` | The endpoint ID that served the request |

---

## Model aliases

Your team or organization may have aliases configured. For example, `gpt-4o` might route to `qwen3-32b`. The alias is resolved transparently — your code doesn't need to change. See [Model Aliases](13-aliases.md).
