# GPU Inventory

The GPU inventory tracks physical GPU devices, their VRAM, utilization, temperature, and allocations.

---

## How GPU nodes relate to cluster nodes

There are two levels:
- **`nodes`** — the physical server (CPU + RAM + GPU totals)
- **`gpu_nodes`** — the GPU subsystem on a server (linked to `nodes.id`)
- **`gpu_devices`** — individual GPU cards under a `gpu_node`

Migration 006 seeds:
- 1 `nodes` row: `nexus-h200-01`
- 1 `gpu_nodes` row: `h200-node-0` linked to it
- 2 `gpu_devices` rows: GPU 0 and GPU 1 (both H200 NVL 144 GB)

---

## Register a GPU node

```bash
curl -X POST http://localhost:8081/admin/v1/gpu/nodes \
  -H "Content-Type: application/json" \
  -d '{
    "name":        "h200-server-2",
    "host":        "10.0.0.2",
    "driver_type": "docker"
  }'
```

`driver_type` is always `docker` currently. Future: `kubernetes`.

---

## Register GPU devices

```bash
# GPU 0 — H200 NVL 144 GB
curl -X POST http://localhost:8081/admin/v1/gpu/nodes/NODE_ID/devices \
  -H "Content-Type: application/json" \
  -d '{"device_index": 0, "name": "NVIDIA H200 NVL", "vram_mb": 147456}'

# GPU 1 — H200 NVL 144 GB
curl -X POST http://localhost:8081/admin/v1/gpu/nodes/NODE_ID/devices \
  -H "Content-Type: application/json" \
  -d '{"device_index": 1, "name": "NVIDIA H200 NVL", "vram_mb": 147456}'
```

---

## List nodes and devices

```bash
curl http://localhost:8081/admin/v1/gpu/nodes
curl http://localhost:8081/admin/v1/gpu/nodes/NODE_ID/devices
```

Device fields:

| Field | Description |
|---|---|
| `device_index` | NVIDIA device index (0, 1, ...) — used in `--gpus device=N` |
| `vram_mb` | Total VRAM in MB (147456 = 144 GB for H200 NVL) |
| `status` | `available` or `allocated` |
| `utilization_pct` | Current GPU utilization (updated by node agent) |
| `temperature_c` | Current temperature in Celsius |
| `power_draw_w` | Current power consumption in Watts |
| `numa_node` | NUMA node the GPU is attached to (0 or 1) |

---

## GPU allocation

When a vLLM model is deployed via NexusLLM, GPU devices are marked `allocated`:

```sql
gpu_allocations:
  endpoint_id     → model endpoint that owns this GPU
  gpu_device_id   → the GPU device row
  vram_allocated_mb
  released_at     → NULL while allocated, timestamp when freed
```

When a model is stopped or deleted, allocations are released:
```bash
# Stop triggers release
curl -X POST "http://localhost:8081/admin/v1/models/MODEL_ID/stop?endpoint_id=EP_ID"
```

---

## GPU packing simulation

Simulate how multiple models would be packed onto available GPUs using the FFD (First-Fit Decreasing) algorithm:

```bash
curl -X POST http://localhost:8081/admin/v1/gpu/pack \
  -H "Content-Type: application/json" \
  -d '{
    "node_id": "GPU_NODE_ID",
    "models": [
      {"model_name": "llama-70b",        "required_vram_mb": 140000, "gpu_count": 1},
      {"model_name": "qwen3-32b",        "required_vram_mb": 65536,  "gpu_count": 1},
      {"model_name": "deepseek-coder-7b","required_vram_mb": 16000,  "gpu_count": 1}
    ]
  }'
```

Response:
```json
{
  "assignments": {
    "llama-70b":         [1],
    "qwen3-32b":         [0],
    "deepseek-coder-7b": [0]
  },
  "unscheduled": [],
  "explanation": "GPU Packing Plan:\n  llama-70b → GPU [1]\n  qwen3-32b → GPU [0]\n  deepseek-coder-7b → GPU [0]"
}
```

The packing algorithm:
1. Sorts models largest-first (FFD heuristic)
2. Tries to place models in contiguous GPU blocks (better NVLink bandwidth)
3. Falls back to non-contiguous if needed
4. Returns unscheduled models if not enough VRAM

---

## GPU telemetry (from node agent)

The node agent polls `nvidia-smi` every 30 seconds and writes to:
- `gpu_devices.utilization_pct`, `temperature_c`, `power_draw_w` (current state)
- `gpu_telemetry` table (time-series history)

Query recent GPU history:
```sql
SELECT gd.name, gt.utilization_pct, gt.temperature_c, gt.power_draw_w, gt.recorded_at
FROM gpu_telemetry gt
JOIN gpu_devices gd ON gd.id = gt.device_id
ORDER BY gt.recorded_at DESC
LIMIT 20;
```
