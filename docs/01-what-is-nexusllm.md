# What is NexusLLM?

NexusLLM is a self-hosted **AI Resource Orchestrator** — a platform that sits between your teams and your AI infrastructure.

Instead of every team connecting directly to vLLM, Ollama, or Whisper servers, they connect to NexusLLM's unified API. NexusLLM handles routing, authentication, rate limiting, usage tracking, and model lifecycle management.

---

## What it solves

| Problem | NexusLLM solution |
|---|---|
| Multiple teams sharing GPU resources with no control | Per-team rate limits, quotas, and model permissions |
| Models scattered across servers, no single endpoint | Unified OpenAI-compatible API for every AI service |
| No visibility into who uses what | Per-team/org usage tracking and cost estimation |
| Manual model deployment and health monitoring | Automated container lifecycle, health watcher, circuit breaker |
| Different APIs for chat, embeddings, STT, TTS, OCR | Single `/v1/*` endpoint for all service types |
| Hard to know if a model is healthy | Real-time health dashboard with watcher + circuit breaker |

---

## Service types supported

| Type | Examples | Gateway endpoint |
|---|---|---|
| `CHAT` | vLLM, Ollama, TGI, any OpenAI-compatible LLM | `POST /v1/chat/completions` |
| `EMBEDDING` | Infinity, TEI, FastEmbed | `POST /v1/embeddings` |
| `RERANK` | TEI rerank, Cohere-compat | `POST /v1/rerank` |
| `STT` | faster-whisper-server, whisper.cpp | `POST /v1/audio/transcriptions` |
| `TTS` | Kokoro TTS, Coqui TTS | `POST /v1/audio/speech` |
| `OCR` | EasyOCR REST, Tesseract API | `POST /v1/ocr` |
| `AGENT` | LangChain serve, custom agents | `POST /v1/chat/completions` |
| `MCP` | MCP HTTP bridges | `POST /v1/chat/completions` |

---

## Three binaries

| Binary | Port | Purpose |
|---|---|---|
| `nexus-gateway` | 8080 | Inference API — the endpoint your apps call |
| `nexus-admin` | 8081 | Management API — deploy models, manage teams |
| `nexus-scheduler` | — | Priority queue dispatcher for GPU capacity management |

The **Web Admin UI** (port 3001) is a Next.js frontend that calls the Admin API.

---

## Hardware targets

NexusLLM is designed for:
- **Dev/local:** CPU machine with Ollama — no GPU needed
- **Production:** Bare-metal GPU servers (tested on 2× NVIDIA H200 NVL, 288 GB VRAM, 384 vCPUs, 1 TB RAM)
- **Future:** Multi-server clusters — add nodes by inserting rows into the `nodes` table, no code changes
