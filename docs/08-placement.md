# Resource Placement Engine

The placement engine automatically selects the best GPU(s) or CPU resources for a service deployment, based on current hardware state.

---

## How it works

When you deploy with `auto_place: true` or use `POST /services/deploy`, the engine:

1. **Queries all online nodes** from the `nodes` table
2. **Scores each candidate** based on available resources
3. **Picks the best match** (highest score)
4. **Records the decision** in `placement_decisions` for audit

### GPU placement scoring

Each GPU device is scored 0–1000:

| Factor | Weight | Logic |
|---|---|---|
| Low utilization | 0–400 | `(1 - util%) × 400` |
| Low temperature | 0–200 | `(1 - temp/85°C) × 200` |
| Same NUMA node | 0–200 | +200 if all GPUs on same NUMA node |
| VRAM headroom | 0–200 | How close to `max_vram_mb` requested |

Higher score = better choice. GPU 0 at 20% util, 45°C, NUMA 0 beats GPU 1 at 80% util, 78°C, NUMA 1.

### CPU placement scoring

| Factor | Weight | Logic |
|---|---|---|
| Free CPUs | 0–500 | `(free/total) × 500` |
| Free RAM | 0–200 | `(free_ram/total_ram) × 200` |

---

## Simulate before deploying (dry-run)

Check if your desired placement is feasible **without committing any resources**:

```bash
curl -X POST http://localhost:8081/admin/v1/placement/simulate \
  -H "Content-Type: application/json" \
  -d '{
    "model_name":   "deepseek-v3",
    "service_type": "CHAT",
    "runtime_type": "GPU_RUNTIME",
    "min_vram_mb":  144000,
    "gpu_count":    2,
    "priority":     "critical"
  }'
```

**Response when feasible:**
```json
{
  "feasible": true,
  "decision": {
    "node_id":     "uuid-...",
    "node_host":   "nexus-h200-01",
    "gpu_devices": [0, 1],
    "vram_mb":     294912,
    "cpu_cores":   0,
    "numa_node":   0,
    "strategy":    "gpu_score",
    "score":       742.5,
    "reason":      "node=nexus-h200-01 score=742.50"
  }
}
```

**Response when infeasible:**
```json
{
  "feasible": false,
  "error": "placement: insufficient GPU resources (need 2 GPUs × 144000 MB VRAM)"
}
```

The web UI (**Placement** page) provides a visual simulator with quick presets.

---

## Placement decisions audit log

Every placement decision (including dry-runs) is recorded:

```bash
curl http://localhost:8081/admin/v1/placement/decisions
```

```json
{
  "data": [
    {
      "id":          "uuid-...",
      "model_id":    "uuid-...",
      "node_id":     "uuid-...",
      "gpu_devices": "[0]",
      "cpu_cores":   0,
      "numa_node":   0,
      "strategy":    "gpu_score",
      "score":       742.5,
      "reason":      "node=nexus-h200-01 score=742.50",
      "applied":     true,
      "created_at":  "2026-06-21T10:00:00Z"
    }
  ]
}
```

`applied: true` means the deployment actually used this decision.
`applied: false` means it was a dry-run simulation.

---

## H200 recommended placement

For the target 2× H200 NVL (144 GB each, 288 GB total):

```
GPU 0 — NUMA 0 (144 GB)
├── Qwen3-32B          ~65 GB   (tensor_parallel=1, min_vram_mb=65536)
└── DeepSeek-Coder-7B  ~16 GB   (tensor_parallel=1, min_vram_mb=16384)
    Remaining: ~63 GB free

GPU 1 — NUMA 1 (144 GB)
└── Llama-3.3-70B      ~140 GB  (tensor_parallel=1, fp8 quant, min_vram_mb=136000)
    Remaining: ~4 GB headroom

CPU Pool (384 vCPUs, 1 TB RAM)
NUMA 0 (cores 0-191):
├── Embedding service  32 cores, 64 GB
├── Reranker           16 cores, 32 GB
└── MCP bridges         8 cores,  8 GB

NUMA 1 (cores 192-383):
├── Whisper STT        32 cores, 16 GB
├── Kokoro TTS         16 cores,  8 GB
├── OCR service        16 cores, 16 GB
└── Agent runtimes     64 cores, 128 GB
```

---

## NUMA topology

NUMA (Non-Uniform Memory Access) awareness is important for performance:
- GPU 0 is typically on NUMA node 0 — CPU cores 0–191 access it faster
- GPU 1 is typically on NUMA node 1 — CPU cores 192–383 access it faster

The placement engine:
1. Reads `numa_node` from `gpu_devices.numa_node` (populated by node agent via `nvidia-smi` + sysfs)
2. Picks CPUs on the same NUMA node as the GPU when both are needed
3. Passes `--cpuset-cpus` and `--cpuset-mems` to Docker for CPU-only services

---

## Manual placement (override)

You can always override auto-placement by specifying `gpu_devices` directly:

```bash
curl -X POST http://localhost:8081/admin/v1/models/deploy \
  -d '{
    ...
    "auto_place":      false,
    "gpu_devices":     [1],
    "tensor_parallel": 1
  }'
```
