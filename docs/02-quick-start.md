# Quick Start

Two paths: **local dev with Ollama** (no GPU needed) or **GPU server with vLLM**.

---

## Path A — Local dev with Ollama (5 minutes)

### Prerequisites
- Docker + Docker Compose
- Go 1.22+
- Node.js 18+ (for the web UI)
- [Ollama](https://ollama.ai) installed and running

### Step 1 — Pull a model into Ollama

```bash
ollama pull gemma2:2b
# or any model you like:
# ollama pull phi3:mini
# ollama pull qwen2.5-coder:7b
```

Verify Ollama is serving: `curl http://localhost:11434/` should return `Ollama is running`.

### Step 2 — Start the infrastructure

```bash
git clone <repo>
cd nexusllm

make dev-up          # starts postgres + redis, runs all migrations
```

### Step 3 — Start the three services (3 terminals)

```bash
# Terminal 1
make run-gateway     # inference API → http://localhost:8080

# Terminal 2
make run-admin       # management API → http://localhost:8081

# Terminal 3
make run-scheduler   # queue dispatcher
```

### Step 4 — Start the web UI

```bash
make web-install     # first time only
make run-web         # → http://localhost:3001
```

### Step 5 — Import your Ollama models

Open http://localhost:3001/models, click **Import from Ollama**. All models from `ollama list` are registered in one click.

### Step 6 — Create a team and get an API key

1. Go to http://localhost:3001/teams → **Create Team**
2. Click the team → **API Keys** → **Create Key**
3. Copy the key (shown only once, starts with `nxs_`)
4. Go back to the team → **Models** → grant permission for your model

### Step 7 — Make your first request

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer nxs_YOUR_KEY_HERE" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemma2:2b",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

---

## Path B — GPU server with vLLM

### Prerequisites
- NVIDIA GPU with drivers installed
- `nvidia-container-toolkit` installed (for Docker GPU access)
- Docker + Docker Compose
- Go 1.22+

### Step 1 — Set up infrastructure

```bash
make dev-up
```

### Step 2 — Deploy a vLLM model

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
    "hf_token":        "hf_YOUR_TOKEN",
    "start_now":       true
  }'
```

Or use the web UI: **Models → Deploy vLLM Model**.

### Step 3 — Wait for the model to load

The container starts in `loading` state. Check status:

```bash
curl http://localhost:8081/admin/v1/models/MODEL_ID/health
```

When `health_status` becomes `healthy`, the model is ready.

### Step 4 — Create team and API key (same as Path A steps 6–7)

---

## Auto-placement (optional, GPU path only)

Instead of manually specifying `gpu_devices`, let the placement engine decide:

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
    "priority":     "critical",
    "start_now":    true
  }'
```

The engine picks the GPU with the most headroom, lowest temperature, and correct NUMA affinity.

---

## Verifying everything works

```bash
# Gateway alive?
curl http://localhost:8080/healthz
# → {"status":"ok"}

# Which models are loaded?
curl http://localhost:8080/readyz
# → {"status":"ready","models":["gemma2:2b","phi3:mini",...]}

# Admin alive?
curl http://localhost:8081/healthz
# → {"status":"ok"}
```
