# Cluster Nodes & Node Agent

NexusLLM is multi-server ready. All servers are registered in the `nodes` table and the placement engine schedules across them. The **Node Agent** (`nexus-nodeagent`) is a standalone binary that runs on each server and keeps the control plane up to date automatically — no manual data entry needed.

---

## How it works

```
Each server runs:
  nexus-nodeagent
      │
      │  HTTP → POST /admin/v1/nodes/:id/heartbeat     (every 15s)
      │          POST /admin/v1/nodes/:id/inventory     (on startup + every 30s)
      │          POST /admin/v1/nodes/:id/telemetry     (on startup + every 30s)
      │          POST /admin/v1/gpu/nodes               (creates GPU node if missing)
      │          POST /admin/v1/gpu/nodes/:id/devices   (registers GPU devices)
      ▼
  nexus-admin (control plane)
      │
      ▼
  PostgreSQL  →  Web UI shows live data
```

On **first run**, the agent:
1. Reads `/proc/cpuinfo`, `/proc/meminfo`, `nvidia-smi` to discover hardware
2. Calls `POST /admin/v1/nodes` to register itself — **no manual step needed**
3. Saves the assigned node ID to `/var/lib/nexus-agent/node-id` for restarts
4. Creates a `gpu_node` and registers each GPU device

On **every tick** (default: 30s), the agent pushes fresh CPU utilization, RAM usage, disk, and GPU metrics.

---

## Running the node agent

### Single-server (same machine as admin)

```bash
# Terminal 5 (or add to systemd)
make run-nodeagent
# equivalent to:
NEXUS_ADMIN_URL="http://localhost:8081" \
NEXUS_AGENT_INTERVAL="30s" \
NEXUS_HEARTBEAT_INTERVAL="15s" \
./bin/nexus-nodeagent
```

That's it. The agent auto-registers and starts pushing data immediately.

### Remote server (multi-node cluster)

Copy the binary to the remote server and run:

```bash
# On the remote server — point at the central admin
NEXUS_ADMIN_URL="http://10.0.0.1:8081" \
NEXUS_AGENT_INTERVAL="30s" \
NEXUS_HEARTBEAT_INTERVAL="15s" \
./nexus-nodeagent
```

The remote server appears in the Nodes page within seconds.

### As a systemd service (production)

```ini
# /etc/systemd/system/nexus-nodeagent.service
[Unit]
Description=NexusLLM Node Agent
After=network.target

[Service]
ExecStart=/usr/local/bin/nexus-nodeagent
Restart=always
RestartSec=10
Environment=NEXUS_ADMIN_URL=http://10.0.0.1:8081
Environment=NEXUS_AGENT_INTERVAL=30s
Environment=NEXUS_HEARTBEAT_INTERVAL=15s

[Install]
WantedBy=multi-user.target
```

```bash
systemctl enable --now nexus-nodeagent
```

---

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `NEXUS_ADMIN_URL` | `http://localhost:8081` | URL of the nexus-admin control plane |
| `NEXUS_NODE_ID` | _(auto)_ | Skip auto-registration, use this node ID |
| `NEXUS_AGENT_INTERVAL` | `30s` | How often to push telemetry + inventory |
| `NEXUS_HEARTBEAT_INTERVAL` | `15s` | How often to send heartbeat |

Node ID is persisted in `/var/lib/nexus-agent/node-id` after first registration.

---

## What is collected

| Data | How | Pushed to |
|---|---|---|
| CPU utilization | `/proc/stat` delta (200ms) | `node_telemetry` table |
| RAM total / used / available | `/proc/meminfo` | `node_telemetry` + `nodes.total_ram_mb` |
| Disk usage | `df -BG /` | `node_telemetry` |
| NUMA node count | `numactl --hardware` | `node_telemetry` |
| GPU utilization, VRAM, temp, power, fan | `nvidia-smi` | `gpu_devices` + `gpu_telemetry` |
| GPU PCIe bus ID → NUMA affinity | `/sys/bus/pci/devices/*/numa_node` | `gpu_devices.numa_node` |
| Full inventory snapshot | All of the above | `node_inventory_snapshots` |
| CPU model, kernel version, OS | `/proc/cpuinfo`, `uname -r` | inventory snapshot |

---

## Register a new node manually (optional)

The agent auto-registers on first run, but you can pre-register a node:

```bash
curl -X POST http://localhost:8081/admin/v1/nodes \
  -H "Content-Type: application/json" \
  -d '{
    "hostname":      "nexus-h200-02",
    "display_name":  "Secondary AI Server",
    "total_cpu":     384,
    "total_ram_mb":  1048576,
    "labels": {"tier": "gpu", "gpu": "h200"}
  }'
```

Then run the agent on that server with `NEXUS_NODE_ID=<uuid>` to skip re-registration.

---

## Labels

Labels are arbitrary key-value tags set on registration or updated via API:

```bash
curl -X PUT http://localhost:8081/admin/v1/nodes/NODE_ID/labels \
  -H "Content-Type: application/json" \
  -d '{"labels": {"tier":"primary","gpu":"h200","rack":"A1"}}'
```

The placement engine can use labels for preferred/required placement in future versions.

---

## Default node (H200 server)

Migration 006 seeds a `nexus-h200-01` node. Run the agent on the H200 server to start populating it with real data:

```bash
NEXUS_ADMIN_URL="http://<admin-ip>:8081" ./bin/nexus-nodeagent
```

The agent will:
- Detect the H200 server already exists (by hostname lookup)
- Reuse the existing node ID
- Start pushing real GPU telemetry from `nvidia-smi`

---

## Troubleshooting

**Agent registers a new node instead of finding the existing one:**
The hostname on the server doesn't match `nexus-h200-01`. Either rename the machine or pre-set `NEXUS_NODE_ID`:
```bash
export NEXUS_NODE_ID=$(curl -s http://localhost:8081/admin/v1/nodes | python3 -c "import sys,json; print(next(n['id'] for n in json.load(sys.stdin)['data'] if 'h200' in n['hostname']))")
```

**`nvidia-smi` not found:**
No GPU or driver not installed. The agent degrades gracefully — CPU/RAM telemetry still works, GPU section shows 0 devices.

**Telemetry shows stale data:**
Check the agent is running: `pgrep -a nexus-nodeagent`. Restart it if needed.


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
