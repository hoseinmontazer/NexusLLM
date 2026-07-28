# Model Registry — LLM Models

This page covers CHAT models (LLMs). For other service types (embeddings, STT, TTS, OCR), see [AI Service Registry](07-ai-services.md).

---

## Three ways to add a model

### 1. Register an external model (already running)

Use this for any model that's already running somewhere (TGI, an existing vLLM instance, a remote API):

```bash
curl -X POST http://localhost:8081/admin/v1/models \
  -H "Content-Type: application/json" \
  -d '{
    "name":         "my-llm",
    "display_name": "My LLM",
    "backend_type": "openai_compat",
    "host":         "localhost",
    "port":         8000
  }'
```

Supported `backend_type` values:

| Value | Description |
|---|---|
| `vllm` | vLLM server — health via `GET /health` + `/metrics` scrape |
| `tgi` | HuggingFace TGI — health via `GET /health` |
| `llamacpp` | llama.cpp server — health via `GET /health` |
| `cpu_native` | CPU AI services (faster-whisper, Kokoro, Infinity) — health via `GET /health` |
| `openai_compat` | Any OpenAI-compatible API — health via `GET /v1/models` |

---

### 2. Deploy via Docker (NexusLLM manages the container)

#### vLLM — requires NVIDIA GPU + nvidia-container-toolkit

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
    "port":            0,
    "gpu_devices":     [0],
    "tensor_parallel": 1,
    "gpu_memory_util": 0.90,
    "max_model_len":   32768,
    "dtype":           "bfloat16",
    "hf_token":        "hf_...",
    "start_now":       true
  }'
```

#### llama.cpp — CPU or GPU, no special runtime required

```bash
curl -X POST http://localhost:8081/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name":           "gemma2-2b",
    "display_name":   "Gemma 2 2B",
    "backend_type":   "llamacpp",
    "image":          "ghcr.io/ggml-org/llama.cpp:server",
    "hf_repo":        "bartowski/gemma-2-2b-it-GGUF",
    "hf_file":        "gemma-2-2b-it-Q4_K_M.gguf",
    "host":           "localhost",
    "port":           0,
    "execution_mode": "cpu",
    "start_now":      true
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
    "port":         0,
    "auto_place":   true,
    "min_vram_mb":  65536,
    "max_vram_mb":  122880,
    "priority":     "critical",
    "start_now":    true
  }'
```

The engine scores available GPUs by free VRAM, utilization, temperature, and NUMA locality.

#### Multi-GPU (tensor parallel)

```json
{
  "gpu_devices":     [0, 1],
  "tensor_parallel": 2,
  "min_vram_mb":     144000
}
```

---

### 3. Register a cloud / external API

For OpenAI, Anthropic, Google, or any hosted API — use the **Register Cloud/External** button in the web UI, or:

```bash
curl -X POST http://localhost:8081/admin/v1/models \
  -H "Content-Type: application/json" \
  -d '{
    "name":               "gpt-4o",
    "display_name":       "GPT-4o",
    "backend_type":       "openai_compat",
    "host":               "cloud",
    "port":               443,
    "upstream_api_key":   "sk-...",
    "upstream_base_url":  "https://api.openai.com"
  }'
```

---

## Model lifecycle states

```
registered → downloading → loading → active/warm → idle → unloaded
                 ↓                        ↓
               failed                   failed
```

| State | Meaning |
|---|---|
| `registered` | Recorded in DB, container not started |
| `downloading` | Container starting, pulling weights |
| `loading` | Container running, model loading into VRAM/RAM |
| `active` | Ready to serve requests |
| `warm` | Ready but lower traffic recently |
| `idle` | No requests for >idle timeout, may be evicted |
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

```bash
curl -X POST http://localhost:8081/admin/v1/models/MODEL_ID/reset-health
```

### Enable / disable

```bash
curl -X POST http://localhost:8081/admin/v1/models/MODEL_ID/enable
curl -X POST http://localhost:8081/admin/v1/models/MODEL_ID/disable
```

### Start / stop / restart (Docker-managed only)

```bash
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
2. Is the port correct? (`curl http://localhost:PORT/health`)
3. Call reset-health to clear the failed state

### vLLM container status is `Created` but not `Up`

Your machine likely has no GPU or the NVIDIA Container Runtime isn't installed. Error: `could not select device driver "" with capabilities: [[gpu]]`.

Fix: Install `nvidia-container-toolkit`, or use `llamacpp` with `execution_mode: cpu` for CPU-only dev.

### `model_not_allowed` error

The team doesn't have permission for this model. See [Teams → Model permissions](04-orgs-and-teams.md#model-permissions).
