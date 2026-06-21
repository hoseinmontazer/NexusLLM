# Model Registry — LLM Models

This page covers CHAT models (LLMs). For other service types (embeddings, STT, TTS, OCR), see [AI Service Registry](07-ai-services.md).

---

## Three ways to add a model

### 1. Import from Ollama (easiest)

If you have Ollama running locally with models already pulled, import them all at once:

```bash
curl -X POST http://localhost:8081/admin/v1/models/import-ollama \
  -H "Content-Type: application/json" \
  -d '{"host": "localhost", "port": 11434}'
```

Response:
```json
{
  "results": [
    {"name": "gemma2:2b",         "status": "registered", "model_id": "uuid-..."},
    {"name": "phi3:mini",         "status": "registered", "model_id": "uuid-..."},
    {"name": "qwen2.5-coder:7b",  "status": "already_registered"}
  ]
}
```

Or use the web UI: **Models → Import from Ollama**.

After import, grant the model to a team:
```bash
curl -X POST http://localhost:8081/admin/v1/teams/TEAM_ID/models \
  -H "Content-Type: application/json" \
  -d '{"model_name": "gemma2:2b"}'
```

---

### 2. Register an external model (already running)

Use this for any model that's already running somewhere (Ollama, TGI, an existing vLLM instance, a remote API):

```bash
curl -X POST http://localhost:8081/admin/v1/models \
  -H "Content-Type: application/json" \
  -d '{
    "name":         "my-llm",
    "display_name": "My LLM",
    "backend_type": "ollama",
    "host":         "localhost",
    "port":         11434
  }'
```

Supported `backend_type` values:

| Value | Description |
|---|---|
| `ollama` | Ollama server — health via `GET /`, models via `/api/tags` |
| `vllm` | vLLM server — health via `GET /health` + `/metrics` scrape |
| `tgi` | HuggingFace TGI — health via `GET /health` |
| `openai_compat` | Any OpenAI-compatible API — health via `GET /v1/models` |

---

### 3. Deploy vLLM via Docker (NexusLLM manages the container)

**Requires:** NVIDIA GPU + Docker + `nvidia-container-toolkit`.

```bash
curl -X POST http://localhost:8081/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name":            "llama3-8b",
    "display_name":    "LLaMA 3 8B",
    "backend_type":    "vllm",
    "image":           "vllm/vllm-openai:v0.4.3",
    "hf_model_id":     "meta-llama/Meta-Llama-3-8B-Instruct",
    "host":            "localhost",
    "port":            8000,
    "gpu_devices":     [0],
    "tensor_parallel": 1,
    "gpu_memory_util": 0.90,
    "max_model_len":   32768,
    "dtype":           "bfloat16",
    "hf_token":        "hf_...",
    "start_now":       true
  }'
```

#### Deploy with auto-placement (recommended)

Instead of specifying `gpu_devices` manually, let the placement engine decide:

```bash
curl -X POST http://localhost:8081/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name":         "qwen3-32b",
    "display_name": "Qwen3 32B",
    "backend_type": "vllm",
    "image":        "vllm/vllm-openai:latest",
    "hf_model_id":  "Qwen/Qwen3-32B-Instruct",
    "host":         "localhost",
    "port":         8010,
    "auto_place":   true,
    "min_vram_mb":  65536,
    "max_vram_mb":  122880,
    "priority":     "critical",
    "start_now":    true
  }'
```

The engine scores available GPUs by free VRAM, utilization, temperature, and NUMA locality.

#### Multi-GPU (tensor parallel)

For models that need multiple GPUs:
```json
{
  "gpu_devices":     [0, 1],
  "tensor_parallel": 2,
  "min_vram_mb":     144000
}
```

---

## Model lifecycle states

After deployment, a model's endpoint goes through these states:

```
registered → downloading → loading → active/warm → idle → unloaded
                 ↓                        ↓
               failed                   failed
```

| State | Meaning |
|---|---|
| `registered` | Recorded in DB, container not started |
| `downloading` | Container starting, pulling weights |
| `loading` | Container running, model loading into VRAM |
| `active` | Ready to serve requests |
| `warm` | Ready but lower traffic recently |
| `idle` | No requests for >30 min (gateway), may be evicted |
| `unloaded` | Container stopped |
| `failed` | Container crashed or health checks all failed |
| `draining` | Accepting no new requests, finishing in-flight |

---

## Managing models

### List all models

```bash
curl http://localhost:8081/admin/v1/models
```

### Check health

```bash
curl http://localhost:8081/admin/v1/models/MODEL_ID/health
```

### Reset a failed endpoint

If an endpoint is stuck in `failed` or `unknown` state (e.g., after restarting the service):

```bash
curl -X POST http://localhost:8081/admin/v1/models/MODEL_ID/reset-health
```

This sets `health_status='unknown'`, `lifecycle_state='active'`, `consecutive_failures=0` — the watcher will re-evaluate within 5 seconds.

### Enable / disable

```bash
curl -X POST http://localhost:8081/admin/v1/models/MODEL_ID/enable
curl -X POST http://localhost:8081/admin/v1/models/MODEL_ID/disable
```

Disabled models are removed from routing immediately. Existing in-flight requests are not affected.

### Start / stop / restart (Docker-managed only)

```bash
# endpoint_id is from the health check response
curl -X POST "http://localhost:8081/admin/v1/models/MODEL_ID/start?endpoint_id=EP_ID"
curl -X POST "http://localhost:8081/admin/v1/models/MODEL_ID/stop?endpoint_id=EP_ID"
curl -X POST "http://localhost:8081/admin/v1/models/MODEL_ID/restart?endpoint_id=EP_ID"
```

### Upgrade to a new Docker image

```bash
curl -X POST "http://localhost:8081/admin/v1/models/MODEL_ID/upgrade?endpoint_id=EP_ID" \
  -H "Content-Type: application/json" \
  -d '{"image": "vllm/vllm-openai:v0.5.0"}'
```

### Rollback

```bash
curl -X POST "http://localhost:8081/admin/v1/models/MODEL_ID/rollback?endpoint_id=EP_ID" \
  -H "Content-Type: application/json" \
  -d '{"previous_image": "vllm/vllm-openai:v0.4.3"}'
```

### View container logs

```bash
curl "http://localhost:8081/admin/v1/models/MODEL_ID/logs?endpoint_id=EP_ID"
```

### Delete a model

```bash
curl -X DELETE http://localhost:8081/admin/v1/models/MODEL_ID
```

> Stop the container first. Delete removes the DB row but does not stop a running container.

---

## Health watcher

The gateway runs a background watcher every 5 seconds that:
1. Calls the appropriate health endpoint for each backend
2. Applies a circuit breaker (3 failures → `down`)
3. Updates `model_endpoints.health_status` in PostgreSQL
4. Updates the in-memory `Registry` and Redis for instant routing decisions
5. Records latency and error history in `endpoint_health_log`

**Circuit breaker:** A single failure produces `degraded` status (still routes). Only 3 consecutive failures produce `down` and remove the endpoint from routing.

---

## Troubleshooting

### Model shows `unknown` health

The watcher hasn't checked it yet, or the gateway hasn't reloaded. Wait 5–10 seconds, or call reset-health.

### Model shows `failed`

The backend isn't reachable. Check:
1. Is the container/process running? (`docker ps`)
2. Is the port correct? (`curl http://localhost:PORT/`)
3. For Ollama: `ollama list` — is the model pulled?
4. Call reset-health to clear the failed state

### vLLM container status is `Created` but not `Up`

Your machine likely has no GPU or the NVIDIA Container Runtime isn't installed. Error: `could not select device driver "" with capabilities: [[gpu]]`.

Fix: Install `nvidia-container-toolkit` or use Ollama for CPU-only dev.

### `model_not_allowed` error

The team doesn't have permission for this model. See [Teams → Model permissions](04-orgs-and-teams.md#model-permissions).
