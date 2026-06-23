# Requirements Document

## Introduction

NexusLLM currently organizes workloads in a two-level hierarchy: **Organization → Team → Models → Runtimes**. As enterprise customers scale their AI usage, teams need to separate workloads by business domain, enforce different SLAs per workload class, and ensure that mission-critical pipelines are never starved by batch jobs or experimental services running on the same GPU cluster.

This feature introduces **Project** as a first-class hierarchy level sitting between Team and Models, giving operators fine-grained control over resource reservation, scheduling priority, and runtime preemption. The resulting hierarchy becomes:

**Organization → Team → Project → Models → Runtimes**

A Project carries a named **priority level** (CRITICAL, HIGH, NORMAL, LOW, BEST_EFFORT) and may declare **reserved resources** (VRAM, CPU, memory). The NexusLLM scheduler and runtime manager enforce these contracts at allocation time and under resource pressure, including automatic preemption of lower-priority runtimes when a higher-priority project cannot be served.

---

## Glossary

- **Project**: A named workload grouping that belongs to one Team (and transitively to one Organization). It is the unit of SLA enforcement, resource reservation, and priority scheduling.
- **Priority_Level**: One of five ordered tiers — CRITICAL > HIGH > NORMAL > LOW > BEST_EFFORT — assigned to a Project and used by the Scheduler and Preemption_Engine.
- **Reservation**: A per-Project declaration of minimum resources (reserved_vram_mb, reserved_cpu_cores, reserved_memory_mb) that the Scheduler guarantees cannot be consumed by lower-priority Projects.
- **Preemption**: The act of stopping one or more running Runtimes belonging to a lower-priority Project so that a higher-priority Project's Runtime can be deployed.
- **Protected_Runtime**: A Runtime whose project sets `always_running = true`, `minimum_replicas > 0`, or `protected = true`. The Idle_Manager and Preemption_Engine must never unload a Protected_Runtime automatically.
- **Deployment_Queue**: A per-Project queue holding pending Runtime deployment requests that cannot be immediately scheduled due to insufficient resources.
- **Preemption_Engine**: The NexusLLM subsystem that selects candidate Runtimes to evict when resource pressure is detected or a higher-priority deployment request cannot otherwise be satisfied.
- **Idle_Manager**: The existing subsystem (`runtimemgr.IdleManager`) that stops idle Runtimes after their configured timeout expires.
- **Scheduler**: The NexusLLM component that decides node, GPU, and CPU placement for Runtime deployments. It currently lives in `internal/placement` and dispatches tasks via `internal/taskmanager`.
- **Resource_Guard**: The existing `runtimemgr.ResourceGuard` that checks RAM and GPU headroom before starting a container.
- **Usage_Tracker**: The existing `usage.Tracker` that records inference events and aggregates billing data.
- **Audit_Log**: The existing `audit_logs` table that records admin actions.
- **Node_Telemetry**: Real-time GPU utilization and memory readings reported by node agents and stored in `gpu_telemetry` and `node_telemetry` tables.

---

## Requirements

### Requirement 1: Project Entity CRUD

**User Story:** As a platform administrator, I want to create, read, update, and delete Projects within a Team, so that I can organize workloads by domain and assign each domain its own SLA tier.

#### Acceptance Criteria

1. THE Project_Service SHALL expose the following Admin API endpoints: `POST /admin/v1/projects`, `GET /admin/v1/projects`, `GET /admin/v1/projects/{id}`, `PUT /admin/v1/projects/{id}`, and `DELETE /admin/v1/projects/{id}`.
2. WHEN creating a Project, THE Project_Service SHALL persist the fields: `id` (UUID), `organization_id`, `team_id`, `name` (max 200 characters), `description` (max 1000 characters), `priority`, `status`, `created_at`, and `updated_at`.
3. WHEN creating a Project, IF the `team_id` does not reference an existing active Team in the same `organization_id`, THEN THE Project_Service SHALL return HTTP 422 with an error indicating the parent Team was not found.
4. WHEN creating a Project, IF a Project with the same `name` already exists within the same `team_id`, THEN THE Project_Service SHALL return HTTP 409 with an error indicating the name conflict.
5. WHEN creating or updating a Project, THE Project_Service SHALL accept a `priority` value of exactly one of: `CRITICAL`, `HIGH`, `NORMAL`, `LOW`, or `BEST_EFFORT`; IF an unsupported value is supplied, THEN THE Project_Service SHALL return HTTP 400.
6. WHEN a Project is created without an explicit `priority`, THE Project_Service SHALL default the priority to `NORMAL`.
7. WHEN a Project is created without an explicit `status`, THE Project_Service SHALL default the status to `active`; valid `status` values are `active`, `inactive`, and `archived`; IF an unsupported value is supplied, THEN THE Project_Service SHALL return HTTP 400.
8. WHEN deleting a Project that still has Models or Runtimes associated with it, THE Project_Service SHALL return HTTP 409 and SHALL NOT delete the Project row.
9. WHEN updating a Project's `priority`, THE Project_Service SHALL record the change in the `audit_logs` table with action `project.priority_changed`, including the previous and new priority values in the metadata.
10. THE Project_Service SHALL support filtering the project list by `org_id`, `team_id`, `priority`, and `status` query parameters.
11. WHEN `GET /admin/v1/projects/{id}` or `PUT /admin/v1/projects/{id}` is called with an `id` that does not reference an existing Project, THE Project_Service SHALL return HTTP 404 with an error indicating the Project was not found.
12. WHEN `GET /admin/v1/projects` is called with filters that match no Projects, THE Project_Service SHALL return HTTP 200 with an empty list.

---

### Requirement 2: Project–Model and Project–Runtime Association

**User Story:** As a platform administrator, I want to associate Models and Runtimes with a specific Project, so that the scheduler can enforce per-project SLAs when allocating compute resources.

#### Acceptance Criteria

1. THE Project_Service SHALL support a nullable `project_id` foreign key association on the `models` table and on the `agent_runtimes` table, enforced at the application layer such that only an existing active Project's `id` may be assigned.
2. WHEN a Model is created via `POST /admin/v1/models/deploy` and a `project_id` is provided, IF the referenced Project does not exist or is not active, THEN THE Project_Service SHALL return HTTP 422 with an error indicating the invalid Project reference; IF the referenced Project is valid, THE Project_Service SHALL set `models.project_id` to the supplied value; IF `project_id` is omitted, THE Project_Service SHALL leave `models.project_id` as NULL.
3. WHEN a Runtime is deployed for a Model that has a non-null `project_id`, IF the referenced Project still exists and is active, THEN THE Project_Service SHALL propagate `project_id` to the `agent_runtimes` row at deployment time; IF the Project no longer exists, THE Project_Service SHALL set `agent_runtimes.project_id` to NULL and proceed with deployment.
4. THE Project_Service SHALL expose `GET /admin/v1/projects/{id}/runtimes` which returns all `agent_runtimes` rows associated with that Project, including the fields: `id`, `model_id`, `state`, `node_id`, `gpu_ids`, `bind_host`, `bind_port`, `reserved_vram_mb`, `reserved_memory_mb`; IF no Runtimes are associated, THE Project_Service SHALL return HTTP 200 with an empty list.
5. WHEN a Runtime's `agent_runtimes.project_id` is set, THE Scheduler SHALL apply the associated Project's `priority` and `reservation` as inputs to its placement decision, evaluated in the order defined in Requirement 4.

---

### Requirement 3: Resource Reservation

**User Story:** As a platform administrator, I want to declare minimum reserved resources (VRAM, CPU, memory) for a Project, so that higher-priority workloads are always guaranteed their compute budget and are not starved by lower-priority projects.

#### Acceptance Criteria

1. WHEN `POST /admin/v1/projects/{id}/reserve` is called, THE Project_Service SHALL accept integer fields `reserved_vram_mb`, `reserved_cpu_cores`, and `reserved_memory_mb` (each ≥ 0, where 0 means no reservation); IF any field is negative or non-integer, THE Project_Service SHALL return HTTP 400; THE Project_Service SHALL upsert the reservation row (create on first call, update on subsequent calls).
2. WHEN the Scheduler calculates available VRAM on a specific node for a new Runtime deployment, THE Scheduler SHALL subtract the sum of `reserved_vram_mb` for all Projects with higher or equal priority that have active Runtimes on that node from the raw free VRAM reported by Node_Telemetry for that node.
3. WHEN the Scheduler places a Runtime for a CRITICAL or HIGH priority Project that has a Reservation, THE Scheduler SHALL treat any lower-priority Projects' reserved VRAM on that node as available headroom, ensuring lower-priority Reservations do not block the placement.
4. IF a reservation update would cause the total `reserved_vram_mb` across all Projects in the organization to exceed the sum of `vram_mb` for all nodes with `status = 'active'`, THEN THE Project_Service SHALL return HTTP 422 with a capacity-exceeded error and SHALL NOT persist the update.
5. THE Project_Service SHALL store reservations in a dedicated `project_reservations` table with columns: `project_id`, `reserved_vram_mb`, `reserved_cpu_cores`, `reserved_memory_mb`, `updated_at`.
6. WHEN a Project is deleted, THE Project_Service SHALL delete its associated `project_reservations` row.

---

### Requirement 4: Priority-Aware Scheduling

**User Story:** As an infrastructure engineer, I want the scheduler to place Runtimes in strict priority order across all active Projects, so that CRITICAL workloads always get first access to available GPUs.

#### Acceptance Criteria

1. WHEN the Scheduler evaluates placement candidates for a new Runtime deployment, THE Scheduler SHALL sort pending deployment requests by the associated Project's `priority` score (CRITICAL=100, HIGH=75, NORMAL=50, LOW=25, BEST_EFFORT=10) in descending order before assigning nodes.
2. WHEN evaluating placement, THE Scheduler SHALL consider the following factors as successive tiebreakers in this order: (1) Project Priority score, (2) Project Reserved Resources availability, (3) Team Quotas, (4) Organization Quotas, (5) Node Capacity; each subsequent factor is applied only when the preceding factor produces equal results.
3. WHEN two deployment requests have the same Project priority, THE Scheduler SHALL break the tie by `created_at` ascending (FIFO within the same tier).
4. WHEN a CRITICAL or HIGH priority Runtime deployment request cannot be immediately satisfied due to resource constraints, THE Scheduler SHALL trigger the Preemption_Engine before queuing the request; IF the Preemption_Engine succeeds and resources are freed, THE Scheduler SHALL deploy the Runtime immediately without queuing; IF the Preemption_Engine fails (no eligible candidates or stop task fails), THE Scheduler SHALL add the request to the Deployment_Queue.
5. WHEN a LOW or BEST_EFFORT priority Runtime deployment request cannot be satisfied, THE Scheduler SHALL add the request to the Deployment_Queue without triggering preemption.
6. WHEN a NORMAL priority Runtime deployment request cannot be immediately satisfied due to resource constraints, THE Scheduler SHALL add the request to the Deployment_Queue without triggering preemption and retry placement on each scheduler tick.

---

### Requirement 5: Resource Pressure Detection

**User Story:** As an SRE, I want NexusLLM to automatically detect GPU resource pressure and preserve higher-priority workloads, so that CRITICAL pipelines continue running even when the cluster is overloaded.

#### Acceptance Criteria

1. WHEN GPU utilization on any node exceeds 95% as reported by Node_Telemetry, THE Preemption_Engine SHALL classify that node as under resource pressure with trigger `gpu_utilization`.
2. WHEN a node's available VRAM — computed as `vram_mb - memory_used_mb` from the most recent `gpu_telemetry` row for that node — drops below the sum of `reserved_vram_mb` for all CRITICAL and HIGH priority Projects with active Runtimes on that node, THE Preemption_Engine SHALL classify that node as under resource pressure with trigger `vram_exhaustion`.
3. WHEN a node's available RAM — computed as `ram_total_mb - ram_used_mb` from the most recent `node_telemetry` row for that node — drops below 5% of `ram_total_mb`, THE Preemption_Engine SHALL classify that node as under resource pressure with trigger `memory_exhaustion`.
4. WHEN the Preemption_Engine selects Runtimes for eviction under resource pressure, THE Preemption_Engine SHALL select candidates starting from the lowest priority tier (BEST_EFFORT first) and SHALL exhaust all non-protected candidates in a tier before moving to the next higher tier.
5. WHILE a node is under resource pressure, IF two Runtimes have the same Project priority, THE Preemption_Engine SHALL select for eviction the Runtime with the older `last_used_at` timestamp; IF `last_used_at` is NULL, that Runtime SHALL be treated as the oldest (least recently used).
6. THE Preemption_Engine SHALL NOT preempt a Protected_Runtime under any resource pressure condition.
7. THE Preemption_Engine SHALL evaluate node pressure on a polling interval of at most 30 seconds, consistent with the existing `nodehealth.CheckInterval`.
8. WHEN the Preemption_Engine detects resource pressure on a node, THE Preemption_Engine SHALL emit a structured log entry and write a row to the `preemption_events` table recording: `node_id`, `trigger` (`gpu_utilization` | `vram_exhaustion` | `memory_exhaustion`), `pressure_value`, `evaluated_at`.

---

### Requirement 6: Runtime Preemption Rules

**User Story:** As a platform administrator, I want well-defined preemption rules enforced automatically, so that priority inversions cannot occur and lower-priority projects cannot block higher-priority ones.

#### Acceptance Criteria

1. WHEN the Preemption_Engine evaluates a candidate Runtime for preemption, IF the requesting Project's priority score is strictly greater than the candidate Runtime's Project priority score, THEN THE Preemption_Engine SHALL permit preemption; IF the requesting Project's priority score is equal to or less than the candidate Runtime's Project priority score, THEN THE Preemption_Engine SHALL deny preemption and exclude that Runtime from the candidate pool.
2. THE Preemption_Engine SHALL NOT permit any Project to preempt a Runtime belonging to a CRITICAL priority Project.
3. THE Preemption_Engine SHALL NOT permit a Project to preempt a Runtime belonging to a Project with the same or higher priority level.
4. WHEN the Preemption_Engine selects a Runtime for preemption, THE Preemption_Engine SHALL execute the following steps in order: (1) set `agent_runtimes.state = 'stopping'`, (2) disable the associated endpoint, (3) dispatch a `STOP_RUNTIME` task via the Task_Manager, (4) wait for the task to reach `success` or `failed` state with a timeout of 60 seconds, (5) upon `success`, deploy the higher-priority Runtime using the freed resources.
5. WHEN a preempted Runtime's `STOP_RUNTIME` task returns `failed`, THE Preemption_Engine SHALL log the failure, release the deployment lock, and re-queue the higher-priority deployment request in the Deployment_Queue.
6. WHEN a preempted Runtime's `STOP_RUNTIME` task does not complete within 60 seconds, THE Preemption_Engine SHALL treat the result as `failed`, log a timeout error, release the deployment lock, and re-queue the higher-priority deployment request in the Deployment_Queue.
7. WHEN a Runtime is preempted, THE Preemption_Engine SHALL record a row in the `preemption_events` table including: `id`, `node_id`, `preempted_runtime_id`, `preempted_project_id`, `preempted_priority`, `requesting_runtime_id`, `requesting_project_id`, `requesting_priority`, `trigger`, `created_at`.

---

### Requirement 7: Runtime Protection

**User Story:** As a platform administrator, I want to mark certain Runtimes as protected or always-running, so that automated systems never evict production-critical inference endpoints.

#### Acceptance Criteria

1. THE Project_Service SHALL persist protection fields on the `project_configurations` table per Project: `always_running` (boolean, default FALSE), `protected` (boolean, default FALSE), and `minimum_replicas` (integer, default 0, max 100).
2. THE Project_Service SHALL expose `PUT /admin/v1/projects/{id}/protection` which accepts `always_running`, `protected`, and `minimum_replicas` fields and persists them on the `project_configurations` row; IF `minimum_replicas` is outside the range 0–100, THE Project_Service SHALL return HTTP 400.
3. WHEN the Idle_Manager evaluates a Runtime for idle eviction, IF the associated Project's `always_running` or `protected` flag is TRUE, THEN THE Idle_Manager SHALL skip that Runtime and leave it running.
4. WHEN the Idle_Manager evaluates a Runtime for idle eviction, IF evicting the Runtime would cause the count of Runtimes in state `active`, `warm`, or `idle` for that Project to fall below `minimum_replicas`, THEN THE Idle_Manager SHALL skip that Runtime.
5. WHEN the Preemption_Engine evaluates a Runtime as a preemption candidate, IF the associated Project's `protected` or `always_running` flag is TRUE, THEN THE Preemption_Engine SHALL exclude that Runtime from the candidate pool.
6. IF `always_running = TRUE` and the count of Runtimes in state `active`, `warm`, or `idle` for the associated Project falls below `minimum_replicas`, THEN THE Idle_Manager SHALL dispatch a `WARM_RUNTIME` task if a stopped container with model files exists on an eligible node, or a `DEPLOY_RUNTIME` task otherwise; IF the task fails, THE Idle_Manager SHALL retry on the next evaluation tick.

---

### Requirement 8: Deployment Queue and Admission Control

**User Story:** As an infrastructure engineer, I want a configurable admission control policy for each Project, so that operators can choose whether to queue, preempt, or reject requests that cannot be immediately scheduled.

#### Acceptance Criteria

1. THE Scheduler SHALL maintain a `deployment_queue` table with columns: `id`, `project_id`, `runtime_config` (JSONB), `priority_score`, `admission_policy`, `status` (one of: `pending`, `deployed`, `expired`, `failed`), `enqueued_at`, `expires_at`, `attempts` (max 10).
2. WHEN a Runtime deployment request cannot be satisfied — meaning the requesting Runtime's declared VRAM, CPU, and memory requirements cannot be met by any node given current allocations and reservations — THE Scheduler SHALL evaluate the Project's `admission_policy` which takes one of: `queue` (default), `preempt_then_queue`, `reject`.
3. IF `admission_policy = 'queue'`, THEN THE Scheduler SHALL add the request to the Deployment_Queue with status `pending` and retry placement on every scheduler tick.
4. IF `admission_policy = 'preempt_then_queue'`, THEN THE Scheduler SHALL attempt preemption first; IF preemption frees resources that fully satisfy the requesting Runtime's declared requirements, THE Scheduler SHALL deploy immediately without queuing (see also Requirement 4 criterion 4); ELSE THE Scheduler SHALL enqueue the request with status `pending`.
5. IF `admission_policy = 'reject'`, THEN THE Scheduler SHALL return HTTP 409 immediately without inserting a queue entry.
6. WHEN the Scheduler processes the Deployment_Queue and a queued entry's `expires_at` timestamp has passed, THE Scheduler SHALL set the entry's status to `expired` and emit a structured log entry containing: `queue_entry_id`, `project_id`, `admission_policy`, `enqueued_at`, `expired_at`.
7. THE Scheduler SHALL process the Deployment_Queue on each scheduler tick, re-evaluating whether resources are now available, ordered by `priority_score DESC, enqueued_at ASC`; WHEN a placement attempt fails, THE Scheduler SHALL increment `attempts`; WHEN `attempts` reaches 10, THE Scheduler SHALL set the entry's status to `failed` and emit a structured log entry containing: `queue_entry_id`, `project_id`, `priority_score`, `attempts`, `failed_at`.
8. THE Project_Service SHALL expose `GET /admin/v1/projects/{id}/queue` returning entries in the Deployment_Queue with status `pending` or `expired` for the specified Project, with `limit` (max 100, default 50) and `offset` pagination parameters.

---

### Requirement 9: Gateway Integration — Project Context Propagation

**User Story:** As a platform engineer, I want every gateway usage record, audit log, and metric to include project context, so that I can attribute resource consumption to individual Projects for billing and SLA reporting.

#### Acceptance Criteria

1. WHEN the Gateway records a `usage_events` row, THE Gateway SHALL enrich it with `project_id`, `project_name`, and `project_priority` by reading the Project association from the `models` table at record time; IF the model's `project_id` is NULL, THE Gateway SHALL record `project_id = NULL`, `project_name = NULL`, and `project_priority = NULL` without error.
2. WHEN the Gateway writes an inference-related `audit_logs` entry, THE Gateway SHALL include `project_id` and `project_priority` in the `metadata` JSON field; IF the model has no associated Project, both fields SHALL be set to NULL.
3. THE Gateway SHALL maintain a Prometheus gauge metric `nexus_project_active_runtimes{project_id, project_name, priority}` tracking the count of `agent_runtimes` in state `active` or `warm` per Project; THE metric SHALL be updated whenever a Runtime transitions into or out of those states.
4. THE Usage_Tracker SHALL accept the `project_id` and `project_priority` fields on the `usage.Event` struct and persist them in the `usage_events` table; these fields SHALL be nullable to preserve backward compatibility.
5. WHEN `project_id` is not set on a Model (legacy Models created before this feature), THE Gateway SHALL record `project_id = NULL` without error and SHALL NOT reject the inference request.

---

### Requirement 10: Per-Project Analytics API

**User Story:** As a platform administrator, I want to query per-project resource consumption, GPU usage, token usage, cost, and runtime count, so that I can generate per-project SLA and cost reports.

#### Acceptance Criteria

1. THE Project_Analytics_Service SHALL expose `GET /admin/v1/projects/{id}/usage` accepting `from` and `to` query parameters in ISO 8601 format (e.g., `2026-01-01T00:00:00Z`); IF the Project does not exist, THE service SHALL return HTTP 404; IF `from` or `to` are malformed or `from` is after `to`, THE service SHALL return HTTP 400.
2. THE Project_Analytics_Service SHALL include in the response: `total_requests`, `total_tokens`, `prompt_tokens`, `completion_tokens`, `cost_usd`, `gpu_time_ms` (sum of inference durations weighted by GPU count), `avg_latency_ms` (arithmetic mean of per-request latency over all requests in the window), `error_count`.
3. THE Project_Analytics_Service SHALL include in the response: `runtime_count` (point-in-time count of Runtimes in state `active` or `warm` for the Project at the time of the query), `preemption_count` (total rows in `preemption_events` where `preempted_project_id = {id}` and `created_at` is within the `from`–`to` window).
4. THE Project_Analytics_Service SHALL query the `usage_events` table filtered by `project_id = {id}` and `created_at` within the `from`–`to` range to compute all windowed metrics.
5. WHEN no usage data exists for the requested Project within the supplied date range, THE Project_Analytics_Service SHALL return HTTP 200 with all windowed numeric fields (`total_requests`, `total_tokens`, `prompt_tokens`, `completion_tokens`, `cost_usd`, `gpu_time_ms`, `avg_latency_ms`, `error_count`, `preemption_count`) set to zero; `runtime_count` SHALL reflect the current active count regardless of the window.
6. WHERE the `breakdown=model` query parameter is provided, THE Project_Analytics_Service SHALL return an array of per-model rows, each containing `model_name` and all windowed metrics listed in criteria 2–3; IF no data exists for a model in the window, that model SHALL be omitted from the breakdown array.

---

### Requirement 11: Preemption History API

**User Story:** As a platform administrator, I want to query the history of preemption events for a Project, so that I can audit SLA violations and diagnose scheduling problems.

#### Acceptance Criteria

1. THE Project_Analytics_Service SHALL expose `GET /admin/v1/projects/{id}/preemptions` returning a paginated list of `preemption_events` rows where `preempted_project_id = {id}` OR `requesting_project_id = {id}`, ordered by `created_at` descending; IF the Project `{id}` does not exist, THE service SHALL return HTTP 404.
2. THE response SHALL include for each event: `id`, `node_id`, `preempted_runtime_id`, `preempted_project_id`, `preempted_priority`, `requesting_runtime_id`, `requesting_project_id`, `requesting_priority`, `trigger`, `created_at`.
3. THE Project_Analytics_Service SHALL support `limit` (max 100, default 50) and `offset` pagination query parameters on the preemption history endpoint; IF `limit` exceeds 100 or is less than 1, THE service SHALL return HTTP 400.
4. WHEN a preemption event references a Project that has since been deleted, THE Project_Analytics_Service SHALL return the stored `project_id` and `priority` values without error (no foreign key join required on deleted Projects).

---

### Requirement 12: Admin UI — Projects Management

**User Story:** As a platform administrator, I want a Projects page in the NexusLLM Admin UI, so that I can manage projects, view priority and reservations, and inspect preemption history without using the API directly.

#### Acceptance Criteria

1. THE Admin_UI SHALL render a "Projects" page accessible from the main navigation sidebar, listing up to 50 Projects per page with columns: name, team, priority (color-coded badge), status, active runtime count, reserved VRAM (MB).
2. THE Admin_UI SHALL render a Project Details page showing: `name`, `description`, `team`, `organization`, `status`, `created_at`, `updated_at`; the current resource reservation settings (`reserved_vram_mb`, `reserved_cpu_cores`, `reserved_memory_mb`); a table of active Runtimes (fields: `id`, `model_id`, `state`, `node_id`); a usage summary for the last 30 days (fields: `total_requests`, `total_tokens`, `cost_usd`, `avg_latency_ms`, `preemption_count`); and a preemption history table (50 rows/page with limit/offset pagination).
3. THE Admin_UI SHALL provide a Priority Management panel on the Project Details page containing a dropdown with the five priority values; WHEN the administrator submits a change via `POST /admin/v1/projects/{id}/priority`, THE Admin_UI SHALL display a success notification on HTTP 200 and an error notification with the server message on any 4xx or 5xx response.
4. THE Admin_UI SHALL provide a Reserved Resource Management panel on the Project Details page displaying current reservation values and allowing updates via `POST /admin/v1/projects/{id}/reserve`; WHEN the server returns HTTP 422 (capacity exceeded), THE Admin_UI SHALL display the server's capacity-exceeded error message to the administrator without navigating away from the page.
5. THE Admin_UI SHALL provide a Resource Allocation View showing a per-node stacked bar chart of VRAM allocated to each Project, using data aggregated from `agent_runtimes` grouped by `node_id` and `project_id`.
6. WHEN the Admin_UI displays priority levels, THE Admin_UI SHALL render CRITICAL in red, HIGH in orange, NORMAL in blue, LOW in grey, and BEST_EFFORT in light grey to provide immediate visual hierarchy.

---

### Requirement 13: Priority Change API

**User Story:** As a platform administrator, I want a dedicated API to change a Project's priority, so that the change is audited and immediately reflected by the scheduler and preemption engine.

#### Acceptance Criteria

1. THE Project_Service SHALL expose `POST /admin/v1/projects/{id}/priority` accepting a `priority` field in the request body.
2. WHEN a valid priority is provided, THE Project_Service SHALL atomically update `projects.priority` and record an `audit_logs` entry with action `project.priority_changed`, including the previous and new priority values in the metadata.
3. WHEN the priority is changed, THE Scheduler SHALL use the updated priority within one scheduling tick (no more than 60 seconds after the update is persisted) without requiring a restart.
4. IF the new priority is the same as the current priority, THE Project_Service SHALL return HTTP 200 with a response body indicating no change was made and SHALL NOT write an audit log entry.
5. IF the supplied priority value is not one of `CRITICAL`, `HIGH`, `NORMAL`, `LOW`, or `BEST_EFFORT`, THE Project_Service SHALL return HTTP 400 with an error indicating an invalid Priority_Level.
6. IF the `{id}` does not reference an existing Project, THE Project_Service SHALL return HTTP 404 with an error indicating the Project was not found.
