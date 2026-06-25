# NexusLLM Automatic Scheduler Flow Diagrams

## 1. Request Flow: Cold Start (Model Not Running)

```
┌─────────────┐
│    User     │
│             │
│  POST /chat │
│  model:     │
│  "gemma-2"  │
└──────┬──────┘
       │
       ▼
┌─────────────────────────────────────────────────────────┐
│                    Gateway                              │
│  1. Receive request                                     │
│  2. Check RuntimeRegistry                               │
│  3. Model "gemma-2" → NOT FOUND or STOPPED              │
└──────┬──────────────────────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────────────────────┐
│                RuntimeActivator                         │
│  1. EnsureRunning("gemma-2")                            │
│  2. Deduplicate concurrent requests (singleflight)      │
│  3. Load model config from DB                           │
│  4. node_id = empty? → CALL SCHEDULER                   │
└──────┬──────────────────────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────────────────────┐
│                   Scheduler.Decide()                    │
│                                                         │
│  Step 1: Build PlacementRequest                         │
│    • model_id, model_name                               │
│    • required_vram_mb, required_ram_mb, required_cpu    │
│    • execution_mode (cpu/gpu/auto)                      │
│    • priority, workload_policy                          │
│                                                         │
│  Step 2: Load Candidate Nodes                           │
│    SELECT * FROM nodes WHERE status IN ('online')       │
│    • Node-1: 8×GPU, 80GB VRAM, 256GB RAM               │
│    • Node-2: CPU-only, 128 cores, 512GB RAM            │
│                                                         │
│  Step 3: Filter by Requirements                         │
│    • Model needs GPU → filter out CPU-only nodes        │
│    • Check: free_vram >= required_vram                  │
│    • Check: free_ram >= required_ram                    │
│    → Node-1 passes                                      │
│                                                         │
│  Step 4: Score Candidates                               │
│    Node-1 Score:                                        │
│      Capacity:  +380 (free_vram=45GB, free_ram=200GB)  │
│      Load:      +250 (gpu_util=30%, runtime_count=2)   │
│      Locality:  +150 (model cached)                     │
│      Priority:  +100 (normal)                           │
│      TOTAL:     880                                     │
│                                                         │
│  Step 5: Select Best (highest score)                    │
│    Decision:                                            │
│      node_id = node-1                                   │
│      gpu_devices = [0]                                  │
│      cpu_set_cpus = "0-15"                              │
│      numa_node = 0                                      │
│      score = 880                                        │
└──────┬──────────────────────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────────────────────┐
│             RuntimeActivator (continued)                │
│  5. Apply placement decision                            │
│  6. Enqueue START_MODEL task:                           │
│     • node_id = node-1                                  │
│     • gpu_devices = [0]                                 │
│     • cpu_set_cpus = "0-15"                             │
│     • numa_node = 0                                     │
│     • payload = full model config                       │
└──────┬──────────────────────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────────────────────┐
│              TaskManager (Control Plane)                │
│  1. Insert task into agent_tasks table                  │
│  2. Task status = 'pending'                             │
│  3. Wait for node agent to poll                         │
└──────┬──────────────────────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────────────────────┐
│              Node Agent (Node-1)                        │
│  1. Poll: SELECT * FROM agent_tasks                     │
│           WHERE node_id = 'node-1' AND status='pending' │
│  2. Claim task (UPDATE status='claimed')                │
│  3. Execute START_MODEL pipeline:                       │
│                                                         │
│     pending → validating → downloading →                │
│     starting → loading_model → waiting_ready → ready    │
│                                                         │
│  4. Validation: check image, port, GPU available        │
│  5. Download: fetch GGUF from HuggingFace (if needed)   │
│  6. Start: docker run --gpus '"device=0"' ...          │
│  7. Loading: model weights load into GPU VRAM           │
│  8. Health check: GET http://node-1:8080/health         │
│     • Connection refused → EXPECTED (loading)           │
│     • HTTP 200 → READY                                  │
│  9. Update agent_runtimes: state = 'ready'              │
└──────┬──────────────────────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────────────────────┐
│              RuntimeRegistry (Gateway)                  │
│  1. Reload: SELECT * FROM agent_runtimes                │
│            WHERE state = 'ready'                        │
│  2. Register endpoint:                                  │
│     "gemma-2" → "http://node-1:8080"                    │
│  3. Enable routing                                      │
└──────┬──────────────────────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────────────────────┐
│              Gateway (continued)                        │
│  4. Forward request to http://node-1:8080/v1/chat       │
└──────┬──────────────────────────────────────────────────┘
       │
       ▼
┌─────────────┐
│  Response   │
│  to User    │
└─────────────┘

Total Latency: ~5-30 seconds (cold start)
```


## 2. Capacity Exhausted → Queue

```
┌─────────────┐
│   Admin     │
│  Deploy     │
│  qwen3-32b  │
│ (requires   │
│  80GB VRAM) │
└──────┬──────┘
       │
       ▼
┌─────────────────────────────────────────────────────────┐
│                Scheduler.Decide()                       │
│                                                         │
│  Step 1: Load Candidate Nodes                           │
│    Node-1: total_vram=80GB, free_vram=10GB             │
│    Node-2: total_vram=80GB, free_vram=20GB             │
│                                                         │
│  Step 2: Filter by Requirements                         │
│    required_vram=80GB                                   │
│    Node-1: 10GB < 80GB → FAIL                          │
│    Node-2: 20GB < 80GB → FAIL                          │
│    → NO CANDIDATES                                      │
│                                                         │
│  Step 3: Handle Insufficient Capacity                   │
│    Check priority: normal                               │
│    Check admission_policy: queue (default)              │
│    Decision: QUEUE                                      │
└──────┬──────────────────────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────────────────────┐
│              Scheduler.Queue()                          │
│  1. INSERT INTO deployment_queue                        │
│     • runtime_config = model config JSON                │
│     • required_vram_mb = 80000                          │
│     • priority_score = 50 (normal)                      │
│     • status = 'pending'                                │
│     • enqueued_at = NOW()                               │
│  2. Return queue_id                                     │
└──────┬──────────────────────────────────────────────────┘
       │
       ▼
┌─────────────┐
│   Admin     │
│  Response:  │
│  {          │
│   "status": │
│    "queued" │
│   "queue_   │
│    id": ... │
│  }          │
└─────────────┘
       │
       │ (30 seconds later, queue processor runs)
       ▼
┌─────────────────────────────────────────────────────────┐
│          Scheduler.processQueue()                       │
│  1. SELECT * FROM deployment_queue                      │
│     WHERE status='pending' ORDER BY priority_score DESC │
│  2. For each queued item:                               │
│     • Try Decide() again                                │
│     • If success → deploy                               │
│     • If still no capacity → exponential backoff        │
│       next_retry_at = NOW() + 2^attempts * 30s          │
└─────────────────────────────────────────────────────────┘
```


## 3. High Priority → Preemption

```
┌─────────────┐
│   Admin     │
│  Deploy     │
│  CRITICAL   │
│  model      │
└──────┬──────┘
       │
       ▼
┌─────────────────────────────────────────────────────────┐
│            Scheduler.Decide()                           │
│  Step 1: Check capacity → NONE AVAILABLE                │
│  Step 2: Check priority → CRITICAL                      │
│  Step 3: Check admission_policy → preempt_then_queue    │
│  Step 4: Call Preemptor                                 │
└──────┬──────────────────────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────────────────────┐
│         PreemptionEngine.PreemptForProject()            │
│                                                         │
│  Step 1: Find victim on target node                     │
│    SELECT * FROM agent_runtimes ar                      │
│    JOIN projects p ON p.id = ar.project_id             │
│    WHERE ar.node_id = 'node-1'                          │
│      AND ar.state IN ('ready','active','warm')          │
│      AND p.priority IN ('LOW','BEST_EFFORT')           │
│    ORDER BY priority_score ASC,                         │
│             last_used_at ASC                            │
│    LIMIT 1                                              │
│                                                         │
│    → Found: runtime_id=123, priority=LOW                │
│                                                         │
│  Step 2: Verify preemption rules                        │
│    CRITICAL.CanPreempt(LOW) → TRUE                      │
│                                                         │
│  Step 3: Execute preemption                             │
│    • UPDATE agent_runtimes SET state='stopping'         │
│    • UPDATE model_endpoints SET is_enabled=FALSE        │
│    • Enqueue STOP_RUNTIME task                          │
│    • Wait for task completion (poll)                    │
│                                                         │
│  Step 4: Record preemption event                        │
│    INSERT INTO preemption_events (                      │
│      preempted_runtime_id = 123,                        │
│      preempted_priority = 'LOW',                        │
│      requesting_priority = 'CRITICAL',                  │
│      trigger = 'admission'                              │
│    )                                                    │
└──────┬──────────────────────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────────────────────┐
│           Scheduler.Decide() (retry)                    │
│  Step 1: Load nodes (capacity now available)            │
│  Step 2: Select node-1                                  │
│  Step 3: Return PlacementDecision                       │
└──────┬──────────────────────────────────────────────────┘
       │
       ▼
┌─────────────┐
│  Deploy     │
│  CRITICAL   │
│  model to   │
│  node-1     │
└─────────────┘
```


## 4. Node Failure → Reschedule

```
┌─────────────┐           ┌─────────────┐
│   Node-1    │           │   Node-2    │
│  (running   │           │  (online)   │
│   gemma-2)  │           │             │
└──────┬──────┘           └─────────────┘
       │
       X  (crash / network failure)
       
       │ (90 seconds pass, no heartbeat)
       ▼
┌─────────────────────────────────────────────────────────┐
│            Control Plane (NodeHealth Monitor)           │
│  1. Detect: last_heartbeat_at > 90 seconds              │
│  2. UPDATE nodes SET status='offline' WHERE id='node-1' │
│  3. UPDATE agent_runtimes SET state='lost'              │
│     WHERE node_id='node-1'                              │
└──────┬──────────────────────────────────────────────────┘
       │
       │ (User makes request to gemma-2)
       ▼
┌─────────────────────────────────────────────────────────┐
│              RuntimeActivator.EnsureRunning()           │
│  1. Resolve "gemma-2" → state='lost'                    │
│  2. Treat as cold start                                 │
│  3. Call Scheduler.Decide()                             │
└──────┬──────────────────────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────────────────────┐
│              Scheduler.Decide()                         │
│  1. Load candidate nodes                                │
│     • Node-1: status='offline' → EXCLUDE                │
│     • Node-2: status='online' → INCLUDE                 │
│  2. Filter & score                                      │
│  3. Decision: node_id = node-2                          │
└──────┬──────────────────────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────────────────────┐
│          RuntimeActivator (continued)                   │
│  4. Enqueue START_MODEL to node-2                       │
│  5. Node-2 agent executes pipeline                      │
│  6. gemma-2 now running on node-2                       │
└──────┬──────────────────────────────────────────────────┘
       │
       ▼
┌─────────────┐
│  User gets  │
│  response   │
│  (from      │
│   Node-2)   │
└─────────────┘
```

## 5. Idle Eviction → Free Capacity

```
┌─────────────────────────────────────────────────────────┐
│              IdleManager.Sweep()                        │
│  Runs every 30 seconds                                  │
│                                                         │
│  1. Find idle runtimes:                                 │
│     SELECT * FROM agent_runtimes                        │
│     WHERE state IN ('ready','active','idle')            │
│       AND workload_policy = 'lazy_load'                 │
│       AND last_used_at < NOW() - INTERVAL '30 minutes'  │
│       AND NOT EXISTS (                                  │
│         SELECT 1 FROM project_configurations            │
│         WHERE always_running=TRUE OR protected=TRUE     │
│       )                                                 │
│                                                         │
│     → Found: runtime_id=456, model="gemma-2"            │
│        last_used_at = 35 minutes ago                    │
│                                                         │
│  2. Enqueue UNLOAD_RUNTIME task                         │
│  3. Node agent stops container                          │
│  4. UPDATE agent_runtimes SET state='stopped'           │
│  5. Free VRAM: 8GB                                      │
└──────┬──────────────────────────────────────────────────┘
       │
       │ (Idle eviction freed capacity)
       ▼
┌─────────────────────────────────────────────────────────┐
│         Scheduler.processQueue()                        │
│  1. Retry queued deployments                            │
│  2. Check capacity → NOW AVAILABLE                      │
│  3. Deploy queued item                                  │
└─────────────────────────────────────────────────────────┘
```

## Summary

The scheduler automatically handles:
- ✅ **Cold starts**: Select best node based on capacity + load + locality
- ✅ **Capacity exhaustion**: Queue deployments with exponential backoff
- ✅ **High priority**: Preempt low-priority runtimes when needed
- ✅ **Node failures**: Reschedule to healthy nodes automatically
- ✅ **Idle eviction**: Free resources for queued deployments

**Users never interact with nodes directly** — they only request models by name, and the platform handles everything automatically.
