# Web Admin UI

The web UI is a Next.js 14 app running on port 3001 that provides a visual interface for all admin operations.

**URL:** http://localhost:3001

---

## Starting the web UI

```bash
# First time only
make web-install

# Start
make run-web
```

The UI proxies all API calls to `http://localhost:8081/admin/v1/*` via Next.js rewrites. No CORS configuration needed.

---

## Pages

### Dashboard (`/`)

Overview of the platform:
- Count cards: organizations, teams, LLM models, AI services, cluster nodes, GPU nodes
- Model status table: all models with health counts
- Cluster nodes table: hostname, CPUs, RAM, VRAM, status
- Quick actions: import Ollama, deploy service, simulate placement, create team

### Organizations (`/orgs`)

- List all organizations
- Create org (name + slug)
- Deactivate org

### Teams (`/teams`)

- List all teams with org filter
- Create team (name, slug, priority 1–100)
- Edit rate limits and quotas (RPM, TPD, max concurrent, max context)
- Manage model permissions (add/remove models)
- Create and revoke API keys

### API Keys (`/api-keys`)

- Create keys for any team (key shown once on creation)
- List all keys across all teams with prefix, status, last used
- Revoke keys

### Models (`/models`)

Three ways to add models:

**Import from Ollama** (button in top bar):
- Queries local Ollama at `localhost:11434`
- Registers all available models in one click
- Already-registered models are skipped

**Register External** (button):
- For already-running models (Ollama, TGI, vLLM, any API)
- Just needs: name, backend type, host, port

**Deploy vLLM Model** (button):
- Full vLLM deployment form
- GPU devices, tensor parallel, memory util, HF token
- NexusLLM manages the Docker container lifecycle

Per-model actions:
- **Health** → shows endpoint health table with latency, failures, last check time
- **Reset Health** → clears failed state, watcher re-evaluates within 5s
- **Enable / Disable** → toggles routing
- **Delete** → removes from registry

### AI Services (`/services`)

- List all AI services grouped by type (EMBEDDING, STT, TTS, etc.)
- Filter by service type
- Register a new service with CPU/GPU resource spec
- Shows health status per service

### Cluster Nodes (`/nodes`)

- List all cluster nodes with status indicators
- Node detail modal with:
  - Hardware summary (vCPUs, RAM, VRAM)
  - Labels display
  - **Telemetry tab**: CPU util bar, RAM util bar, last 60 snapshots
  - **Inventory tab**: raw JSON hardware inventory
- Register new node
- Auto-refreshes every 15 seconds

### Placement (`/placement`)

**Simulator panel:**
- Fill in model name, service type, runtime type, VRAM/CPU requirements
- Click **Run Placement Simulation** → see exactly which node/GPU/NUMA would be chosen
- Quick presets: Qwen3-32B, Llama-70B, DeepSeek-V3, Embedding, Whisper, Reranker
- Shows feasibility, score, reason, all resource details

**History table:**
- All placement decisions (including dry-runs)
- Which were actually applied (`applied: true`)

### GPU Inventory (`/gpu`)

- List GPU nodes
- Register GPU nodes and devices
- GPU packing simulation

### Usage (`/usage`)

- Select a team and date range
- Daily usage breakdown by model: requests, tokens, cost
- Org monthly spend total

### Settings (`/settings`)

- Service endpoint URLs with clickable links
- Gateway API route reference table
- Admin API highlights table
- Environment variable reference
- Quick start code block

---

## Making the UI work with your own services

The UI auto-discovers everything from the admin API. When you:
1. Register an Ollama model → it appears in `/models`
2. Grant it to a team → appears in that team's permissions
3. Watcher marks it healthy → health badge turns green

No configuration needed. Just register models via the UI or API and they flow through automatically.

---

## Troubleshooting

### UI shows "Loading…" forever

The Next.js dev server can't reach the admin API. Check:
1. Is `nexus-admin` running? (`curl http://localhost:8081/healthz`)
2. Is `make run-web` running from the `nexusllm` directory?

### Changes don't appear

Most tables auto-refresh every 15–30 seconds. Click the **Refresh** button or navigate away and back.

### "Error: Failed to fetch"

The admin API returned an error. Open browser DevTools → Network tab to see the actual error response from the admin.
