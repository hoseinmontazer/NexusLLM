# NexusLLM Automatic Scheduler & Placement Architecture

## Overview

This document describes the automatic runtime placement and scheduling system for NexusLLM. Users request models by name only — the platform automatically selects nodes, deploys runtimes, manages capacity, and handles lifecycle without administrator intervention.

## Design Principles

1. **Zero Manual Placement**: Administrators never manually select nodes in normal operation
2. **Transparent to Users**: Users only request model names; infrastructure is abstracted
3. **Dynamic Lifecycle**: Models auto-start on demand and auto-stop when idle
4. **Multi-Node Native**: Supports N nodes joining/leaving dynamically
5. **Capacity Aware**: Automatically queue, preempt, or reject when capacity exhausted
6. **Provider Independent**: Works with Docker, Kubernetes, or future providers

## Current State vs Desired State

### Current (Manual)
```
User → Request model
Admin → Choose node
Admin → Deploy to node
User → Get response
```

### Desired (Automatic)
```
User → Request "gemma-2"
Platform → Check runtime registry
Platform → Runtime exists? Route request
Platform → Runtime missing? Scheduler decides node
Platform → Deploy via START_MODEL task
Platform → Agent starts container
Platform → Health check passes
Platform → Register endpoint
Platform → Route request
User → Get response
```


## Architecture Components

### 1. Runtime Registry (Source of Truth)

The `agent_runtimes` table is the single source of truth for all runtime state:

```
Runtime States:
┌─────────────────────────────────────────────────┐
│ Deployment Pipeline                             │
├─────────────────────────────────────────────────┤
│ pending      → awaiting task dispatch           │
│ validating   → checking config/image            │
│ downloading  → fetching model weights           │
│ starting     → docker run executing             │
│ loading_model→ weights loading into VRAM        │
│ waiting_ready→ health endpoint starting         │
│ ready        → all 4 conditions met             │
├─────────────────────────────────────────────────┤
│ Operational States                              │
├─────────────────────────────────────────────────┤
│ idle         → no traffic, eviction pending     │
│ stopping     → graceful shutdown in progress    │
│ stopped      → cleanly stopped (weights cached) │
│ failed       → terminal error                   │
│ lost         → node offline                     │
└─────────────────────────────────────────────────┘
```

**Key Columns**:
- `state`: current lifecycle state
- `node_id`: which node runs this runtime
- `model_id`: which model this runtime serves
- `last_used_at`: for idle detection
- `requested_mode`, `effective_mode`: cpu/gpu/auto execution
- `workload_policy`: lazy_load | always_on


### 2. Scheduler (Decision Engine)

Location: `/internal/scheduler/scheduler.go`

**Responsibilities**:
1. Accept deployment requests (model name + requirements)
2. Query node inventory for capacity
3. Run placement algorithm (scoring + ranking)
4. Select best node
5. Enqueue START_MODEL task
6. Return deployment handle

**Placement Algorithm**:

```go
type PlacementRequest struct {
    ModelID         string
    ModelName       string
    
    // Requirements
    RequiredCPU     int
    RequiredRAMMB   int64
    RequiredVRAMMB  int64
    ExecutionMode   string // cpu | gpu | auto
    
    // Policy
    Priority        Priority // CRITICAL | HIGH | NORMAL | LOW | BEST_EFFORT
    ProjectID       string
    WorkloadPolicy  string   // lazy_load | always_on
}

type PlacementDecision struct {
    NodeID           string
    NodeHostname     string
    GPUDeviceIndices []int
    CPUSetCPUs       string  // e.g. "0-31"
    NUMANode         int
    Score            float64
    Reason           string
}
```


**Scoring Formula**:

```
Node Score = CapacityScore + LoadScore + LocalityScore + PriorityBonus

CapacityScore (0-400):
  - Free VRAM above requirement: +200
  - Free RAM above requirement: +100
  - Free CPU cores: +100

LoadScore (0-300):
  - GPU utilization < 50%: +150
  - Runtime density < threshold: +100
  - Health status healthy: +50

LocalityScore (0-200):
  - Model weights already cached: +150
  - NUMA locality match: +50

PriorityBonus (0-200):
  - CRITICAL project: +200
  - HIGH project: +150
  - NORMAL project: +100
  - LOW project: +50
  - BEST_EFFORT: 0

Penalties:
  - Node degraded: -200
  - Recent deployment failure: -100
  - Preemption required: -50
```

**Tie-Breaking Rules**:
1. Highest score wins
2. If scores equal: prefer node with most free VRAM
3. If still equal: prefer node with fewest runtimes
4. If still equal: prefer node with lowest ID (deterministic)


### 3. Runtime Requirements

Each model declares resource requirements in `model_runtime_configs`:

```sql
-- Example: Gemma 2 2B (GPU mode)
INSERT INTO model_runtime_configs (model_id, execution_mode, gguf_path, n_gpu_layers, ctx_size) 
VALUES ('...', 'gpu', '/models/gemma-2-2b-Q4_K_M.gguf', -1, 4096);

-- Derived requirements:
required_vram_mb: 8000     (computed from quantization + ctx_size)
required_cpu: 8
required_memory: 16384
execution_mode: gpu

-- Example: BGE-M3 Embedding (CPU mode)
execution_mode: cpu
required_cpu: 16
required_memory: 32768

-- Example: Qwen3-32B (Large GPU model)
execution_mode: gpu
required_vram_mb: 80000
tensor_parallel: 4         (needs 4 GPUs)
```

**Requirement Resolution**:

```go
func (s *Scheduler) ResolveRequirements(ctx context.Context, modelID string) (*Requirements, error) {
    // 1. Load from model_runtime_configs
    config := s.loadModelConfig(modelID)
    
    // 2. Compute VRAM from model size + quantization
    vramMB := s.estimateVRAM(config)
    
    // 3. Respect execution_mode constraint
    if config.ExecutionMode == "cpu" {
        return &Requirements{
            CPU: config.CPUThreads,
            RAM: config.MemoryLimit,
            GPU: 0,
        }
    }
    
    return &Requirements{
        VRAM: vramMB,
        GPU:  config.TensorParallel,
        CPU:  config.CPUThreads,
        RAM:  config.MemoryLimit,
    }
}
```


### 4. Node Inventory

Node Agent reports hardware to control plane every 30 seconds:

```go
type NodeInventory struct {
    NodeID      string
    Hostname    string
    Status      string  // online | offline | degraded | maintenance
    
    // CPU
    CPUCoresTotal  int
    CPUCoresUsed   int
    CPUUtilPct     float64
    
    // RAM
    RAMTotalMB     int64
    RAMUsedMB      int64
    RAMAvailMB     int64
    
    // GPU
    GPUs           []GPUDevice
    
    // NUMA
    NUMANodes      int
    NUMATopology   map[int]NUMANodeInfo
    
    // Capabilities
    HasGPU         bool
    HasNVLink      bool
    HasRDMA        bool
    
    LastHeartbeat  time.Time
}

type GPUDevice struct {
    DeviceIndex    int
    Name           string
    VRAMMB         int
    MemUsedMB      int
    UtilizationPct int
    TemperatureC   int
    NUMANode       int
    Status         string  // available | allocated | degraded
}
```

**Capacity Calculation**:

```sql
-- Available VRAM (total - allocated - reserved)
SELECT 
    n.id AS node_id,
    SUM(d.vram_mb) AS total_vram_mb,
    SUM(COALESCE(gt.memory_used_mb, 0)) AS used_vram_mb,
    COALESCE(pr.reserved_vram_mb, 0) AS reserved_vram_mb,
    SUM(d.vram_mb) - SUM(COALESCE(gt.memory_used_mb, 0)) - COALESCE(pr.reserved_vram_mb, 0) AS available_vram_mb
FROM nodes n
JOIN gpu_nodes gn ON gn.node_id = n.id
JOIN gpu_devices d ON d.node_id = gn.id
LEFT JOIN LATERAL (
    SELECT memory_used_mb FROM gpu_telemetry WHERE device_id = d.id ORDER BY recorded_at DESC LIMIT 1
) gt ON TRUE
LEFT JOIN (
    SELECT node_id, SUM(reserved_vram_mb) AS reserved_vram_mb
    FROM project_reservations pr
    JOIN agent_runtimes ar ON ar.project_id = pr.project_id
    WHERE ar.state IN ('ready', 'active', 'warm')
    GROUP BY node_id
) pr ON pr.node_id = n.id
WHERE n.status = 'online'
GROUP BY n.id;
```


### 5. Placement Strategy Implementation

**Location**: `/internal/scheduler/placer.go`

```go
// Placer decides where to run a model
type Placer struct {
    db          *sqlx.DB
    inventory   *NodeInventory
    preemptor   *preemption.Engine
    log         *zap.Logger
}

func (p *Placer) Decide(ctx context.Context, req PlacementRequest) (*PlacementDecision, error) {
    // 1. Load candidate nodes
    nodes, err := p.inventory.LoadOnlineNodes(ctx)
    if err != nil {
        return nil, err
    }
    
    // 2. Filter by hard constraints
    candidates := p.filterByRequirements(nodes, req)
    if len(candidates) == 0 {
        return p.handleInsufficientCapacity(ctx, req)
    }
    
    // 3. Score each candidate
    scored := make([]ScoredNode, 0, len(candidates))
    for _, node := range candidates {
        score := p.scoreNode(ctx, node, req)
        scored = append(scored, ScoredNode{Node: node, Score: score})
    }
    
    // 4. Sort by score descending
    sort.Slice(scored, func(i, j int) bool {
        if scored[i].Score != scored[j].Score {
            return scored[i].Score > scored[j].Score
        }
        // Tie breaker: prefer more free VRAM
        return scored[i].Node.FreeVRAMMB > scored[j].Node.FreeVRAMMB
    })
    
    // 5. Select best node
    best := scored[0]
    return p.buildDecision(ctx, best, req)
}
```


### 6. Capacity Management

When capacity exhausted, the scheduler has four options:

#### Option 1: Queue Runtime (Default)

```go
func (s *Scheduler) QueueDeployment(ctx context.Context, req PlacementRequest) (string, error) {
    queueID := uuid.New().String()
    _, err := s.db.ExecContext(ctx, `
        INSERT INTO deployment_queue 
          (id, project_id, runtime_config, priority_score, status, enqueued_at)
        VALUES ($1, $2, $3, $4, 'pending', NOW())`,
        queueID,
        req.ProjectID,
        req.ToJSON(),
        priorityToScore(req.Priority),
    )
    return queueID, err
}

// Background worker periodically retries queued deployments
func (s *Scheduler) ProcessQueue(ctx context.Context) {
    ticker := time.NewTicker(30 * time.Second)
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            s.retryQueuedDeployments(ctx)
        }
    }
}
```

#### Option 2: Preempt Low Priority Runtime

```go
func (s *Scheduler) PreemptAndDeploy(ctx context.Context, req PlacementRequest, targetNodeID string) error {
    // 1. Find lowest priority runtime on target node
    candidate, err := s.preemptor.SelectEvictionCandidate(ctx, targetNodeID)
    if err != nil {
        return fmt.Errorf("no preemption candidates: %w", err)
    }
    
    // 2. Verify priority rules (CRITICAL > HIGH > NORMAL > LOW > BEST_EFFORT)
    if !req.Priority.CanPreempt(candidate.Priority) {
        return fmt.Errorf("priority %s cannot preempt %s", req.Priority, candidate.Priority)
    }
    
    // 3. Execute preemption
    if err := s.preemptor.Preempt(ctx, candidate.RuntimeID); err != nil {
        return fmt.Errorf("preemption failed: %w", err)
    }
    
    // 4. Deploy new runtime
    return s.Deploy(ctx, req, targetNodeID)
}
```


#### Option 3: Unload Idle Runtime

```go
func (s *Scheduler) UnloadIdleAndDeploy(ctx context.Context, req PlacementRequest, targetNodeID string) error {
    // 1. Find idle runtimes (no traffic for > idle_timeout)
    var idleRuntimes []string
    err := s.db.SelectContext(ctx, &idleRuntimes, `
        SELECT id FROM agent_runtimes
        WHERE node_id = $1
          AND state IN ('ready', 'active', 'idle')
          AND workload_policy = 'lazy_load'
          AND last_used_at < NOW() - INTERVAL '30 minutes'
        ORDER BY last_used_at ASC
        LIMIT 1`,
        targetNodeID,
    )
    
    if len(idleRuntimes) == 0 {
        return fmt.Errorf("no idle runtimes to unload")
    }
    
    // 2. Dispatch UNLOAD_RUNTIME task
    for _, runtimeID := range idleRuntimes {
        payload := taskmanager.StopRuntimePayload{
            RuntimeID: runtimeID,
            DrainSecs: 10,
        }
        _, err := s.taskMgr.Enqueue(ctx, targetNodeID, 
            taskmanager.TaskUnloadRuntime, payload,
            taskmanager.WithPriority(90),
        )
        if err != nil {
            s.log.Warn("failed to enqueue unload", zap.Error(err))
        }
    }
    
    // 3. Wait for resources to free (poll loop)
    return s.waitForCapacity(ctx, targetNodeID, req.RequiredVRAMMB, 60*time.Second)
}
```

#### Option 4: Reject Deployment

```go
func (s *Scheduler) Reject(ctx context.Context, req PlacementRequest) error {
    s.log.Warn("deployment rejected: insufficient capacity",
        zap.String("model", req.ModelName),
        zap.Int64("required_vram_mb", req.RequiredVRAMMB),
    )
    return ErrInsufficientCapacity
}
```

**Decision Matrix**:

```
Project Priority | Capacity Exhausted | Action
-----------------+-------------------+----------------------------
CRITICAL         | Any               | Preempt → Queue if fails
HIGH             | NORMAL/LOW/BEST   | Preempt → Queue if fails
HIGH             | HIGH/CRITICAL     | Queue
NORMAL           | LOW/BEST          | Preempt → Queue if fails
NORMAL           | NORMAL+           | Queue
LOW              | BEST              | Preempt → Queue if fails
LOW              | LOW+              | Queue
BEST_EFFORT      | Any               | Queue (no preemption)
```


### 7. Dynamic Model Lifecycle

#### Cold Start → Auto Start
```
User requests "gemma-2" (model not running)
↓
Gateway checks RuntimeRegistry
↓
State = "unknown" or "stopped"
↓
RuntimeActivator.EnsureRunning("gemma-2")
↓
Scheduler.Decide(PlacementRequest)
↓
PlacementDecision: node_id=node-1, gpu_devices=[0]
↓
Enqueue START_MODEL task to node-1
↓
Agent executes pipeline:
  pending → validating → downloading → 
  starting → loading_model → waiting_ready → ready
↓
Health check passes (4 conditions met)
↓
Register endpoint in RuntimeRegistry
↓
Route request to endpoint
↓
Return response to user
```

#### Auto Stop (Idle Eviction)

```go
// IdleManager runs every 30 seconds
func (m *IdleManager) Sweep(ctx context.Context) {
    var candidates []Runtime
    err := m.db.SelectContext(ctx, &candidates, `
        SELECT id, model_id, node_id, container_id, last_used_at
        FROM agent_runtimes
        WHERE state IN ('ready', 'active', 'idle')
          AND workload_policy = 'lazy_load'
          AND last_used_at < NOW() - interval_seconds * INTERVAL '1 second'
          AND NOT EXISTS (
              SELECT 1 FROM project_configurations pc
              WHERE pc.project_id = agent_runtimes.project_id
                AND (pc.always_running = TRUE OR pc.protected = TRUE)
          )
    `)
    
    for _, runtime := range candidates {
        m.log.Info("idle runtime detected, unloading",
            zap.String("runtime_id", runtime.ID),
            zap.Duration("idle_for", time.Since(runtime.LastUsedAt)),
        )
        
        // Enqueue UNLOAD_RUNTIME task
        payload := taskmanager.StopRuntimePayload{
            RuntimeID:   runtime.ID,
            ContainerID: runtime.ContainerID,
            DrainSecs:   30,
        }
        _, _ = m.taskMgr.Enqueue(ctx, runtime.NodeID,
            taskmanager.TaskUnloadRuntime, payload,
            taskmanager.WithPriority(50),
            taskmanager.WithActor("idle-manager"),
        )
    }
}
```


#### Auto Restart (Always-On Policy)

```go
// Watchdog runs every 60 seconds
func (w *Watchdog) MonitorAlwaysRunning(ctx context.Context) {
    var failed []Runtime
    err := w.db.SelectContext(ctx, &failed, `
        SELECT ar.id, ar.model_id, ar.node_id, m.name AS model_name
        FROM agent_runtimes ar
        JOIN models m ON m.id = ar.model_id
        JOIN project_configurations pc ON pc.project_id = ar.project_id
        WHERE ar.state IN ('failed', 'stopped', 'lost')
          AND pc.always_running = TRUE
    `)
    
    for _, runtime := range failed {
        w.log.Info("always-on runtime down, restarting",
            zap.String("model", runtime.ModelName),
            zap.String("runtime_id", runtime.ID),
        )
        
        // Trigger restart via activator
        err := w.activator.StartModel(ctx, runtime.ModelName)
        if err != nil {
            w.log.Error("failed to restart always-on runtime",
                zap.String("model", runtime.ModelName),
                zap.Error(err),
            )
        }
    }
}
```

### 8. Multi-Node Support

**Node Registration**:

```sql
-- Node Agent automatically registers on first heartbeat
INSERT INTO nodes (id, hostname, total_cpu, total_ram_mb, status, agent_version, last_heartbeat_at)
VALUES (gen_random_uuid(), 'node-gpu-1', 64, 256000, 'online', '1.0.0', NOW())
ON CONFLICT (hostname) DO UPDATE SET
    last_heartbeat_at = NOW(),
    status = 'online',
    agent_version = EXCLUDED.agent_version;
```

**Dynamic Node Discovery**:

```go
// Scheduler automatically uses all online nodes
func (s *Scheduler) LoadCandidateNodes(ctx context.Context) ([]Node, error) {
    var nodes []Node
    err := s.db.SelectContext(ctx, &nodes, `
        SELECT id, hostname, status, total_cpu, total_ram_mb, total_vram_mb
        FROM nodes
        WHERE status IN ('online', 'degraded')
        ORDER BY status ASC, total_vram_mb DESC
    `)
    return nodes, err
}
```

**Node Failure Handling**:

```
Node Agent stops sending heartbeats
↓
Control plane detects: last_heartbeat_at > 90 seconds
↓
Mark node status = 'offline'
↓
Mark all runtimes on that node: state = 'lost'
↓
If workload_policy = 'always_on':
  → Reschedule to different node
↓
If workload_policy = 'lazy_load':
  → Next user request triggers cold start on healthy node
```


## Database Schema Extensions

### Migration 017: Scheduler Schema

```sql
-- Migration 017: Automatic Scheduler
BEGIN;

-- ─── Scheduler State ─────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS scheduler_state (
    singleton       BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton = TRUE),
    last_sweep_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    queue_length    INTEGER NOT NULL DEFAULT 0,
    active_deploys  INTEGER NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO scheduler_state (singleton) VALUES (TRUE) ON CONFLICT DO NOTHING;

-- ─── Node Capabilities (extended) ────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS node_capabilities (
    node_id         UUID PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    has_gpu         BOOLEAN NOT NULL DEFAULT FALSE,
    gpu_count       INTEGER NOT NULL DEFAULT 0,
    gpu_available   BOOLEAN NOT NULL DEFAULT FALSE,
    gpu_vram_mb     BIGINT NOT NULL DEFAULT 0,
    has_nvlink      BOOLEAN NOT NULL DEFAULT FALSE,
    has_rdma        BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ─── Model Requirements (cached computed values) ─────────────────────────────
CREATE TABLE IF NOT EXISTS model_requirements (
    model_id            UUID PRIMARY KEY REFERENCES models(id) ON DELETE CASCADE,
    required_cpu        INTEGER NOT NULL DEFAULT 0,
    required_ram_mb     BIGINT NOT NULL DEFAULT 0,
    required_vram_mb    BIGINT NOT NULL DEFAULT 0,
    required_gpu_count  INTEGER NOT NULL DEFAULT 0,
    execution_mode      VARCHAR(20) NOT NULL DEFAULT 'auto',
    workload_policy     VARCHAR(20) NOT NULL DEFAULT 'lazy_load',
    computed_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_model_requirements_exec_mode ON model_requirements(execution_mode);

-- ─── Scheduler Decisions Log ─────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS scheduler_decisions (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id            UUID NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    runtime_id          UUID REFERENCES agent_runtimes(id) ON DELETE SET NULL,
    node_id             UUID REFERENCES nodes(id) ON DELETE SET NULL,
    decision_type       VARCHAR(30) NOT NULL CHECK (decision_type IN 
                          ('placement', 'preemption', 'queue', 'reject')),
    score               NUMERIC(8,4) NOT NULL DEFAULT 0,
    reason              TEXT NOT NULL DEFAULT '',
    alternatives        JSONB NOT NULL DEFAULT '[]',
    outcome             VARCHAR(20) NOT NULL DEFAULT 'pending' CHECK (outcome IN
                          ('pending', 'success', 'failed', 'timeout')),
    decided_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_scheduler_decisions_model ON scheduler_decisions(model_id, decided_at DESC);
CREATE INDEX idx_scheduler_decisions_outcome ON scheduler_decisions(outcome, decided_at DESC);

COMMIT;
```


## Sequence Diagrams

### 1. Cold Start (Model Not Running)

```
User          Gateway         Scheduler       Agent          Runtime
 |               |                |             |               |
 |--POST /chat-->|                |             |               |
 |               |                |             |               |
 |               |--resolve model->|            |               |
 |               |<-not running----|            |               |
 |               |                 |            |               |
 |               |--Decide(req)--->|            |               |
 |               |                 |            |               |
 |               |                 |--query nodes               |
 |               |                 |--score candidates          |
 |               |                 |--select best               |
 |               |                 |            |               |
 |               |<-Decision-------|            |               |
 |               |(node=N1, gpu=0) |            |               |
 |               |                 |            |               |
 |               |--START_MODEL task----------->|               |
 |               |                 |            |               |
 |               |                 |            |--docker run-->|
 |               |                 |            |<-starting-----|
 |               |                 |            |               |
 |               |                 |            |<-loading------|
 |               |                 |            |               |
 |               |--poll health------------------->|            |
 |               |<-connection refused-------------|            |
 |               |(expected during loading)        |            |
 |               |                 |            |               |
 |               |--poll health------------------->|            |
 |               |<-200 OK-------------------------|            |
 |               |                 |            |               |
 |               |--register endpoint             |            |
 |               |                 |            |               |
 |               |--forward request--------------->|            |
 |               |<-response-----------------------|            |
 |<-response-----|                 |            |               |
```


### 2. Capacity Exhausted → Preemption

```
Admin         Scheduler       Preemptor       Agent          Runtime
 |               |                |             |               |
 |--deploy CRITICAL model-------->|            |               |
 |               |                |             |               |
 |               |--Decide()----->|            |               |
 |               |<-no capacity---|            |               |
 |               |                |             |               |
 |               |--PreemptFor(CRITICAL)------>|               |
 |               |                |             |               |
 |               |                |--find victim              |
 |               |                |(priority=LOW)             |
 |               |                |             |               |
 |               |                |--STOP_RUNTIME task------->|
 |               |                |             |--stop-------->|
 |               |                |             |<-stopped------|
 |               |                |             |               |
 |               |                |<-resources freed           |
 |               |                |             |               |
 |               |--retry Decide()              |               |
 |               |<-capacity available          |               |
 |               |                |             |               |
 |               |--START_MODEL task----------->|               |
 |               |                |             |--docker run-->|
 |<-deployed-----|                |             |               |
```

### 3. Node Failure → Reschedule

```
Node Agent    Control Plane   Scheduler       Agent(N2)      Runtime
(Node-1)                                                      
 |               |                |             |               |
 X (crash)       |                |             |               |
                 |                |             |               |
                 |--heartbeat timeout           |               |
                 |                |             |               |
                 |--mark node offline           |               |
                 |--mark runtimes lost          |               |
                 |                |             |               |
User------------>|                |             |               |
                 |--resolve model->|            |               |
                 |<-state=lost----|            |               |
                 |                 |            |               |
                 |--Decide()------>|            |               |
                 |<-Decision-------|            |               |
                 |(node=Node-2)    |            |               |
                 |                 |            |               |
                 |--START_MODEL task----------->|               |
                 |                 |            |--docker run-->|
                 |                 |            |<-ready--------|
User<-response---|                 |            |               |
```


## API Extensions

### Admin API: Deploy Model (Automatic Placement)

```http
POST /admin/v1/models/deploy
Content-Type: application/json

{
  "name": "gemma-2-9b",
  "display_name": "Gemma 2 9B Instruct",
  "backend_type": "llamacpp",
  "image": "ghcr.io/ggml-org/llama.cpp:server",
  "hf_repo": "bartowski/gemma-2-9b-it-GGUF",
  "hf_file": "gemma-2-9b-it-Q4_K_M.gguf",
  
  // Automatic placement (NEW)
  "auto_place": true,
  "execution_mode": "gpu",
  "workload_policy": "lazy_load",
  "priority": "normal",
  
  // Manual placement (legacy, optional)
  "node_id": "",
  "gpu_devices": []
}
```

**Response**:

```json
{
  "model_id": "uuid",
  "model_name": "gemma-2-9b",
  "endpoint_id": "uuid",
  "placement": {
    "node_id": "node-gpu-1",
    "node_hostname": "gpu-server-1",
    "gpu_devices": [0],
    "cpu_cores": 8,
    "numa_node": 0,
    "score": 875.5,
    "reason": "node=gpu-server-1 free_vram=45GB locality=cached"
  },
  "task_id": "uuid",
  "status": "deploying",
  "note": "START_MODEL task dispatched to node-gpu-1"
}
```


### Scheduler API: Manual Placement Override

```http
POST /admin/v1/scheduler/place
Content-Type: application/json

{
  "model_id": "uuid",
  "require_node_id": "node-gpu-2",  // hard constraint
  "prefer_gpu_devices": [2, 3],      // soft hint
  "priority": "high"
}
```

### Scheduler API: View Decisions

```http
GET /admin/v1/scheduler/decisions?model_id=uuid&limit=10
```

**Response**:

```json
{
  "decisions": [
    {
      "id": "uuid",
      "model_id": "uuid",
      "model_name": "gemma-2-9b",
      "decision_type": "placement",
      "node_id": "node-gpu-1",
      "node_hostname": "gpu-server-1",
      "score": 875.5,
      "reason": "free_vram=45GB util=30% cached=true",
      "alternatives": [
        {"node": "node-gpu-2", "score": 720.0, "reason": "free_vram=30GB util=60%"}
      ],
      "outcome": "success",
      "decided_at": "2026-06-24T10:30:00Z"
    }
  ]
}
```

### Scheduler API: Queue Status

```http
GET /admin/v1/scheduler/queue
```

**Response**:

```json
{
  "pending": 3,
  "items": [
    {
      "id": "uuid",
      "project_id": "uuid",
      "model_name": "qwen3-32b",
      "priority": "high",
      "priority_score": 75,
      "status": "pending",
      "enqueued_at": "2026-06-24T10:25:00Z",
      "attempts": 2,
      "last_attempt_at": "2026-06-24T10:28:00Z",
      "error_msg": "insufficient VRAM on all nodes"
    }
  ]
}
```


## Implementation Roadmap

### Phase 1: Core Scheduler (Week 1-2)

**Files to Create**:
- `/internal/scheduler/scheduler.go` - Main scheduler implementation
- `/internal/scheduler/placer.go` - Placement decision engine
- `/internal/scheduler/scorer.go` - Node scoring logic
- `/internal/scheduler/requirements.go` - Requirement resolver
- `/internal/scheduler/types.go` - Type definitions

**Integration Points**:
1. Extend `RuntimeActivator.StartModel()` to call scheduler when `node_id` is empty
2. Add `auto_place` flag to deploy API
3. Create migration 017 for scheduler tables

**Deliverable**: Deploy with `auto_place: true` selects node automatically

### Phase 2: Capacity Management (Week 3)

**Files to Create**:
- `/internal/scheduler/queue.go` - Deployment queue worker
- `/internal/scheduler/capacity.go` - Capacity calculator

**Features**:
1. Queue deployments when capacity exhausted
2. Background worker retries queued items every 30s
3. Admin API to view queue status

**Deliverable**: Deployments queue instead of failing when no capacity

### Phase 3: Idle Manager Enhancement (Week 4)

**Files to Modify**:
- `/internal/runtimemgr/idle_manager.go` - Add workload policy awareness

**Features**:
1. Respect `workload_policy` (lazy_load vs always_on)
2. Check `project_configurations.always_running` before eviction
3. Integrate with queue: evict idle → notify scheduler → retry queue

**Deliverable**: Idle runtimes auto-unload, freeing capacity for queue


### Phase 4: Preemption Integration (Week 5)

**Files to Modify**:
- `/internal/preemption/engine.go` - Add admission control trigger
- `/internal/scheduler/scheduler.go` - Call preemptor when needed

**Features**:
1. Scheduler calls `PreemptForProject()` when high-priority request arrives
2. Check `admission_policy` to decide preempt vs queue
3. Audit preemption events

**Deliverable**: CRITICAL deployments preempt LOW/BEST_EFFORT runtimes

### Phase 5: Multi-Node Discovery (Week 6)

**Files to Modify**:
- `/internal/nodeagent/agent.go` - Auto-register node on startup
- `/internal/scheduler/scheduler.go` - Query all online nodes

**Features**:
1. Node Agent creates `nodes` row on first heartbeat
2. Scheduler discovers nodes automatically
3. Handle node failure (mark offline, reschedule lost runtimes)

**Deliverable**: Add new node → automatically used by scheduler

### Phase 6: Requirement Computing & Caching (Week 7)

**Files to Create**:
- `/internal/scheduler/requirements_computer.go`

**Features**:
1. Compute requirements from model config (VRAM estimator)
2. Cache in `model_requirements` table
3. Invalidate cache on model config update

**Deliverable**: Accurate VRAM/RAM/CPU requirements for all backends

### Phase 7: Web UI Integration (Week 8)

**Files to Modify**:
- `/web/src/pages/ModelsPage.tsx` - Add placement info
- `/web/src/pages/SchedulerPage.tsx` - New page for queue/decisions

**Features**:
1. Show placement decision on model detail page
2. Scheduler dashboard: queue, decisions, node capacity
3. Trigger manual reschedule from UI

**Deliverable**: Full visibility into scheduler decisions via Web UI


## Testing Strategy

### Unit Tests

```go
// Test: Scoring Algorithm
func TestScoreNode(t *testing.T) {
    node := Node{
        FreeVRAMMB: 50000,
        FreeRAMMB: 100000,
        CPUUtil: 30.0,
        RuntimeCount: 2,
    }
    req := PlacementRequest{
        RequiredVRAMMB: 8000,
        Priority: PriorityNormal,
    }
    score := ScoreNode(node, req)
    assert.Greater(t, score, 500.0)
}

// Test: Tie Breaking
func TestTieBreaker(t *testing.T) {
    n1 := ScoredNode{Score: 800, Node: Node{FreeVRAMMB: 40000}}
    n2 := ScoredNode{Score: 800, Node: Node{FreeVRAMMB: 50000}}
    sorted := []ScoredNode{n1, n2}
    sort.Slice(sorted, tieBreakerFunc)
    assert.Equal(t, n2.Node.FreeVRAMMB, sorted[0].Node.FreeVRAMMB)
}

// Test: Preemption Priority Rules
func TestCanPreempt(t *testing.T) {
    assert.True(t, PriorityCritical.CanPreempt(PriorityLow))
    assert.False(t, PriorityNormal.CanPreempt(PriorityHigh))
    assert.False(t, PriorityCritical.CanPreempt(PriorityCritical))
}
```

### Integration Tests

```go
func TestEndToEndPlacement(t *testing.T) {
    // Setup: 2 nodes, 1 with GPU, 1 CPU-only
    db := setupTestDB()
    insertNode(db, "node-gpu-1", GPUNode{VRAM: 80000})
    insertNode(db, "node-cpu-1", CPUNode{Cores: 64})
    
    scheduler := NewScheduler(db, log)
    
    // Test 1: GPU model → placed on GPU node
    req := PlacementRequest{
        ModelName: "gemma-2",
        ExecutionMode: "gpu",
        RequiredVRAMMB: 8000,
    }
    dec, err := scheduler.Decide(ctx, req)
    assert.NoError(t, err)
    assert.Equal(t, "node-gpu-1", dec.NodeID)
    
    // Test 2: CPU model → placed on CPU node
    req2 := PlacementRequest{
        ModelName: "bge-m3",
        ExecutionMode: "cpu",
        RequiredCPU: 16,
    }
    dec2, err := scheduler.Decide(ctx, req2)
    assert.NoError(t, err)
    assert.Equal(t, "node-cpu-1", dec2.NodeID)
}
```


### Load Tests

```bash
# Concurrent deployments (stress test scheduler)
ab -n 100 -c 10 -T application/json -p deploy.json \
  http://localhost:3000/admin/v1/models/deploy

# Cold start latency (measure end-to-end)
for i in {1..50}; do
  time curl -X POST http://localhost:8080/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{"model":"gemma-2","messages":[{"role":"user","content":"hi"}]}'
done

# Capacity exhaustion (queue behavior)
# Deploy 10 large models to 2-GPU cluster → verify queue
for i in {1..10}; do
  curl -X POST http://localhost:3000/admin/v1/models/deploy \
    -d '{"name":"model-'$i'","auto_place":true,"required_vram_mb":40000}'
done

# Check queue
curl http://localhost:3000/admin/v1/scheduler/queue
```

### Chaos Tests

```bash
# Kill node mid-deployment
docker stop nexus-node-agent-1
# Verify: deployment retries on different node

# Fill VRAM to 100%
# Deploy large model → verify queue or preemption

# Rapid node add/remove
while true; do
  docker start nexus-node-agent-2
  sleep 30
  docker stop nexus-node-agent-2
  sleep 30
done
# Verify: no stuck deployments, scheduler adapts
```


## Monitoring & Observability

### Metrics

```go
// Prometheus metrics
var (
    schedulerDecisionsTotal = promauto.NewCounterVec(
        prometheus.CounterOpts{
            Name: "nexus_scheduler_decisions_total",
            Help: "Total placement decisions by outcome",
        },
        []string{"outcome", "decision_type"},
    )
    
    schedulerQueueLength = promauto.NewGauge(
        prometheus.GaugeOpts{
            Name: "nexus_scheduler_queue_length",
            Help: "Current deployment queue length",
        },
    )
    
    placementLatency = promauto.NewHistogram(
        prometheus.HistogramOpts{
            Name: "nexus_placement_decision_duration_seconds",
            Help: "Time to compute placement decision",
            Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1.0, 2.0},
        },
    )
    
    nodeCapacityUtilization = promauto.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "nexus_node_capacity_utilization",
            Help: "Resource utilization per node",
        },
        []string{"node_id", "resource_type"},
    )
)
```

### Logging

```go
// Structured logging for every decision
log.Info("placement decision",
    zap.String("model", req.ModelName),
    zap.String("decision_type", "placement"),
    zap.String("node_id", dec.NodeID),
    zap.String("node_hostname", dec.NodeHost),
    zap.Float64("score", dec.Score),
    zap.String("reason", dec.Reason),
    zap.Int64("free_vram_mb", node.FreeVRAMMB),
    zap.Int("runtime_count", node.RuntimeCount),
    zap.Duration("latency", time.Since(start)),
)

// Alert on repeated failures
if outcome == "failed" && attempts > 3 {
    log.Error("placement failed repeatedly",
        zap.String("model", req.ModelName),
        zap.Int("attempts", attempts),
        zap.String("last_error", lastError),
    )
}
```


### Grafana Dashboard

```json
{
  "dashboard": {
    "title": "NexusLLM Scheduler",
    "panels": [
      {
        "title": "Placement Decision Rate",
        "targets": [{
          "expr": "rate(nexus_scheduler_decisions_total[5m])"
        }]
      },
      {
        "title": "Queue Length Over Time",
        "targets": [{
          "expr": "nexus_scheduler_queue_length"
        }]
      },
      {
        "title": "Placement Latency (p95)",
        "targets": [{
          "expr": "histogram_quantile(0.95, nexus_placement_decision_duration_seconds)"
        }]
      },
      {
        "title": "Node VRAM Utilization",
        "targets": [{
          "expr": "nexus_node_capacity_utilization{resource_type=\"vram\"}"
        }]
      },
      {
        "title": "Deployment Outcome Breakdown",
        "targets": [{
          "expr": "sum by (outcome) (increase(nexus_scheduler_decisions_total[1h]))"
        }]
      }
    ]
  }
}
```

## Troubleshooting

### Issue: Deployments Always Queue

**Symptoms**: All deployment requests enter queue, never deploy

**Diagnosis**:
```sql
-- Check node status
SELECT id, hostname, status, last_heartbeat_at FROM nodes;

-- Check available capacity
SELECT 
    n.hostname,
    n.total_vram_mb,
    SUM(d.vram_mb) AS device_vram,
    COALESCE(SUM(gt.memory_used_mb), 0) AS used_vram
FROM nodes n
JOIN gpu_nodes gn ON gn.node_id = n.id
JOIN gpu_devices d ON d.node_id = gn.id
LEFT JOIN LATERAL (
    SELECT memory_used_mb FROM gpu_telemetry 
    WHERE device_id = d.id ORDER BY recorded_at DESC LIMIT 1
) gt ON TRUE
WHERE n.status = 'online'
GROUP BY n.id, n.hostname, n.total_vram_mb;
```

**Resolution**:
- If no nodes online: Start node agent
- If VRAM fully allocated: Unload idle runtimes or add nodes
- If requirements too high: Adjust model `required_vram_mb`


### Issue: Model Deploys to Wrong Node

**Symptoms**: Model placed on suboptimal node despite better options

**Diagnosis**:
```sql
-- Check last placement decision
SELECT 
    sd.model_id,
    m.name,
    sd.node_id,
    n.hostname,
    sd.score,
    sd.reason,
    sd.alternatives
FROM scheduler_decisions sd
JOIN models m ON m.id = sd.model_id
JOIN nodes n ON n.id = sd.node_id
WHERE sd.decision_type = 'placement'
ORDER BY sd.decided_at DESC
LIMIT 5;
```

**Resolution**:
- Review scoring logic in `/internal/scheduler/scorer.go`
- Check if node telemetry is stale (agent not reporting)
- Verify GPU devices marked `status='available'`

### Issue: Preemption Not Working

**Symptoms**: High-priority deployment queued instead of preempting

**Diagnosis**:
```sql
-- Check runtime priorities
SELECT 
    ar.id,
    m.name,
    ar.node_id,
    COALESCE(p.priority, 'NORMAL') AS priority,
    ar.state
FROM agent_runtimes ar
JOIN models m ON m.id = ar.model_id
LEFT JOIN projects p ON p.id = ar.project_id
WHERE ar.state IN ('ready', 'active', 'warm')
ORDER BY project_priority_score(COALESCE(p.priority, 'NORMAL')) ASC;
```

**Resolution**:
- Ensure victim has lower priority than requestor
- Check `project_configurations.protected = FALSE` for victim
- Verify preemption engine is running (`PreemptionEngine.Start()`)


## Future Enhancements

### 1. Machine Learning-Based Placement

Train a model to predict optimal placement based on:
- Historical performance data
- Failure patterns
- Load forecasting

```go
type MLPlacer struct {
    model    *tensorflow.SavedModel
    features FeatureExtractor
}

func (p *MLPlacer) PredictScore(node Node, req PlacementRequest) float64 {
    features := p.features.Extract(node, req)
    tensor := tensorflow.NewTensor(features)
    output := p.model.Run(tensor)
    return output[0].Value().(float64)
}
```

### 2. Multi-Region Support

Extend scheduler to place across geographic regions:

```go
type Region struct {
    ID       string
    Name     string
    Latency  map[string]time.Duration // region_id → latency
    Nodes    []Node
}

func (s *Scheduler) DecideMultiRegion(req PlacementRequest) (*Decision, error) {
    // Consider: data locality, compliance, latency, cost
}
```

### 3. Cost-Aware Scheduling

Factor in GPU/CPU costs when scoring:

```go
type CostModel struct {
    GPUCostPerHour  float64
    CPUCostPerHour  float64
    VRAMCostPerGB   float64
}

func (s *Scheduler) ScoreWithCost(node Node, req PlacementRequest, budget float64) float64 {
    performanceScore := s.scoreNode(node, req)
    costScore := s.estimateCost(node, req) / budget * 100
    return performanceScore - costScore
}
```


### 4. Kubernetes Provider

Extend to Kubernetes backend:

```go
type K8sProvider struct {
    clientset *kubernetes.Clientset
}

func (p *K8sProvider) DeployRuntime(spec RuntimeSpec, decision PlacementDecision) error {
    pod := &corev1.Pod{
        ObjectMeta: metav1.ObjectMeta{
            Name: "nexus-" + spec.ModelName,
            Labels: map[string]string{
                "app":   "nexus-runtime",
                "model": spec.ModelName,
            },
        },
        Spec: corev1.PodSpec{
            NodeSelector: map[string]string{
                "kubernetes.io/hostname": decision.NodeHost,
            },
            Containers: []corev1.Container{{
                Name:  "runtime",
                Image: spec.Image,
                Resources: corev1.ResourceRequirements{
                    Limits: corev1.ResourceList{
                        "nvidia.com/gpu": resource.MustParse(fmt.Sprintf("%d", len(decision.GPUDeviceIndices))),
                        "memory":         resource.MustParse(fmt.Sprintf("%dMi", spec.MemoryLimitMB)),
                    },
                },
            }},
        },
    }
    
    _, err := p.clientset.CoreV1().Pods("nexus-system").Create(ctx, pod, metav1.CreateOptions{})
    return err
}
```

### 5. Bin Packing Optimization

Use integer linear programming for optimal placement:

```go
// Maximize: sum(placed[i])
// Subject to:
//   sum(vram[i] * placed[i]) <= node_vram for each node
//   sum(ram[i] * placed[i]) <= node_ram for each node
//   placed[i] in {0, 1}

func (s *Scheduler) OptimalPacking(requests []PlacementRequest, nodes []Node) []Placement {
    solver := lpsolve.NewSolver()
    // ... setup constraints and objective ...
    solution := solver.Solve()
    return parseSolution(solution)
}
```


## Summary

The automatic scheduler transforms NexusLLM from a manually-managed gateway into a true AI platform. Key benefits:

### For Users
- **Transparent Infrastructure**: Request models by name; never think about nodes
- **Instant Access**: Models start automatically on first request
- **Consistent Experience**: Always routed to healthy endpoint

### For Operators
- **Zero Manual Placement**: Add nodes; scheduler uses them automatically
- **Capacity Awareness**: Queue, preempt, or reject when full
- **Self-Healing**: Failed nodes trigger automatic rescheduling

### For Platform
- **Dynamic Scaling**: N nodes join/leave without config changes
- **Provider Independent**: Works with Docker, Kubernetes, or future backends
- **Resource Efficiency**: Idle models unload; busy models stay running

## References

- [Architecture Overview](03-architecture.md)
- [Node Agent Design](19-node-agent-architecture.md)
- [Placement Engine](08-placement.md)
- [Projects & Priorities](11_projects.sql)
- [Preemption Engine](internal/preemption/engine.go)
- [Runtime Manager](internal/runtimemgr/activator.go)

---

**Next Steps**: See [Implementation Roadmap](#implementation-roadmap) to begin development.
