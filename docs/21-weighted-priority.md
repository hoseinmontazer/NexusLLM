# Weighted Priority & Scheduling

NexusLLM uses a **continuous numeric priority weight** (`priority_weight`) instead of fixed tiers.
Every scheduling decision — node selection, preemption, queue ordering — uses the computed
**effective priority**, not the raw weight.

---

## Why Weighted, Not Enum?

The old 5-tier system (CRITICAL / HIGH / NORMAL / LOW / BEST_EFFORT) had no ordering within
a tier. Ten projects all marked CRITICAL had identical scheduling priority — the scheduler
still needed a tiebreaker, which defaulted to FIFO. This breaks SLA guarantees when multiple
high-priority workloads compete.

The weighted model gives every project a unique, tunable position on a 0–1000 scale.

---

## Priority Weight Scale

`priority_weight` is an integer in `[0, 1000]`. Higher = scheduled sooner and harder to preempt.

| Weight | Canonical label | Typical use case |
|---|---|---|
| 1000 | Emergency | Incident response — scheduled immediately above everything |
| 950 | Customer Production Chat | Revenue-generating user-facing chat |
| 900 | Revenue Critical Services | Core inference pipelines, SLA-bound APIs |
| 800 | Core Internal Services | Internal tools that affect productivity |
| 700 | Core Internal (lower) | Supporting internal services |
| 500 | Standard Business Workloads | Default for new projects |
| 300 | Batch Processing | Offline jobs, nightly re-indexing |
| 100 | Development | Dev/test environments |
| 50 | Playground | Experimentation with no SLA |
| 0 | Best Effort | Background tasks, can be preempted by anyone |

These labels are **display-only**. The scheduler only reads the integer value.

---

## Effective Priority

The **effective priority** is what the scheduler actually uses. It adjusts the base weight
in real time to prevent starvation and reward good citizens:

```
effective_priority = base_weight
                   + waiting_bonus      (anti-starvation aging)
                   + reservation_bonus  (has guaranteed quota)
                   - resource_penalty   (consuming beyond max quota)

Clamped to [0, 1000].
```

### Waiting Bonus (Anti-starvation)

Low-priority jobs that have been waiting a long time gradually gain priority.
This prevents a high-priority flood from permanently starving lower tiers.

```
waiting_bonus = floor(wait_seconds / 60)   max: +200
```

A job waiting 3 hours gains +180 waiting bonus. A Best Effort job (weight 0)
that waits long enough can eventually displace a Standard job (weight 500)
if it accumulates the full +200 bonus and the Standard job has no waiting bonus.

### Reservation Bonus

Projects with configured **resource reservations** (`reserved_vram_mb > 0` or
`reserved_cpu_cores > 0`) receive +50 bonus. This rewards projects that have
pre-declared their needs, helping the scheduler plan ahead.

### Resource Penalty

Projects consuming **beyond their max quota** (`max_gpu_vram_mb`, `max_cpu`, `max_memory_mb`)
receive a −100 penalty. This prevents quota-overrunning projects from unfairly outranking
well-behaved peers.

### Example breakdown (UI display)

```
Project: customer-chat
──────────────────────────────
Base Priority:      900
Waiting Bonus:      +50   (50 min in queue)
Reservation Bonus:  +50   (80 GB VRAM reserved)
Resource Penalty:   -20   (slightly over CPU quota)
──────────────────────────────
Effective Priority: 980
```

---

## Preemption Rules

Preemption stops a running runtime to free resources for a higher-priority deployment.

**Rule:** `requester.effective_priority - victim.priority_weight ≥ 50`

The gap of 50 prevents thrashing between near-equal projects. A project at 600 cannot
preempt one at 560 — they are treated as peers.

**Non-preemptible flag:** Projects with `preemptible=false` or `protected=true` are
never selected as preemption victims, regardless of priority gap.

**Preemption never targets VRAM-reserved runtimes** when their reserved quota is in use
and a higher-weight project on the same node needs the VRAM — instead the reservation
guarantee is enforced.

---

## API Reference

### Create a project with a specific weight

```bash
curl -X POST http://localhost:8081/admin/v1/projects \
  -H 'Content-Type: application/json' \
  -d '{
    "organization_id": "ORG_ID",
    "team_id":         "TEAM_ID",
    "name":            "Customer Chat",
    "priority_weight": 900,
    "preemptible":     false
  }'
```

Response includes `priority_label`, `effective_priority`, and all breakdown fields.

### Change priority at runtime

```bash
curl -X POST http://localhost:8081/admin/v1/projects/PROJECT_ID/priority \
  -H 'Content-Type: application/json' \
  -d '{"priority_weight": 950}'
```

Response:
```json
{
  "message":             "priority updated",
  "old_priority_weight": 900,
  "new_priority_weight": 950,
  "new_priority_label":  "Production Critical",
  "changed":             true
}
```

The change takes effect on the next scheduler cycle (within ~60 seconds).
Every priority change is written to `audit_logs`.

### Set resource reservations and quotas

```bash
curl -X POST http://localhost:8081/admin/v1/projects/PROJECT_ID/reserve \
  -H 'Content-Type: application/json' \
  -d '{
    "reserved_vram_mb":   81920,
    "reserved_cpu_cores": 16,
    "reserved_memory_mb": 65536,
    "max_gpu_vram_mb":    163840,
    "max_cpu":            64,
    "max_memory_mb":      131072
  }'
```

`reserved_*` = guaranteed minimum (held back from lower-priority projects).
`max_*` = ceiling quota (consuming beyond triggers `resource_penalty`).

### View effective priority breakdown

```bash
curl http://localhost:8081/admin/v1/projects/PROJECT_ID
```

Response includes:
```json
{
  "priority_weight":    900,
  "priority_label":     "Production Critical",
  "effective_priority": 980,
  "waiting_bonus":      50,
  "reservation_bonus":  50,
  "resource_penalty":   20
}
```

### View scheduler queue (all projects, globally ordered)

```bash
curl http://localhost:8081/admin/v1/scheduler/queue
```

Returns pending deployments ordered by `effective_priority DESC, waiting_since ASC`.

Each row includes: `model_name`, `priority_weight`, `effective_priority`,
`required_vram_mb`, `required_ram_mb`, `required_cpu`, `waiting_since`,
`attempts`, `preemption_reason`.

### View placement decisions with trace

```bash
curl http://localhost:8081/admin/v1/scheduler/decisions?limit=20
```

Each decision includes:
- `decision_type`: `placement | preemption | queue | reject | reschedule`
- `priority_weight`, `effective_priority`, `waiting_bonus`, `reservation_bonus`, `resource_penalty`
- `node_score`: the score of the selected node
- `reason`: human-readable explanation
- `decision_trace`: full JSON trace with all candidate nodes and their scores

### Get priority presets (for UI)

```bash
curl http://localhost:8081/admin/v1/scheduler/priority-presets
```

Returns the canonical preset list for UI priority pickers.

### View a project's deployment queue

```bash
curl http://localhost:8081/admin/v1/projects/PROJECT_ID/queue
```

### View preemption history (with numeric weights)

```bash
curl http://localhost:8081/admin/v1/projects/PROJECT_ID/preemptions
```

Returns events with `preempted_weight` and `requesting_weight` as integers.

---

## Mark a project non-preemptible

```bash
# Set preemptible=false at creation
curl -X POST http://localhost:8081/admin/v1/projects \
  -d '{"name":"...", "priority_weight":900, "preemptible":false}'

# Or update existing project
curl -X PUT http://localhost:8081/admin/v1/projects/PROJECT_ID \
  -d '{"preemptible":false}'

# Or use protection panel (prevents idle eviction AND preemption)
curl -X PUT http://localhost:8081/admin/v1/projects/PROJECT_ID/protection \
  -d '{"protected":true, "always_running":true}'
```

---

## Admission Policy

When a deployment cannot be placed immediately, the project's `admission_policy` governs what happens:

| Policy | Behaviour |
|---|---|
| `queue` | Add to `deployment_queue`; retry every 30 s with exponential backoff |
| `preempt_then_queue` | Attempt preemption first; queue if preemption fails or is not allowed |
| `reject` | Return HTTP 409 immediately — no queuing |

```bash
curl -X PUT http://localhost:8081/admin/v1/projects/PROJECT_ID/protection \
  -d '{"admission_policy":"preempt_then_queue"}'
```

---

## Database Tables

| Table | Purpose |
|---|---|
| `projects.priority_weight` | Base scheduling weight [0–1000] |
| `projects.preemptible` | Whether this project can be a preemption victim |
| `project_effective_priority` | Cached effective priority with component breakdown |
| `project_reservations` | Guaranteed minimums + max quotas per project |
| `deployment_queue` | Pending deployments ordered by `effective_priority DESC` |
| `scheduler_decisions` | Full audit trail of every placement decision |
| `preemption_events` | Preemption history with numeric `preempted_weight` / `requesting_weight` |

---

## SQL Helper Functions

These functions are available in PostgreSQL after migration 018:

```sql
-- Get human-readable label for a weight
SELECT priority_label(900);         -- → 'Production Critical'
SELECT priority_label(500);         -- → 'Standard'

-- Compute effective priority
SELECT compute_effective_priority(
    900,    -- base_weight
    3600,   -- waiting_secs (1 hour = +60 bonus)
    true,   -- has_reservation (+50)
    false   -- over_quota (no penalty)
);
-- → 1000 (clamped: 900 + 60 + 50 = 1010 → 1000)

-- Live effective priority for all projects
SELECT p.name, p.priority_weight, ep.effective_priority,
       ep.waiting_bonus, ep.reservation_bonus, ep.resource_penalty
FROM projects p
JOIN project_effective_priority ep ON ep.project_id = p.id
ORDER BY ep.effective_priority DESC;
```

---

## Migration Notes

Migration `018_weighted_priority.sql` handles the transition from the old enum system:

| Old enum | New weight |
|---|---|
| `CRITICAL` | 900 |
| `HIGH` | 700 |
| `NORMAL` | 500 |
| `LOW` | 300 |
| `BEST_EFFORT` | 0 |

The old `priority` VARCHAR column is kept (with its CHECK constraint removed) for one
release as a soft alias, then dropped in migration 019. All application code uses
`priority_weight` exclusively.

---

## Prometheus Metrics

| Metric | Labels | Description |
|---|---|---|
| `nexus_project_active_runtimes` | `project_id`, `project_name`, `priority_weight`, `priority_label` | Active runtimes per project |
| `nexus_project_effective_priority` | `project_id`, `project_name` | Live effective priority value |
| `nexus_scheduler_queue_length` | — | Number of pending deployments |
| `nexus_scheduler_decisions_total` | `outcome`, `decision_type` | Decision counter |
| `nexus_placement_decision_duration_seconds` | — | Latency histogram |

---

*Related: [Automatic Scheduler](20-automatic-scheduler.md) | [Policies](12-policies.md) | [Projects & Teams](04-orgs-and-teams.md)*
