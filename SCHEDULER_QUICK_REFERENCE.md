# NexusLLM Scheduler Quick Reference

## Key Concepts

### Runtime States
```
pending → validating → downloading → starting → 
loading_model → waiting_ready → ready
```

### Priorities (High to Low)
```
CRITICAL > HIGH > NORMAL > LOW > BEST_EFFORT
```

### Execution Modes
- `cpu`: CPU-only (n_gpu_layers=0)
- `gpu`: Requires GPU
- `auto`: Use GPU if available, else CPU

### Workload Policies
- `lazy_load`: Auto-start on request, auto-stop when idle (default for LLMs)
- `always_on`: Start on deploy, never idle-evict, restart on crash (for services)

## Scoring Formula

```
Node Score = CapacityScore + LoadScore + LocalityScore + PriorityBonus

CapacityScore (0-400):
  Free VRAM above requirement: +200
  Free RAM above requirement:  +100
  Free CPU cores:              +100

LoadScore (0-300):
  GPU utilization < 50%:       +150
  Runtime density low:         +100
  Health status healthy:       +50

LocalityScore (0-200):
  Model weights cached:        +150
  NUMA locality match:         +50

PriorityBonus (0-200):
  CRITICAL:                    +200
  HIGH:                        +150
  NORMAL:                      +100
  LOW:                         +50
  BEST_EFFORT:                 0
```


## Capacity Management Decision Matrix

| Priority     | Capacity  | Action                           |
|--------------|-----------|----------------------------------|
| CRITICAL     | Any       | Preempt → Queue if fails         |
| HIGH         | LOW/BEST  | Preempt → Queue if fails         |
| HIGH         | HIGH/CRIT | Queue                            |
| NORMAL       | LOW/BEST  | Preempt → Queue if fails         |
| NORMAL       | NORMAL+   | Queue                            |
| LOW          | BEST      | Preempt → Queue if fails         |
| LOW          | LOW+      | Queue                            |
| BEST_EFFORT  | Any       | Queue (no preemption)            |

## Common CLI Commands

### Apply Migration
```bash
psql nexusllm < migrations/017_scheduler.sql
```

### Check Node Status
```sql
SELECT id, hostname, status, total_vram_mb, last_heartbeat_at 
FROM nodes;
```

### Check Available VRAM
```sql
SELECT node_id, hostname, scheduler_available_vram_mb(id) AS free_vram_mb
FROM nodes;
```

### View Queue
```sql
SELECT id, priority_score, required_vram_mb, status, enqueued_at, attempts
FROM deployment_queue
WHERE status = 'pending'
ORDER BY priority_score DESC;
```

### View Recent Decisions
```sql
SELECT model_id, decision_type, node_id, score, reason, outcome, decided_at
FROM scheduler_decisions
ORDER BY decided_at DESC
LIMIT 10;
```

### Check Idle Runtimes
```sql
SELECT ar.id, m.name, ar.state, ar.last_used_at,
       EXTRACT(EPOCH FROM (NOW() - ar.last_used_at))/60 AS idle_minutes
FROM agent_runtimes ar
JOIN models m ON m.id = ar.model_id
WHERE ar.state IN ('ready', 'active', 'idle')
  AND ar.workload_policy = 'lazy_load'
  AND ar.last_used_at < NOW() - INTERVAL '30 minutes'
ORDER BY ar.last_used_at ASC;
```


## API Quick Reference

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

### Check Queue Status
```bash
curl http://localhost:3000/admin/v1/scheduler/queue
```

### View Placement Decisions
```bash
curl http://localhost:3000/admin/v1/scheduler/decisions?limit=10
```

### View Node Metrics
```bash
curl http://localhost:3000/admin/v1/nodes
```

## Troubleshooting

### All deployments queue
**Cause**: No nodes online or insufficient capacity

**Check**:
```sql
SELECT * FROM scheduler_node_metrics;
```

**Fix**: Start node agent or add nodes

### Wrong node selected
**Cause**: Stale telemetry or scoring issue

**Check**:
```sql
SELECT alternatives FROM scheduler_decisions
WHERE id = '<decision_id>';
```

**Fix**: Verify node agent is reporting metrics

### Preemption not working
**Cause**: Priority rules violated or protected runtime

**Check**:
```sql
SELECT ar.id, m.name, COALESCE(p.priority, 'NORMAL') AS priority,
       COALESCE(pc.protected, FALSE) AS protected
FROM agent_runtimes ar
JOIN models m ON m.id = ar.model_id
LEFT JOIN projects p ON p.id = ar.project_id
LEFT JOIN project_configurations pc ON pc.project_id = ar.project_id
WHERE ar.state IN ('ready', 'active', 'warm');
```

**Fix**: Verify priority rules and protected flag

## Files Reference

### Core Implementation
- `/internal/scheduler/scheduler.go` - Main scheduler
- `/internal/scheduler/types.go` - Type definitions
- `/internal/scheduler/placer.go` - Placement engine (TODO)
- `/internal/scheduler/scorer.go` - Scoring logic (TODO)

### Documentation
- `/docs/20-automatic-scheduler.md` - Complete architecture
- `/docs/SCHEDULER_FLOW_DIAGRAM.md` - Visual flows
- `/SCHEDULER_IMPLEMENTATION.md` - Implementation guide
- `/SCHEDULER_QUICK_REFERENCE.md` - This file

### Database
- `/migrations/017_scheduler.sql` - Scheduler schema

## Key Metrics

### Prometheus Metrics
- `nexus_scheduler_decisions_total{outcome,decision_type}`
- `nexus_scheduler_queue_length`
- `nexus_placement_decision_duration_seconds`
- `nexus_node_capacity_utilization{node_id,resource_type}`

### Grafana Queries
```promql
# Placement decision rate
rate(nexus_scheduler_decisions_total[5m])

# Queue length
nexus_scheduler_queue_length

# Placement latency p95
histogram_quantile(0.95, nexus_placement_decision_duration_seconds)

# Node VRAM utilization
nexus_node_capacity_utilization{resource_type="vram"}
```

---

**For detailed information**, see `/docs/20-automatic-scheduler.md`
