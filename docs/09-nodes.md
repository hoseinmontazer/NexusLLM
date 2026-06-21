# Cluster Nodes & Node Agent

NexusLLM is multi-server ready. All servers are registered in the `nodes` table and the placement engine schedules across them. Currently optimized for single-server deployment, easily expandable.

---

## Default node

Migration 006 automatically seeds a node named `nexus-h200-01` for the H200 server:

```sql
hostname:     nexus-h200-01
total_cpu:    384
total_ram_mb: 1048576   (1 TB)
total_vram_mb: 294912   (288 GB = 2× 144 GB H200)
labels:       {"tier":"primary","gpu":"h200"}
```

Check it:
```bash
curl http://localhost:8081/admin/v1/nodes
```

---

## Register a new node (multi-server expansion)

```bash
curl -X POST http://localhost:8081/admin/v1/nodes \
  -H "Content-Type: application/json" \
  -d '{
    "hostname":      "nexus-a100-02",
    "display_name":  "Secondary AI Server",
    "total_cpu":     128,
    "total_ram_mb":  524288,
    "total_vram_mb": 163840,
    "labels": {
      "tier":    "secondary",
      "gpu":     "a100",
      "region":  "us-east"
    }
  }'
```

Once registered, the placement engine considers this node when `auto_place: true` is used.

---

## Node labels

Labels are arbitrary key-value tags for organizing nodes. The placement engine can use them for preferred/required placement:

```bash
curl -X PUT http://localhost:8081/admin/v1/nodes/NODE_ID/labels \
  -H "Content-Type: application/json" \
  -d '{"labels": {"tier": "primary", "gpu": "h200", "maintenance": "false"}}'
```

---

## Node Agent

The Node Agent collects hardware metrics and reports them to the control plane. On single-server deployments it runs **in-process** inside `nexus-admin` automatically.

### What it collects

**CPU metrics** (from `/proc/stat`, `/proc/meminfo`):
- CPU utilization (rolling 200ms delta)
- RAM total / used / available
- Disk usage
- NUMA node count

**GPU metrics** (via `nvidia-smi`):
- GPU utilization %
- VRAM used / total
- Temperature (°C)
- Power draw (W) and power limit
- Fan speed %
- PCIe bus ID (for NUMA affinity)

**Inventory** (on startup and periodically):
- CPU model and core count
- Full GPU specs
- OS and kernel version

### Collection interval

Default: every 30 seconds. Configured by the in-process agent constructor in `cmd/admin/main.go`.

### Where data goes

| Data | Table | Purpose |
|---|---|---|
| CPU/RAM/disk snapshots | `node_telemetry` | Usage trends, capacity planning |
| GPU snapshots | `gpu_telemetry` | Temperature alerts, power monitoring |
| Full inventory | `node_inventory_snapshots` | Audit trail, hardware discovery |
| Node status | `nodes.status`, `nodes.last_heartbeat_at` | Liveness monitoring |

---

## Node API

### Get node details + latest telemetry

```bash
curl http://localhost:8081/admin/v1/nodes/NODE_ID
```

```json
{
  "node": {
    "id":               "uuid-...",
    "hostname":         "nexus-h200-01",
    "total_cpu":        384,
    "total_ram_mb":     1048576,
    "total_vram_mb":    294912,
    "status":           "online",
    "agent_version":    "1.0.0",
    "last_heartbeat_at": "2026-06-21T10:05:00Z"
  },
  "telemetry": {
    "cpu_util_pct": 12.4,
    "ram_used_mb":  204800,
    "ram_avail_mb": 843776,
    "recorded_at":  "2026-06-21T10:04:55Z"
  }
}
```

### Get telemetry history (last 60 snapshots)

```bash
curl http://localhost:8081/admin/v1/nodes/NODE_ID/telemetry
```

### Get latest hardware inventory

```bash
curl http://localhost:8081/admin/v1/nodes/NODE_ID/inventory
```

Returns a JSON snapshot with CPU model, core count, GPU specs, OS info.

---

## Heartbeat protocol

The node agent sends a heartbeat every 30 seconds:

```
POST /admin/v1/nodes/NODE_ID/heartbeat
{
  "agent_version": "1.0.0",
  "status":        "online"
}
```

If `last_heartbeat_at` is more than 2 minutes old, you can consider the node agent unavailable. The placement engine still uses the node (it doesn't evict based on heartbeat age) — this is a monitoring signal, not an exclusion signal.

---

## Inventory push protocol

On startup and when hardware changes:

```
POST /admin/v1/nodes/NODE_ID/inventory
{
  "hostname":      "nexus-h200-01",
  "agent_version": "1.0.0",
  "cpu_model":     "Intel Xeon Platinum 8480+",
  "cpu_cores":     384,
  "ram_total_mb":  1048576,
  "numa_nodes":    2,
  "os":            "linux/amd64",
  "kernel":        "6.8.0-52-generic",
  "gpus": [
    {"index": 0, "name": "NVIDIA H200 NVL", "vram_mb": 147456, "numa_node": 0, "pcie_bus_id": "0000:01:00.0"},
    {"index": 1, "name": "NVIDIA H200 NVL", "vram_mb": 147456, "numa_node": 1, "pcie_bus_id": "0000:81:00.0"}
  ]
}
```

The admin server updates `nodes.total_cpu`, `nodes.total_ram_mb`, and stores the full snapshot.

---

## Standalone node agent (multi-server)

For future multi-server deployments, run the node agent on each server:

```bash
# On each remote server, pointing at the central admin:
NODE_ID=<registered-node-uuid> \
NEXUS_ADMIN_URL=http://nexus-control:8081 \
./bin/nexus-nodeagent
```

> The standalone `cmd/nodeagent` binary is on the roadmap (v0.8). Currently the agent runs in-process within `nexus-admin`.
