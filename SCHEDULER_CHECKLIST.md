# NexusLLM Automatic Scheduler - Implementation Checklist

## Prerequisites

- [ ] Review `/docs/20-automatic-scheduler.md` (complete architecture)
- [ ] Review `/SCHEDULER_IMPLEMENTATION.md` (integration guide)
- [ ] Review `/docs/SCHEDULER_FLOW_DIAGRAM.md` (visual flows)
- [ ] Backup production database before applying migration

## Phase 1: Core Scheduler (Week 1-2)

### Database
- [ ] Apply migration: `psql nexusllm < migrations/017_scheduler.sql`
- [ ] Verify tables exist: `scheduler_state`, `node_capabilities`, `model_requirements`, `scheduler_decisions`
- [ ] Verify views exist: `scheduler_node_metrics`
- [ ] Test helper functions: `SELECT scheduler_available_vram_mb('node-id')`

### Implementation Files
- [ ] Create `/internal/scheduler/placer.go`
  - [ ] `loadCandidateNodes()` - Query online nodes
  - [ ] `filterByRequirements()` - Hard constraint filtering
  - [ ] `buildDecision()` - Convert scored node to PlacementDecision
  
- [ ] Create `/internal/scheduler/scorer.go`
  - [ ] `scoreNode()` - Main scoring function
  - [ ] `capacityScore()` - Free VRAM, RAM, CPU
  - [ ] `loadScore()` - Utilization, density, health
  - [ ] `localityScore()` - Cached models, NUMA
  - [ ] `priorityBonus()` - Project priority
  - [ ] `sortScoredNodes()` - Sort + tie-breaking
  
- [ ] Create `/internal/scheduler/requirements.go`
  - [ ] `ResolveRequirements(modelID)` - Main resolver
  - [ ] `estimateVRAM()` - llamacpp GGUF estimator
  - [ ] `estimateVRAM()` - vLLM estimator
  - [ ] `estimateVRAM()` - TGI estimator
  - [ ] `cacheRequirements()` - Store in model_requirements

### Integration
- [ ] Modify `/internal/runtimemgr/activator.go`
  - [ ] Add `scheduler *scheduler.Scheduler` field
  - [ ] Add `WithScheduler()` method
  - [ ] Modify `StartModel()`: call scheduler when `node_id` empty
  - [ ] Build `PlacementRequest` from model config
  
- [ ] Modify `/internal/admin/handlers/runtime.go`
  - [ ] Add `scheduler *scheduler.Scheduler` field
  - [ ] Add `WithScheduler()` method
  - [ ] Modify `DeployModel()`: support `auto_place` flag
  - [ ] Handle queue response when capacity exhausted
  
- [ ] Modify `/cmd/admin/main.go`
  - [ ] Construct scheduler
  - [ ] Start queue processor: `go scheduler.Start(ctx)`
  - [ ] Wire into RuntimeHandler
  - [ ] Wire into RuntimeActivator

### Testing
- [ ] Unit tests: `/internal/scheduler/scheduler_test.go`
  - [ ] `TestScoreNode()`
  - [ ] `TestCapacityScore()`
  - [ ] `TestLoadScore()`
  - [ ] `TestLocalityScore()`
  - [ ] `TestPriorityBonus()`
  - [ ] `TestTieBreaker()`
  - [ ] `TestCanPreempt()`
  
- [ ] Integration tests: `/internal/scheduler/integration_test.go`
  - [ ] `TestEndToEndPlacement()` - 2 nodes, GPU + CPU
  - [ ] `TestGPUPlacement()` - GPU model → GPU node
  - [ ] `TestCPUPlacement()` - CPU model → CPU node
  - [ ] `TestExecutionModeAuto()` - auto resolves correctly
  
- [ ] Manual test: Deploy with auto_place
  ```bash
  curl -X POST http://localhost:3000/admin/v1/models/deploy \
    -d '{"name":"test","auto_place":true,"execution_mode":"gpu"}'
  ```

### Verification
- [ ] Check decision recorded: `SELECT * FROM scheduler_decisions ORDER BY decided_at DESC LIMIT 1;`
- [ ] Check runtime started: `SELECT * FROM agent_runtimes ORDER BY created_at DESC LIMIT 1;`
- [ ] Check node assignment matches decision

### Documentation
- [ ] Update API docs with `auto_place` flag
- [ ] Add metrics to Grafana dashboard


## Phase 2: Capacity Management (Week 3)

### Queue Implementation
- [ ] Implement `Scheduler.Queue(req PlacementRequest) (string, error)`
  - [ ] INSERT into deployment_queue
  - [ ] Return queue_id
  
- [ ] Implement `Scheduler.handleNoCapacity(req)`
  - [ ] Check priority
  - [ ] Check admission_policy
  - [ ] Decide: queue vs preempt vs reject
  
- [ ] Implement exponential backoff
  - [ ] `backoff(attempts int) time.Duration`
  - [ ] Formula: `2^attempts * base_interval`
  - [ ] Max: 30 minutes

### Background Worker
- [ ] Enhance `processQueue()` in scheduler.go
  - [ ] Load pending items
  - [ ] Retry Decide() for each
  - [ ] Update queue status on success
  - [ ] Update next_retry_at on failure
  - [ ] Log queue metrics

### Admin API
- [ ] Create `/internal/admin/handlers/scheduler.go`
  - [ ] `GetQueue()` - List pending deployments
  - [ ] `GetDecisions()` - List recent decisions
  - [ ] `CancelQueuedDeployment()` - Cancel pending item
  
- [ ] Wire routes in `/cmd/admin/main.go`
  - [ ] `GET /admin/v1/scheduler/queue`
  - [ ] `GET /admin/v1/scheduler/decisions`
  - [ ] `DELETE /admin/v1/scheduler/queue/:id`

### Testing
- [ ] Test queue workflow
  - [ ] Fill all nodes
  - [ ] Deploy model → queued
  - [ ] Free capacity
  - [ ] Queue processor deploys
  
- [ ] Test exponential backoff
  - [ ] Deploy with no capacity
  - [ ] Check retry intervals grow: 30s, 60s, 120s, ...

### Verification
- [ ] Check queue: `SELECT * FROM deployment_queue WHERE status='pending';`
- [ ] Check queue metrics: `SELECT queue_length FROM scheduler_state;`
- [ ] Monitor Prometheus: `nexus_scheduler_queue_length`


## Phase 3: Idle Manager Enhancement (Week 4)

### Idle Manager Updates
- [ ] Modify `/internal/runtimemgr/idle_manager.go`
  - [ ] Check `workload_policy` column
  - [ ] Skip `always_on` runtimes
  - [ ] Join `project_configurations`, check `always_running`
  - [ ] Skip protected runtimes
  - [ ] After eviction, notify scheduler (optional)

### Integration
- [ ] IdleManager calls `Scheduler.OnCapacityFreed(nodeID, freedVRAM)`
- [ ] Scheduler immediately retries queue for that node

### Testing
- [ ] Test idle eviction respects workload_policy
- [ ] Test always_on runtime is never evicted
- [ ] Test protected runtime is never evicted
- [ ] Test queue retry after eviction

### Verification
- [ ] Deploy always_on model
- [ ] Wait 30+ minutes with no traffic
- [ ] Verify NOT evicted: `SELECT * FROM agent_runtimes WHERE workload_policy='always_on';`

## Phase 4: Preemption Integration (Week 5)

### Scheduler Preemption
- [ ] Implement `Scheduler.tryPreemption(req, nodeID)`
  - [ ] Call `preemptor.PreemptForProject()`
  - [ ] Wait for preemption completion
  - [ ] Retry Decide()
  
- [ ] Integrate into `handleNoCapacity()`
  - [ ] Check if priority allows preemption
  - [ ] Try preemption first
  - [ ] Fall back to queue if fails

### Admission Control
- [ ] Load `project_configurations.admission_policy`
- [ ] Respect: `queue` | `preempt_then_queue` | `reject`

### Testing
- [ ] Fill node with LOW priority runtime
- [ ] Deploy CRITICAL model
- [ ] Verify: LOW preempted, CRITICAL deployed
  
- [ ] Fill node with HIGH priority runtime
- [ ] Deploy NORMAL model
- [ ] Verify: NORMAL queued (no preemption)

### Verification
- [ ] Check preemption events: `SELECT * FROM preemption_events ORDER BY created_at DESC LIMIT 5;`
- [ ] Verify priority rules enforced

## Phase 5: Multi-Node Discovery (Week 6)

### Node Auto-Registration
- [ ] Modify `/internal/nodeagent/agent.go`
  - [ ] On first heartbeat: INSERT INTO nodes if not exists
  - [ ] ON CONFLICT (hostname) DO UPDATE
  - [ ] Update status, agent_version, last_heartbeat_at

### Node Failure Handling
- [ ] Create `/internal/nodehealth/monitor.go` (if not exists)
  - [ ] Run every 60 seconds
  - [ ] Detect: `last_heartbeat_at > 90 seconds`
  - [ ] Mark: `UPDATE nodes SET status='offline'`
  - [ ] Mark: `UPDATE agent_runtimes SET state='lost'`
  
- [ ] Create `/internal/nodehealth/recovery.go`
  - [ ] Find `always_on` runtimes in `lost` state
  - [ ] Call `RuntimeActivator.StartModel()` to reschedule

### Scheduler Updates
- [ ] `loadCandidateNodes()` filters `status IN ('online','degraded')`
- [ ] Automatically discovers new nodes

### Testing
- [ ] Start new node → verify auto-registered
- [ ] Stop node → verify marked offline
- [ ] Verify lost runtimes rescheduled to healthy nodes

### Verification
- [ ] Check node count: `SELECT COUNT(*) FROM nodes WHERE status='online';`
- [ ] Add node: start agent on new server
- [ ] Deploy model: verify scheduler uses new node


## Phase 6: Requirements Computing (Week 7)

### Requirements Computer
- [ ] Create `/internal/scheduler/requirements_computer.go`
  - [ ] `ComputeRequirements(modelID) (*Requirements, error)`
  - [ ] llamacpp VRAM estimator (quantization + ctx_size)
  - [ ] vLLM VRAM estimator (model size + tensor_parallel)
  - [ ] TGI VRAM estimator
  - [ ] CPU/RAM estimators

### VRAM Estimators
- [ ] llamacpp: Parse quantization from filename
  - [ ] Q4_K_M: ~4.5 bits/param
  - [ ] Q5_K_M: ~5.5 bits/param
  - [ ] Q8_0: ~8 bits/param
  - [ ] Add KV cache overhead: `ctx_size * hidden_dim * 2 * bytes_per_element`
  
- [ ] vLLM: Estimate from HF model card
  - [ ] Load model config JSON
  - [ ] Calculate: `num_params * bytes_per_param`
  - [ ] Add KV cache: `ctx_size * num_layers * hidden_size * 2`

### Background Job
- [ ] Create `/internal/scheduler/requirements_job.go`
  - [ ] Run on startup: compute all models
  - [ ] Run periodically: re-compute every 24 hours
  - [ ] Cache in `model_requirements` table
  
- [ ] Cache Invalidation
  - [ ] Listen for model_runtime_configs updates
  - [ ] Re-compute on change

### Testing
- [ ] Test VRAM estimator accuracy
  - [ ] Deploy gemma-2-2b Q4_K_M
  - [ ] Measure actual VRAM usage
  - [ ] Compare to estimate (within 10%)
  
- [ ] Test cache hit rate
  - [ ] Check: `SELECT COUNT(*) FROM model_requirements;`
  - [ ] Verify populated for all models

### Verification
- [ ] Check requirements: `SELECT * FROM model_requirements WHERE model_id = '...';`
- [ ] Compare estimate to actual: `SELECT memory_used_mb FROM gpu_telemetry ...`

## Phase 7: Web UI Integration (Week 8)

### Models Page Updates
- [ ] Modify `/web/src/pages/ModelsPage.tsx`
  - [ ] Add "Placement" column
  - [ ] Show: node_hostname, gpu_devices, score
  - [ ] Link to placement decision detail
  
- [ ] Add PlacementBadge component
  - [ ] Show node with icon
  - [ ] Color: green=optimal, yellow=fallback, red=queued

### Scheduler Dashboard
- [ ] Create `/web/src/pages/SchedulerPage.tsx`
  - [ ] Queue table (pending deployments)
  - [ ] Recent decisions table
  - [ ] Node capacity chart (VRAM bar chart)
  - [ ] Metrics: queue_length, decisions_total, avg_score
  
- [ ] Create API endpoints
  - [ ] `GET /admin/v1/scheduler/queue`
  - [ ] `GET /admin/v1/scheduler/decisions`
  - [ ] `GET /admin/v1/scheduler/metrics`

### Manual Reschedule
- [ ] Add "Reschedule" button on ModelDetail page
- [ ] POST /admin/v1/models/:id/reschedule
- [ ] Stops runtime → clears node_id → next request triggers new placement

### Testing
- [ ] Deploy model with auto_place
- [ ] Check Models page shows placement info
- [ ] Open Scheduler dashboard
- [ ] Verify queue, decisions, metrics visible


## Monitoring & Observability

### Prometheus Metrics
- [ ] Add `/internal/scheduler/metrics.go`
  - [ ] `nexus_scheduler_decisions_total{outcome,decision_type}`
  - [ ] `nexus_scheduler_queue_length`
  - [ ] `nexus_placement_decision_duration_seconds`
  - [ ] `nexus_node_capacity_utilization{node_id,resource_type}`
  
- [ ] Instrument scheduler code
  - [ ] Increment decision counter
  - [ ] Observe placement latency
  - [ ] Update queue length gauge

### Grafana Dashboard
- [ ] Create "NexusLLM Scheduler" dashboard
  - [ ] Placement decision rate (rate(decisions_total[5m]))
  - [ ] Queue length over time
  - [ ] Placement latency p50/p95/p99
  - [ ] Node VRAM utilization heatmap
  - [ ] Deployment outcome pie chart

### Logging
- [ ] Structured logging for all decisions
  - [ ] Log level: INFO for success, WARN for queue, ERROR for reject
  - [ ] Fields: model_id, node_id, score, reason, latency
  
- [ ] Log queue operations
  - [ ] Enqueue, retry, success, failure

### Alerting
- [ ] Alert: Queue length > 10 for > 5 minutes
- [ ] Alert: Placement failures > 5% for > 10 minutes
- [ ] Alert: Node offline (no heartbeat > 2 minutes)

## Final Verification

### End-to-End Test
- [ ] Clean slate: Stop all runtimes
  ```sql
  UPDATE agent_runtimes SET state='stopped';
  ```
  
- [ ] User request cold model
  ```bash
  curl -X POST http://localhost:8080/v1/chat/completions \
    -d '{"model":"gemma-2","messages":[{"role":"user","content":"hi"}]}'
  ```
  
- [ ] Verify flow:
  - [ ] Gateway detects model not running
  - [ ] RuntimeActivator calls scheduler
  - [ ] Scheduler decides node
  - [ ] START_MODEL task enqueued
  - [ ] Agent executes pipeline
  - [ ] Model reaches READY state
  - [ ] Request routed to model
  - [ ] Response returned to user
  
- [ ] Check decision audit trail
  ```sql
  SELECT * FROM scheduler_decisions ORDER BY decided_at DESC LIMIT 1;
  ```

### Load Test
- [ ] Deploy 10 models concurrently with auto_place
- [ ] Verify all placed correctly
- [ ] Check decision latency < 100ms
- [ ] Verify no race conditions

### Failure Test
- [ ] Stop node mid-deployment
- [ ] Verify runtime marked `lost`
- [ ] Verify next request reschedules to healthy node

### Capacity Test
- [ ] Fill all nodes to capacity
- [ ] Deploy model → verify queued
- [ ] Stop one runtime
- [ ] Verify queue processor deploys

## Documentation Updates

- [ ] Update README.md with scheduler info
- [ ] Update deployment guide with auto_place
- [ ] Update troubleshooting guide
- [ ] Create operator runbook
- [ ] Update API documentation

## Production Readiness

- [ ] Performance testing (1000 concurrent placements)
- [ ] Security review (SQL injection, auth)
- [ ] Backup/restore procedures
- [ ] Rollback plan if issues
- [ ] Monitoring runbook
- [ ] On-call playbook

## Sign-Off

- [ ] Code review completed
- [ ] All tests passing
- [ ] Documentation reviewed
- [ ] Deployment plan approved
- [ ] Monitoring verified
- [ ] Ready for production

---

**Progress Tracking**: Mark items with ✅ when complete, 🚧 when in progress, ❌ when blocked.
