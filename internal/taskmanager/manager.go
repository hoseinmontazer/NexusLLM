// Package taskmanager is the control plane's task queue.
// The control plane creates tasks here; the node agent polls and executes them.
//
// Task lifecycle:
//
//	pending → claimed → running → success | failed | timeout
//
// The control plane NEVER executes tasks — it only enqueues them.
// The node agent NEVER makes placement decisions — it only executes tasks.
package taskmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
)

// ─────────────────────────────────────────────────────────────────────────────
// Task types and states
// ─────────────────────────────────────────────────────────────────────────────

type TaskType string

const (
	// TaskStartModel is the single unified startup task type used for ALL model
	// startup scenarios: initial deploy, cold start, lazy load, re-deploy, warm
	// restart, and crash recovery. DEPLOY_RUNTIME, WARM_RUNTIME, and RESTART_RUNTIME
	// are superseded — all callers must use START_MODEL exclusively.
	TaskStartModel TaskType = "START_MODEL"

	// TaskStopRuntime stops a running container (graceful drain).
	TaskStopRuntime TaskType = "STOP_RUNTIME"

	// TaskUnloadRuntime stops a container for idle eviction (same as STOP_RUNTIME
	// but carries distinct semantics for audit/monitoring).
	TaskUnloadRuntime TaskType = "UNLOAD_RUNTIME"

	// TaskDeleteRuntime removes a container permanently.
	TaskDeleteRuntime TaskType = "DELETE_RUNTIME"

	// TaskPullModel downloads model weights to the shared cache volume.
	TaskPullModel TaskType = "PULL_MODEL"

	// TaskDeleteModel removes model weights from the shared cache volume.
	TaskDeleteModel TaskType = "DELETE_MODEL"

	// TaskVerifyModel verifies checksum and file integrity of cached model weights.
	TaskVerifyModel TaskType = "VERIFY_MODEL"

	// TaskCollectInventory triggers a hardware inventory snapshot from the agent.
	TaskCollectInventory TaskType = "COLLECT_INVENTORY"

	// TaskHealthCheck requests an explicit health check report from the agent.
	TaskHealthCheck TaskType = "HEALTH_CHECK"

	// Deprecated: use TaskStartModel. Kept for backward compatibility during
	// in-flight task migration only — the executor routes both to startModel().
	TaskDeployRuntime  TaskType = "DEPLOY_RUNTIME"
	TaskWarmRuntime    TaskType = "WARM_RUNTIME"
	TaskRestartRuntime TaskType = "RESTART_RUNTIME"
)

type TaskStatus string

const (
	StatusPending   TaskStatus = "pending"
	StatusClaimed   TaskStatus = "claimed"
	StatusRunning   TaskStatus = "running"
	StatusSuccess   TaskStatus = "success"
	StatusFailed    TaskStatus = "failed"
	StatusCancelled TaskStatus = "cancelled"
	StatusTimeout   TaskStatus = "timeout"
)

// ─────────────────────────────────────────────────────────────────────────────
// Task payloads — typed structs for each task type
// ─────────────────────────────────────────────────────────────────────────────

// StartModelPayload is the single unified payload for ALL model startup
// scenarios: initial deploy, cold start, lazy load, re-deploy, warm restart,
// and crash recovery.  Every caller — admin handler, activator, idle manager,
// preemption engine, recovery watchdog — must use this struct with
// TaskStartModel.  The node agent executes the complete startup pipeline from
// this payload alone.
//
// Startup pipeline executed by the agent:
//
//	CREATED → VALIDATING → DOWNLOADING → STARTING →
//	LOADING_MODEL → WAITING_READY → READY
type StartModelPayload struct {
	// ── Identity ──────────────────────────────────────────────────────────
	RuntimeID   string `json:"runtime_id"`   // agent_runtimes.id
	EndpointID  string `json:"endpoint_id"`  // model_endpoints.id (for state sync)
	ModelID     string `json:"model_id"`     // models.id UUID
	RuntimeName string `json:"runtime_name"` // Docker container name, e.g. "nexus-gemma-2"

	// ── Container ─────────────────────────────────────────────────────────
	Backend   string `json:"backend"`             // llamacpp | vllm | ollama | tgi | cpu_native
	Image     string `json:"image"`               // full Docker image reference
	ModelName string `json:"model_name"`          // routing / served-as name
	ServedAs  string `json:"served_as,omitempty"` // alternate served model name (vLLM)

	// ── Network ───────────────────────────────────────────────────────────
	BindHost string `json:"bind_host"`
	BindPort int    `json:"bind_port"`

	// ── Resources (assigned by control plane — agent must not override) ───
	GPUDevices  []int  `json:"gpu_devices"`  // [] = CPU-only
	CPUSetCPUs  string `json:"cpuset_cpus"`  // "0-31"
	NUMANode    int    `json:"numa_node"`    // -1 = no affinity
	MemoryLimit string `json:"memory_limit"` // docker --memory, e.g. "8g"
	CPULimit    string `json:"cpu_limit"`    // --cpus value or thread count

	// ── Model source (llamacpp) ───────────────────────────────────────────
	// Resolution priority: GGUFPath > HFRepo+HFFile > HFRepo alone.
	// Legacy LlamaCpp* prefixed fields are aliases decoded by the executor.
	GGUFPath     string `json:"gguf_path,omitempty"`     // "/models/gemma-2b-Q4_K_M.gguf"
	HFRepo       string `json:"hf_repo,omitempty"`       // "bartowski/gemma-2-2b-it-GGUF"
	HFFile       string `json:"hf_file,omitempty"`       // "gemma-2-2b-it-Q4_K_M.gguf"
	HFToken      string `json:"hf_token,omitempty"`      // for gated repos
	ModelsVolume string `json:"models_volume,omitempty"` // named vol or absolute host path

	// ── llamacpp runtime flags ────────────────────────────────────────────
	CtxSize    int `json:"ctx_size"`     // --ctx-size (default: 4096)
	NGPULayers int `json:"n_gpu_layers"` // -1=all GPU, 0=CPU-only

	// ── Execution mode ────────────────────────────────────────────────────
	// ExecutionMode controls GPU vs CPU deployment.
	//   "cpu"  — never request GPUs; n_gpu_layers is forced to 0
	//   "gpu"  — always request GPUs; fail if unavailable
	//   "auto" — agent detects node GPU capability and resolves at runtime
	//            (prefers GPU, falls back to CPU)
	// Empty string is treated as "auto" for backward compatibility.
	ExecutionMode string `json:"execution_mode,omitempty"` // cpu | gpu | auto

	// ── vLLM / TGI / generic backend args ────────────────────────────────
	TensorParallel int               `json:"tensor_parallel,omitempty"`
	GPUMemoryUtil  float64           `json:"gpu_memory_util,omitempty"`
	MaxModelLen    int               `json:"max_model_len,omitempty"`
	Dtype          string            `json:"dtype,omitempty"`
	Quantization   string            `json:"quantization,omitempty"`
	ExtraArgs      []string          `json:"extra_args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`

	// ── Legacy field aliases (DEPLOY_RUNTIME backward compat) ────────────
	// The executor reads these as fallbacks when the canonical fields above
	// are empty. New callers must use the canonical fields.
	LlamaCppModelPath    string `json:"llamacpp_model_path,omitempty"`
	LlamaCppHFRepo       string `json:"llamacpp_hf_repo,omitempty"`
	LlamaCppHFFile       string `json:"llamacpp_hf_file,omitempty"`
	LlamaCppCtxSize      int    `json:"llamacpp_ctx_size,omitempty"`
	LlamaCppNGPULayers   int    `json:"llamacpp_n_gpu_layers,omitempty"`
	LlamaCppModelsVolume string `json:"llamacpp_models_volume,omitempty"`
}

// DeployRuntimePayload is an alias kept for backward compatibility with any
// in-flight DEPLOY_RUNTIME tasks already in the queue.  New code must use
// StartModelPayload with TaskStartModel exclusively.
//
// Deprecated: use StartModelPayload.
type DeployRuntimePayload = StartModelPayload

// StopRuntimePayload is the payload for STOP_RUNTIME.
type StopRuntimePayload struct {
	RuntimeID   string `json:"runtime_id"`
	ContainerID string `json:"container_id"`
	DrainSecs   int    `json:"drain_secs"` // graceful drain window
}

// WarmRuntimePayload is deprecated. Use StartModelPayload with TaskStartModel.
// Kept for backward compatibility with in-flight tasks only.
//
// Deprecated: use StartModelPayload.
type WarmRuntimePayload struct {
	RuntimeID   string `json:"runtime_id"`
	ContainerID string `json:"container_id"`
}

// RestartRuntimePayload is deprecated. Use StartModelPayload with TaskStartModel.
// Kept for backward compatibility with in-flight tasks only.
//
// Deprecated: use StartModelPayload.
type RestartRuntimePayload struct {
	RuntimeID   string `json:"runtime_id"`
	ContainerID string `json:"container_id"`
}

// DeleteRuntimePayload for DELETE_RUNTIME.
type DeleteRuntimePayload struct {
	RuntimeID   string `json:"runtime_id"`
	ContainerID string `json:"container_id"`
	RemoveModel bool   `json:"remove_model"` // also evict model weights
}

// PullModelPayload for PULL_MODEL.
type PullModelPayload struct {
	ModelID   string `json:"model_id"`
	HFRepo    string `json:"hf_repo"` // e.g. "Qwen/Qwen3-32B-Instruct"
	HFToken   string `json:"hf_token,omitempty"`
	LocalPath string `json:"local_path,omitempty"`
	Backend   string `json:"backend"` // which backend manages this model's cache
}

// HealthCheckPayload for HEALTH_CHECK.
type HealthCheckPayload struct {
	RuntimeIDs []string `json:"runtime_ids"` // empty = check all
}

// ─────────────────────────────────────────────────────────────────────────────
// Task record
// ─────────────────────────────────────────────────────────────────────────────

// Task is a unit of work the control plane dispatches to a node agent.
type Task struct {
	ID             string          `db:"id"              json:"id"`
	NodeID         string          `db:"node_id"         json:"node_id"`
	TaskType       TaskType        `db:"task_type"       json:"task_type"`
	Payload        []byte          `db:"payload"         json:"-"`
	PayloadRaw     json.RawMessage `json:"payload"` // raw JSON — not a string
	Status         TaskStatus      `db:"status"          json:"status"`
	Priority       int             `db:"priority"        json:"priority"`
	CreatedBy      string          `db:"created_by"      json:"created_by"`
	CreatedAt      time.Time       `db:"created_at"      json:"created_at"`
	ClaimedAt      *time.Time      `db:"claimed_at"      json:"claimed_at,omitempty"`
	StartedAt      *time.Time      `db:"started_at"      json:"started_at,omitempty"`
	CompletedAt    *time.Time      `db:"completed_at"    json:"completed_at,omitempty"`
	TimeoutAt      *time.Time      `db:"timeout_at"      json:"timeout_at,omitempty"`
	Result         []byte          `db:"result"          json:"-"`
	ResultJSON     string          `json:"result,omitempty"`
	ErrorMsg       string          `db:"error_msg"       json:"error_msg,omitempty"`
	RuntimeID      string          `db:"runtime_id"      json:"runtime_id,omitempty"`
	IdempotencyKey string          `db:"idempotency_key" json:"idempotency_key,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Manager
// ─────────────────────────────────────────────────────────────────────────────

// Manager is the control plane's task queue interface.
type Manager struct {
	db  *sqlx.DB
	log *zap.Logger
}

// NewManager constructs a task Manager.
func NewManager(db *sqlx.DB, log *zap.Logger) *Manager {
	return &Manager{db: db, log: log}
}

// ─────────────────────────────────────────────────────────────────────────────
// Schema validation — fail fast on startup
// ─────────────────────────────────────────────────────────────────────────────

// requiredTaskTypes lists every task type the control plane may enqueue.
// If any of these are missing from the live DB constraint, startup must abort.
var requiredTaskTypes = []string{
	"START_MODEL",
	"DEPLOY_RUNTIME",
	"STOP_RUNTIME",
	"RESTART_RUNTIME",
	"DELETE_RUNTIME",
	"WARM_RUNTIME",
	"UNLOAD_RUNTIME",
	"PULL_MODEL",
	"DELETE_MODEL",
	"VERIFY_MODEL",
	"COLLECT_INVENTORY",
	"HEALTH_CHECK",
}

// SchemaValidationError is returned by ValidateSchema when the DB is incompatible.
type SchemaValidationError struct {
	MissingTypes  []string // task types absent from the live constraint
	ConstraintDef string   // raw constraint definition from pg_get_constraintdef
	ConnectedDB   string   // current_database() at the time of the check
	SearchPath    string   // current search_path
}

func (e *SchemaValidationError) Error() string {
	return fmt.Sprintf(
		"database schema incompatible: task types %v missing from agent_tasks_task_type_check\n"+
			"  connected_database : %s\n"+
			"  search_path        : %s\n"+
			"  constraint_def     : %s\n"+
			"  fix               : apply migration 013_start_model_task_type.sql against this database",
		e.MissingTypes, e.ConnectedDB, e.SearchPath, e.ConstraintDef,
	)
}

// ValidateSchema probes the live database to confirm that the
// agent_tasks_task_type_check constraint contains every required task type.
//
// It queries pg_constraint directly — not migration history files — so it
// correctly detects the case where migrations ran against a different database
// instance, partial migration failures, or constraint recreation bugs.
//
// Call this at startup before the service begins accepting traffic.
// Returns *SchemaValidationError with full diagnostic context on failure.
func (m *Manager) ValidateSchema(ctx context.Context) error {
	// Gather diagnostic context first so the error message is always complete.
	var connectedDB, searchPath string
	_ = m.db.QueryRowContext(ctx, `SELECT current_database(), current_setting('search_path')`).
		Scan(&connectedDB, &searchPath)

	// Fetch the raw constraint definition from the catalog.
	// pg_get_constraintdef returns the full CHECK(...) expression as text.
	var constraintDef string
	err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(pg_get_constraintdef(c.oid), '')
		FROM pg_constraint c
		JOIN pg_class     t ON t.oid = c.conrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE t.relname  = 'agent_tasks'
		  AND c.conname  = 'agent_tasks_task_type_check'
		  AND n.nspname  = current_schema()`).Scan(&constraintDef)
	if err != nil || constraintDef == "" {
		return &SchemaValidationError{
			MissingTypes:  requiredTaskTypes,
			ConstraintDef: "(constraint not found — migration 007 may not have run)",
			ConnectedDB:   connectedDB,
			SearchPath:    searchPath,
		}
	}

	// Check each required type against the constraint text.
	var missing []string
	for _, t := range requiredTaskTypes {
		// The constraint def looks like: CHECK (task_type = ANY (ARRAY['START_MODEL'::text, ...]))
		// or:                            CHECK ((task_type)::text = ANY (ARRAY[...]))
		// A simple substring match on the quoted name is reliable for both forms.
		if !containsTaskType(constraintDef, t) {
			missing = append(missing, t)
		}
	}

	if len(missing) > 0 {
		return &SchemaValidationError{
			MissingTypes:  missing,
			ConstraintDef: constraintDef,
			ConnectedDB:   connectedDB,
			SearchPath:    searchPath,
		}
	}

	m.log.Info("schema validation passed",
		zap.String("database", connectedDB),
		zap.Int("required_task_types", len(requiredTaskTypes)),
	)
	return nil
}

// containsTaskType checks whether the raw constraint definition string
// contains the given task type as a quoted literal (e.g. 'START_MODEL').
func containsTaskType(constraintDef, taskType string) bool {
	// Match both single-quoted form ('START_MODEL') used in CHECK IN(...)
	// and double-colon cast form ('START_MODEL'::text) used in ANY(ARRAY[...]).
	needle := "'" + taskType + "'"
	return strings.Contains(constraintDef, needle)
}

// Enqueue creates a new task for a node. Returns the task ID.
// payload must be a JSON-serialisable struct.
func (m *Manager) Enqueue(ctx context.Context, nodeID string, taskType TaskType, payload interface{},
	opts ...EnqueueOption) (string, error) {

	o := enqueueOptions{
		priority:  50,
		createdBy: "control-plane",
		timeout:   10 * time.Minute,
	}
	for _, opt := range opts {
		opt(&o)
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}

	taskID := uuid.New().String()
	var ikey interface{}
	if o.idempotencyKey != "" {
		ikey = o.idempotencyKey
	}

	var timeoutAt interface{}
	if o.timeout > 0 {
		t := time.Now().Add(o.timeout)
		timeoutAt = t
	}

	_, err = m.db.ExecContext(ctx, `
		INSERT INTO agent_tasks
		  (id, node_id, task_type, payload, status, priority,
		   created_by, timeout_at, runtime_id, idempotency_key)
		VALUES ($1,$2,$3,$4,'pending',$5,$6,$7,$8,$9)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		taskID, nodeID, string(taskType), payloadBytes,
		o.priority, o.createdBy, timeoutAt,
		nilableStr(o.runtimeID), ikey,
	)
	if err != nil {
		return "", fmt.Errorf("enqueue task: %w", err)
	}

	m.log.Info("task enqueued",
		zap.String("task_id", taskID),
		zap.String("node_id", nodeID),
		zap.String("task_type", string(taskType)),
		zap.Int("priority", o.priority),
	)
	return taskID, nil
}

// PendingForNode returns up to `limit` pending tasks for a node, ordered by
// priority DESC then created_at ASC. Used by the agent's poll endpoint.
func (m *Manager) PendingForNode(ctx context.Context, nodeID string, limit int) ([]Task, error) {
	var tasks []Task
	err := m.db.SelectContext(ctx, &tasks, `
		SELECT id, node_id, task_type, payload, status, priority,
		       created_by, created_at, claimed_at, started_at, completed_at,
		       timeout_at, result, COALESCE(error_msg,'') AS error_msg,
		       COALESCE(runtime_id::text,'') AS runtime_id,
		       COALESCE(idempotency_key,'') AS idempotency_key
		FROM agent_tasks
		WHERE node_id = $1
		  AND status = 'pending'
		  AND (timeout_at IS NULL OR timeout_at > NOW())
		ORDER BY priority DESC, created_at ASC
		LIMIT $2`,
		nodeID, limit,
	)
	// Copy payload bytes to PayloadRaw (json.RawMessage) for direct embedding
	// in the JSON response — agent receives a proper JSON object, not a string.
	for i := range tasks {
		if tasks[i].Payload != nil {
			tasks[i].PayloadRaw = json.RawMessage(tasks[i].Payload)
		}
	}
	return tasks, err
}

// ClaimTask atomically marks a task as claimed by the agent.
// Returns false if the task was already claimed or not found.
func (m *Manager) ClaimTask(ctx context.Context, taskID, nodeID string) (bool, error) {
	res, err := m.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status = 'claimed', claimed_at = NOW()
		WHERE id = $1 AND node_id = $2 AND status = 'pending'`,
		taskID, nodeID,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkRunning transitions a task from claimed → running.
func (m *Manager) MarkRunning(ctx context.Context, taskID string) error {
	_, err := m.db.ExecContext(ctx, `
		UPDATE agent_tasks SET status='running', started_at=NOW()
		WHERE id=$1 AND status='claimed'`, taskID)
	return err
}

// Complete marks a task as success with a result payload.
func (m *Manager) Complete(ctx context.Context, taskID string, result interface{}) error {
	b, _ := json.Marshal(result)
	_, err := m.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status='success', completed_at=NOW(), result=$1
		WHERE id=$2`, b, taskID)
	return err
}

// Fail marks a task as failed with an error message.
func (m *Manager) Fail(ctx context.Context, taskID, errMsg string) error {
	_, err := m.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status='failed', completed_at=NOW(), error_msg=$1
		WHERE id=$2`, errMsg, taskID)
	return err
}

// Cancel cancels a pending task.
func (m *Manager) Cancel(ctx context.Context, taskID string) error {
	_, err := m.db.ExecContext(ctx, `
		UPDATE agent_tasks SET status='cancelled', completed_at=NOW()
		WHERE id=$1 AND status IN ('pending','claimed')`, taskID)
	return err
}

// GetTask returns a task by ID.
func (m *Manager) GetTask(ctx context.Context, taskID string) (*Task, error) {
	var t Task
	err := m.db.GetContext(ctx, &t, `
		SELECT id, node_id, task_type, payload, status, priority,
		       created_by, created_at, claimed_at, started_at, completed_at,
		       timeout_at, result, COALESCE(error_msg,'') AS error_msg,
		       COALESCE(runtime_id::text,'') AS runtime_id,
		       COALESCE(idempotency_key,'') AS idempotency_key
		FROM agent_tasks WHERE id=$1`, taskID)
	if err != nil {
		return nil, err
	}
	if t.Payload != nil {
		t.PayloadRaw = json.RawMessage(t.Payload)
	}
	if t.Result != nil {
		t.ResultJSON = string(t.Result)
	}
	return &t, nil
}

// ListForNode returns recent tasks for a node (for monitoring/UI).
func (m *Manager) ListForNode(ctx context.Context, nodeID string, limit int) ([]Task, error) {
	var tasks []Task
	err := m.db.SelectContext(ctx, &tasks, `
		SELECT id, node_id, task_type, status, priority, created_by,
		       created_at, claimed_at, started_at, completed_at,
		       COALESCE(error_msg,'') AS error_msg,
		       COALESCE(runtime_id::text,'') AS runtime_id
		FROM agent_tasks
		WHERE node_id=$1
		ORDER BY created_at DESC LIMIT $2`, nodeID, limit)
	return tasks, err
}

// TimeoutStale marks timed-out tasks so they don't block the queue.
// Call this periodically from a background goroutine.
func (m *Manager) TimeoutStale(ctx context.Context) (int64, error) {
	res, err := m.db.ExecContext(ctx, `
		UPDATE agent_tasks
		SET status='timeout', completed_at=NOW()
		WHERE status IN ('pending','claimed','running')
		  AND timeout_at IS NOT NULL
		  AND timeout_at < NOW()`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Enqueue options
// ─────────────────────────────────────────────────────────────────────────────

type enqueueOptions struct {
	priority       int
	timeout        time.Duration
	createdBy      string
	runtimeID      string
	idempotencyKey string
}

// EnqueueOption is a functional option for Enqueue.
type EnqueueOption func(*enqueueOptions)

// WithPriority sets the task priority (0–100, higher = processed first).
func WithPriority(p int) EnqueueOption {
	return func(o *enqueueOptions) { o.priority = p }
}

// WithTimeout sets how long until the task expires if unclaimed.
func WithTimeout(d time.Duration) EnqueueOption {
	return func(o *enqueueOptions) { o.timeout = d }
}

// WithActor sets the created_by field.
func WithActor(actor string) EnqueueOption {
	return func(o *enqueueOptions) { o.createdBy = actor }
}

// WithRuntimeID links the task to an agent_runtime row.
func WithRuntimeID(id string) EnqueueOption {
	return func(o *enqueueOptions) { o.runtimeID = id }
}

// WithIdempotencyKey prevents duplicate tasks on retry.
func WithIdempotencyKey(key string) EnqueueOption {
	return func(o *enqueueOptions) { o.idempotencyKey = key }
}

func nilableStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
