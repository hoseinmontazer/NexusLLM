# AI Service Registry

The AI Service Registry extends the model registry to support non-LLM AI services: embeddings, rerankers, speech-to-text, text-to-speech, OCR, agent runtimes, and MCP servers.

---

## Service types

| `service_type` | Description | Default runtime | Gateway endpoint |
|---|---|---|---|
| `CHAT` | LLM chat completion | GPU | `POST /v1/chat/completions` |
| `EMBEDDING` | Text embedding models | CPU | `POST /v1/embeddings` |
| `RERANK` | Cross-encoder rerankers | CPU | `POST /v1/rerank` |
| `STT` | Speech-to-text / transcription | CPU | `POST /v1/audio/transcriptions` |
| `TTS` | Text-to-speech | CPU | `POST /v1/audio/speech` |
| `OCR` | Optical character recognition | CPU | `POST /v1/ocr` |
| `AGENT` | Agent runtimes | CPU/GPU | `POST /v1/chat/completions` |
| `MCP` | Model Context Protocol servers | CPU | `POST /v1/chat/completions` |

---

## Runtime types

| `runtime_type` | Description |
|---|---|
| `GPU_RUNTIME` | Runs on GPU (vLLM, TGI, CUDA-accelerated) |
| `CPU_RUNTIME` | Runs on CPU (embeddings, STT, TTS, OCR, MCP) |

---

## Register an already-running service

```bash
curl -X POST http://localhost:8081/admin/v1/services \
  -H "Content-Type: application/json" \
  -d '{
    "name":         "bge-m3",
    "display_name": "BGE-M3 Embeddings",
    "service_type": "EMBEDDING",
    "runtime_type": "CPU_RUNTIME",
    "host":         "localhost",
    "port":         7997,
    "cpu_cores":    32,
    "ram_mb":       65536,
    "numa_node":    0,
    "priority":     "normal"
  }'
```

### Embedding service examples

**Infinity (recommended for production):**
```bash
docker run -d --name infinity \
  --cpuset-cpus "0-31" \
  -p 7997:7997 \
  michaelf34/infinity:latest \
  v2 --model-name-or-path BAAI/bge-m3 --port 7997

curl -X POST http://localhost:8081/admin/v1/services \
  -H "Content-Type: application/json" \
  -d '{"name":"bge-m3","display_name":"BGE-M3","service_type":"EMBEDDING","runtime_type":"CPU_RUNTIME","host":"localhost","port":7997}'
```

**Then use it:**
```bash
curl http://localhost:8080/v1/embeddings \
  -H "Authorization: Bearer nxs_..." \
  -H "Content-Type: application/json" \
  -d '{"model": "bge-m3", "input": "Hello world"}'
```

### STT (Whisper) service example

**faster-whisper-server:**
```bash
docker run -d --name whisper \
  --cpuset-cpus "32-63" \
  -p 8000:8000 \
  fedirz/faster-whisper-server:latest-cpu \
  --model=Systran/faster-whisper-base

curl -X POST http://localhost:8081/admin/v1/services \
  -H "Content-Type: application/json" \
  -d '{"name":"whisper","display_name":"Whisper STT","service_type":"STT","runtime_type":"CPU_RUNTIME","host":"localhost","port":8000}'
```

**Then transcribe audio:**
```bash
curl http://localhost:8080/v1/audio/transcriptions \
  -H "Authorization: Bearer nxs_..." \
  -F "model=whisper" \
  -F "file=@audio.mp3"
```

### Reranker service example

**TEI (Text Embeddings Inference) rerank:**
```bash
docker run -d --name reranker \
  --cpuset-cpus "64-79" \
  -p 8001:80 \
  ghcr.io/huggingface/text-embeddings-inference:cpu-1.5 \
  --model-id BAAI/bge-reranker-v2-m3

curl -X POST http://localhost:8081/admin/v1/services \
  -H "Content-Type: application/json" \
  -d '{"name":"reranker","display_name":"BGE Reranker","service_type":"RERANK","runtime_type":"CPU_RUNTIME","host":"localhost","port":8001}'
```

**Then rerank:**
```bash
curl http://localhost:8080/v1/rerank \
  -H "Authorization: Bearer nxs_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "reranker",
    "query": "What is the capital of France?",
    "documents": ["Paris is the capital.", "London is in England.", "Berlin is in Germany."],
    "top_n": 2
  }'
```

### TTS service example

**Kokoro TTS:**
```bash
docker run -d --name kokoro -p 8880:8880 ghcr.io/remsky/kokoro-fastapi-cpu:latest

curl -X POST http://localhost:8081/admin/v1/services \
  -H "Content-Type: application/json" \
  -d '{"name":"tts","display_name":"Kokoro TTS","service_type":"TTS","runtime_type":"CPU_RUNTIME","host":"localhost","port":8880}'
```

**Then generate speech:**
```bash
curl http://localhost:8080/v1/audio/speech \
  -H "Authorization: Bearer nxs_..." \
  -H "Content-Type: application/json" \
  -d '{"model":"tts","input":"Hello, this is NexusLLM speaking.","voice":"af_heart"}' \
  --output speech.mp3
```

---

## Deploy a service with auto-placement (GPU server)

```bash
curl -X POST http://localhost:8081/admin/v1/services/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name":         "whisper-large",
    "display_name": "Whisper Large v3",
    "service_type": "STT",
    "runtime_type": "CPU_RUNTIME",
    "image":        "fedirz/faster-whisper-server:latest-cpu",
    "host":         "localhost",
    "port":         9000,
    "cpu_cores":    32,
    "numa_node":    1,
    "ram_mb":       16384,
    "priority":     "normal",
    "start_now":    true
  }'
```

The placement engine will:
1. Find a node with ≥32 free CPU cores and ≥16 GB RAM on NUMA node 1
2. Start the container with `--cpuset-cpus` and `--cpuset-mems` flags
3. Record a CPU allocation in the database

---

## List all services

```bash
# All services
curl http://localhost:8081/admin/v1/services

# Filter by type
curl "http://localhost:8081/admin/v1/services?type=EMBEDDING"
curl "http://localhost:8081/admin/v1/services?type=STT"
```

---

## Resource reservations

Declare the resource envelope for a service. The placement engine uses this when auto-placing.

```bash
curl -X PUT http://localhost:8081/admin/v1/services/MODEL_ID/reservation \
  -H "Content-Type: application/json" \
  -d '{
    "cpu_cores":        32,
    "ram_mb":           65536,
    "numa_node_pref":   0,
    "priority":         "normal",
    "preferred_runtime": "CPU_RUNTIME"
  }'
```

For GPU services:
```bash
curl -X PUT http://localhost:8081/admin/v1/services/MODEL_ID/reservation \
  -H "Content-Type: application/json" \
  -d '{
    "min_vram_mb":      81920,
    "max_vram_mb":      122880,
    "priority":         "critical",
    "preferred_runtime": "GPU_RUNTIME"
  }'
```

View a reservation:
```bash
curl http://localhost:8081/admin/v1/services/MODEL_ID/reservation
```
