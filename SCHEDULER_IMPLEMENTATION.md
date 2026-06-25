# NexusLLM Automatic Scheduler Implementation Guide

## Overview

This document provides the complete implementation plan for transforming NexusLLM from a manually-managed gateway into an automatic AI platform with intelligent runtime placement and scheduling.

## Architecture Summary

```
┌──────────────────────────────────────────────────────────────┐
│                         User Request                          │
│                  POST /v1/chat/completions                   │
│                     {"model": "gemma-2"}                     │
└───────────────────────┬──────────────────────────────────────┘
                        │
                        ▼
┌──────────────────────────────────────────────────────────────┐
│                    Gateway / Proxy                           │
│  • Check RuntimeRegistry for active endpoint                 │
│  • If exists → route request                                 │
│  • If missing → trigger cold start                           │
└───────────────────────┬──────────────────────────────────────┘
                        │
                        ▼
┌──────────────────────────────────────────────────────────────┐
│                  RuntimeActivator                            │
│  • EnsureRunning(model_name)                                 │
│  • Deduplicate concurrent requests                           │
│  • Call Scheduler if no node assigned                        │
└───────────────────────┬──────────────────────────────────────┘
                        │
                        ▼
┌──────────────────────────────────────────────────────────────┐
│                     Scheduler                                │
│  1. Load requirements from model_runtime_configs             │
│  2. Query node inventory (CPU, RAM, GPU VRAM)                │
│  3. Filter by hard constraints                               │
│  4. Score candidates (capacity + load + locality)            │
│  5. Select best node                                         │
│  6. Handle insufficient capacity:                            │
│     - Queue deployment                                       │
│     - Preempt low-priority runtime                           │
│     - Unload idle runtime                                    │
│  7. Return PlacementDecision                                 │
└───────────────────────┬──────────────────────────────────────┘
                        │
                        ▼
┌──────────────────────────────────────────────────────────────┐
│                   TaskManager                                │
│  • Enqueue START_MODEL task to selected node                 │
│  • Task payload includes full model config                   │
└───────────────────────┬──────────────────────────────────────┘
                        │
                        ▼
┌──────────────────────────────────────────────────────────────┐
│                   Node Agent                                 │
│  • Poll for pending tasks                                    │
│  • Execute START_MODEL pipeline:                             │
│    pending → validating → downloading →                      │
│    starting → loading_model → waiting_ready → ready          │
│  • Report state transitions to control plane                 │
└───────────────────────┬──────────────────────────────────────┘
                        │
                        ▼
┌──────────────────────────────────────────────────────────────┐
│                 Docker / Runtime                             │
│  • Container starts                                          │
│  • Model weights load into VRAM                              │
│  • Health endpoint returns 200                               │
└───────────────────────┬──────────────────────────────────────┘
                        │
                        ▼
┌──────────────────────────────────────────────────────────────┐
│                RuntimeRegistry                               │
│  • Register endpoint (model_name → URL)                      │
│  • Enable routing                                            │
└───────────────────────┬──────────────────────────────────────┘
                        │
                        ▼
                  User receives response
```


## Files Created

### Documentation
- `/docs/20-automatic-scheduler.md` - Complete architecture documentation

### Database
- `/migrations/017_scheduler.sql` - Scheduler tables and views

### Go Packages
- `/internal/scheduler/scheduler.go` - Main scheduler implementation
- `/internal/scheduler/types.go` - Type definitions
- `/internal/scheduler/placer.go` - Placement decision engine (TODO)
- `/internal/scheduler/scorer.go` - Node scoring logic (TODO)
- `/internal/scheduler/requirements.go` - Requirements resolver (TODO)
- `/internal/scheduler/queue.go` - Queue processor (partial in scheduler.go)

## Integration Points

### 1. RuntimeActivator Integration

**File**: `/internal/runtimemgr/activator.go`

**Current Flow**:
```go
func (a *RuntimeActivator) StartModel(ctx context.Context, modelName string) error {
    cfg, err := a.loadConfig(ctx, modelName)
    if cfg.NodeID == "" {
        return fmt.Errorf("no node assigned")
    }
    return a.enqueueStartModel(ctx, cfg)
}
```

**New Flow** (add this):
```go
func (a *RuntimeActivator) StartModel(ctx context.Context, modelName string) error {
    cfg, err := a.loadConfig(ctx, modelName)
    if cfg.NodeID == "" {
        // Auto-placement: ask scheduler to decide
        req := a.buildPlacementRequest(cfg)
        dec, err := a.scheduler.Decide(ctx, req)
        if err != nil {
            return fmt.Errorf("placement failed: %w", err)
        }
        cfg.NodeID = dec.NodeID
        cfg.GPUDevices = dec.GPUDeviceIndices
        cfg.CPUSetCPUs = dec.CPUSetCPUs
        cfg.NUMANode = dec.NUMANode
    }
    return a.enqueueStartModel(ctx, cfg)
}
```


### 2. Admin Handler Integration

**File**: `/internal/admin/handlers/runtime.go`

**Modify DeployModel()** to support `auto_place`:

```go
func (h *RuntimeHandler) DeployModel(c *gin.Context) {
    var input struct {
        // ... existing fields ...
        AutoPlace      bool   `json:"auto_place"`
        ExecutionMode  string `json:"execution_mode"`  // cpu | gpu | auto
        WorkloadPolicy string `json:"workload_policy"` // lazy_load | always_on
        Priority       string `json:"priority"`        // normal | high | critical
        
        // Manual placement (legacy)
        NodeID     string `json:"node_id"`
        GPUDevices []int  `json:"gpu_devices"`
    }
    
    // ... existing validation ...
    
    // Auto-placement path
    if input.AutoPlace && h.scheduler != nil && input.NodeID == "" {
        req := scheduler.PlacementRequest{
            ModelID:        mID,
            ModelName:      input.Name,
            RequiredVRAMMB: estimateVRAM(input),  // TODO: implement
            ExecutionMode:  input.ExecutionMode,
            Priority:       parsePriority(input.Priority),
        }
        
        dec, err := h.scheduler.Decide(c.Request.Context(), req)
        if err != nil {
            // Capacity exhausted — queue it
            queueID, _ := h.scheduler.Queue(c.Request.Context(), req)
            c.JSON(http.StatusAccepted, gin.H{
                "model_id":   mID,
                "queue_id":   queueID,
                "status":     "queued",
                "message":    "insufficient capacity, deployment queued",
            })
            return
        }
        
        // Use scheduler's decision
        input.NodeID = dec.NodeID
        input.GPUDevices = dec.GPUDeviceIndices
    }
    
    // ... rest of deployment logic ...
}
```


### 3. Main Entry Point Integration

**File**: `/cmd/admin/main.go`

**Add Scheduler Initialization**:

```go
func main() {
    // ... existing setup ...
    
    // Construct scheduler
    scheduler := scheduler.NewScheduler(db, taskMgr, preemptEng, log)
    
    // Start queue processor in background
    go scheduler.Start(appCtx)
    
    // Wire into handlers
    runtimeH := handlers.NewRuntimeHandler(db, rdb, registry, modelCtrl).
        WithPlacement(placementEng).
        WithTaskManager(taskMgr).
        WithScheduler(scheduler)  // NEW
    
    // Wire into activator
    activator := runtimemgr.NewActivator(db, taskMgr, registry, guard, runtimeCfg, log).
        WithScheduler(scheduler)  // NEW
    
    // ... rest of setup ...
}
```

## Implementation Checklist

### Phase 1: Core Scheduler (Week 1-2)

- [ ] Create `/internal/scheduler/placer.go`
  - [ ] Implement `loadCandidateNodes()`
  - [ ] Implement `filterByRequirements()`
  - [ ] Implement `buildDecision()`
  
- [ ] Create `/internal/scheduler/scorer.go`
  - [ ] Implement `scoreNode()`
  - [ ] Capacity scoring (free VRAM, RAM, CPU)
  - [ ] Load scoring (utilization, runtime density)
  - [ ] Locality scoring (cached models, NUMA)
  - [ ] Priority bonus calculation
  
- [ ] Create `/internal/scheduler/requirements.go`
  - [ ] Implement `ResolveRequirements(modelID)`
  - [ ] VRAM estimator (from quantization + ctx_size)
  - [ ] Cache in `model_requirements` table
  
- [ ] Apply migration 017
  - [ ] Test on dev database
  - [ ] Verify indexes created
  
- [ ] Integration tests
  - [ ] Test GPU placement
  - [ ] Test CPU placement
  - [ ] Test execution_mode resolution

### Phase 2: Capacity Management (Week 3)

- [ ] Implement `handleNoCapacity()`
  - [ ] Queue deployment
  - [ ] Try preemption (if priority allows)
  - [ ] Try idle eviction
  - [ ] Reject if all fail
  
- [ ] Implement `Queue()` method
- [ ] Implement `processQueue()` worker
- [ ] Add exponential backoff logic
- [ ] Admin API: GET /admin/v1/scheduler/queue


### Phase 3: Idle Manager Enhancement (Week 4)

- [ ] Modify `/internal/runtimemgr/idle_manager.go`
  - [ ] Check `workload_policy` before eviction
  - [ ] Respect `always_running` from `project_configurations`
  - [ ] Notify scheduler after eviction (trigger queue retry)
  
- [ ] Add idle metrics to Prometheus
- [ ] Test idle → unload → queue retry flow

### Phase 4: Preemption Integration (Week 5)

- [ ] Modify `Scheduler.Decide()` to call preemptor
- [ ] Implement admission control logic
- [ ] Check `admission_policy` (queue vs preempt_then_queue vs reject)
- [ ] Integration test: CRITICAL preempts LOW

### Phase 5: Multi-Node Discovery (Week 6)

- [ ] Node Agent auto-registration on first heartbeat
- [ ] Scheduler queries all online nodes automatically
- [ ] Node failure handling:
  - [ ] Mark node offline after heartbeat timeout
  - [ ] Mark runtimes as `lost`
  - [ ] Reschedule always-on runtimes
- [ ] Test: add node → automatically used by scheduler

### Phase 6: Requirement Computing & Caching (Week 7)

- [ ] Create `/internal/scheduler/requirements_computer.go`
- [ ] VRAM estimator for llamacpp (quantization + ctx_size)
- [ ] VRAM estimator for vLLM (model size + tensor_parallel)
- [ ] Background job to compute all model requirements
- [ ] Cache invalidation on model config update

### Phase 7: Web UI Integration (Week 8)

- [ ] Models page: show placement decision
- [ ] New Scheduler dashboard page:
  - [ ] Queue table
  - [ ] Recent decisions
  - [ ] Node capacity chart
- [ ] Manual reschedule button

## Testing Strategy

### Unit Tests

Create `/internal/scheduler/scheduler_test.go`:

```go
func TestScoreNode(t *testing.T) {
    // Test capacity scoring
    // Test load scoring
    // Test locality bonus
    // Test priority bonus
}

func TestTieBreaker(t *testing.T) {
    // Same score → prefer more free VRAM
}

func TestCanPreempt(t *testing.T) {
    // CRITICAL can preempt LOW
    // NORMAL cannot preempt HIGH
}
```

### Integration Tests

Create `/internal/scheduler/integration_test.go`:

```go
func TestEndToEndPlacement(t *testing.T) {
    // Setup 2 nodes (1 GPU, 1 CPU)
    // Place GPU model → must go to GPU node
    // Place CPU model → must go to CPU node
}

func TestCapacityExhausted(t *testing.T) {
    // Fill all nodes
    // Next deployment → queued
}

func TestPreemption(t *testing.T) {
    // Fill node with LOW priority runtime
    // Deploy CRITICAL → LOW preempted
}
```


## Configuration

Add to `/internal/config/config.go`:

```go
type Config struct {
    // ... existing fields ...
    
    Scheduler SchedulerConfig
}

type SchedulerConfig struct {
    Enabled           bool          `env:"NEXUS_SCHEDULER_ENABLED"            default:"true"`
    QueuePollInterval time.Duration `env:"NEXUS_SCHEDULER_QUEUE_POLL"         default:"30s"`
    PlacementTimeout  time.Duration `env:"NEXUS_SCHEDULER_PLACEMENT_TIMEOUT"  default:"10s"`
    DefaultPriority   string        `env:"NEXUS_SCHEDULER_DEFAULT_PRIORITY"   default:"normal"`
}
```

## Monitoring

### Metrics

Add to `/internal/scheduler/metrics.go`:

```go
var (
    SchedulerDecisions = promauto.NewCounterVec(
        prometheus.CounterOpts{
            Name: "nexus_scheduler_decisions_total",
            Help: "Total placement decisions by outcome",
        },
        []string{"outcome", "decision_type"},
    )
    
    QueueLength = promauto.NewGauge(
        prometheus.GaugeOpts{
            Name: "nexus_scheduler_queue_length",
            Help: "Current deployment queue length",
        },
    )
    
    PlacementLatency = promauto.NewHistogram(
        prometheus.HistogramOpts{
            Name: "nexus_placement_decision_duration_seconds",
            Help: "Time to compute placement decision",
            Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1.0, 2.0},
        },
    )
)
```

### Logging

All scheduler decisions are logged with:
- `model_id`, `model_name`
- `decision_type` (placement | queue | preemption | reject)
- `node_id`, `node_hostname`
- `score`, `reason`
- `latency`

## API Reference

### Deploy with Auto-Placement

```bash
curl -X POST http://localhost:3000/admin/v1/models/deploy \
  -H "Content-Type: application/json" \
  -d '{
    "name": "gemma-2-9b",
    "backend_type": "llamacpp",
    "hf_repo": "bartowski/gemma-2-9b-it-GGUF",
    "hf_file": "gemma-2-9b-it-Q4_K_M.gguf",
    "auto_place": true,
    "execution_mode": "gpu",
    "priority": "normal"
  }'
```

Response:
```json
{
  "model_id": "uuid",
  "placement": {
    "node_id": "node-gpu-1",
    "node_hostname": "gpu-server-1",
    "gpu_devices": [0],
    "score": 875.5,
    "reason": "free_vram=45GB util=30% cached=true"
  },
  "task_id": "uuid",
  "status": "deploying"
}
```


### View Queue

```bash
curl http://localhost:3000/admin/v1/scheduler/queue
```

Response:
```json
{
  "pending": 2,
  "items": [
    {
      "id": "uuid",
      "model_name": "qwen3-32b",
      "priority": "high",
      "priority_score": 75,
      "required_vram_mb": 80000,
      "status": "pending",
      "enqueued_at": "2026-06-24T10:25:00Z",
      "attempts": 2,
      "last_error": "insufficient VRAM on all nodes"
    }
  ]
}
```

### View Recent Decisions

```bash
curl http://localhost:3000/admin/v1/scheduler/decisions?limit=10
```

Response:
```json
{
  "decisions": [
    {
      "id": "uuid",
      "model_name": "gemma-2-9b",
      "decision_type": "placement",
      "node_hostname": "gpu-server-1",
      "score": 875.5,
      "reason": "free_vram=45GB util=30% cached=true",
      "outcome": "success",
      "decided_at": "2026-06-24T10:30:00Z"
    }
  ]
}
```

## Troubleshooting

### Issue: All deployments queue

**Check node status**:
```sql
SELECT id, hostname, status, last_heartbeat_at FROM nodes;
```

**Check available VRAM**:
```sql
SELECT node_id, scheduler_available_vram_mb(id) AS free_vram_mb FROM nodes;
```

**Resolution**:
- If no nodes online: Start node agent
- If VRAM full: Unload idle runtimes or add nodes
- If requirements too high: Adjust model config

### Issue: Wrong node selected

**Check last decision**:
```sql
SELECT * FROM scheduler_decisions
WHERE decision_type = 'placement'
ORDER BY decided_at DESC LIMIT 5;
```

**Check alternatives**:
```sql
SELECT alternatives FROM scheduler_decisions
WHERE id = '<decision_id>';
```

## Next Steps

1. **Review** `/docs/20-automatic-scheduler.md` for complete architecture
2. **Apply** migration 017: `psql nexusllm < migrations/017_scheduler.sql`
3. **Implement** Phase 1 (Core Scheduler) following the checklist above
4. **Test** with integration tests
5. **Deploy** to staging environment
6. **Monitor** metrics and decisions
7. **Iterate** through remaining phases

## References

- [Architecture Documentation](docs/20-automatic-scheduler.md)
- [Node Agent Design](docs/19-node-agent-architecture.md)
- [Placement Engine](docs/08-placement.md)
- [Runtime Manager](internal/runtimemgr/activator.go)
- [Preemption Engine](internal/preemption/engine.go)

---

**Questions or Issues?** Refer to the comprehensive documentation in `/docs/20-automatic-scheduler.md` or examine the existing placement engine in `/internal/placement/engine.go` for patterns to follow.
