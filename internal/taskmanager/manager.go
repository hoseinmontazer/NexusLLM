// Package taskmanager is the control plane's task queue.
// The control plane creates tasks here; the node agent polls and executes them.
//
// Task lifecycle:
//   pending → claimed → running → success | failed | timeout
//
// The control plane NEVER executes tasks — it only enqueues them.
// The node agent NEVER makes placement decisions — it only executes tasks.
package taskmanager

import (
	"context"
	"encoding/json"
	"fmt"
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
	TaskDeployRuntime   TaskType = "DEPLOY_RUNTIME"
	TaskStopRuntime     TaskType = "STOP_RUNTIME"
	TaskRestartRuntime  TaskType = "RESTART_RUNTIME"
	TaskDeleteRuntime   TaskType = "DELETE_RUNTIME"
	TaskWarmRuntime     TaskType = "WARM_RUNTIME"
	TaskUnloadRuntime   TaskType = "UNLOAD_RUNTIME"
	TaskPullModel       TaskType = "PULL_MODEL"
	TaskDeleteModel     TaskType = "DELETE_MODEL"
	TaskVerifyModel     TaskType = "VERIFY_MODEL"
	TaskCollectInventory TaskType = "COLLECT_INVENTORY"
	TaskHealthCheck     TaskType = "HEALTH_CHECK"
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

// DeployRuntimePayload is the payload for DEPLOY_RUNTIME.
// All placement decisions have already been made by the control plane.
// The agent just executes.
type DeployRuntimePayload struct {
	// Identity
	RuntimeID   string `json:"runtime_id"`   // agent_runtimes.id
	EndpointID  string `json:"endpoint_id"`
	ModelID     string `json:"model_id"`
	RuntimeName string `json:"runtime_name"` // Docker container name

	// What to run
	Backend     string   `json:"backend"`      // vllm | ollama | tgi | cpu_native | llamacpp
	Image       string   `json:"image"`
	ModelName   string   `json:"model_name"`   // HF model ID
	ServedAs    string   `json:"served_as"`    // short routing name

	// Network
	BindHost string `json:"bind_host"`
	BindPort int    `json:"bind_port"`

	// Resources — assigned by control plane, NOT the agent
	GPUDevices     []int  `json:"gpu_devices"`      // [0] or [0,1]
	CPUSetCPUs     string `json:"cpuset_cpus"`      // "0-31"
	NUMANode       int    `json:"numa_node"`        // -1 = no affinity
	MemoryLimit    string `json:"memory_limit"`     // "64g"
	CPULimit       string `json:"cpu_limit"`        // "4"

	// Backend-specific
	TensorParallel int     `json:"tensor_parallel"`
	GPUMemoryUtil  float64 `json:"gpu_memory_util"`
	MaxModelLen    int     `json:"max_model_len"`
	Dtype          string  `json:"dtype"`
	Quantization   string  `json:"quantization"`
	ExtraArgs      []string `json:"extra_args"`
	Env            map[string]string `json:"env"`
	HFToken        string  `json:"hf_token,omitempty"`

	// ── llamacpp-specific ──────────────────────────────────────────────────
	// LlamaCppModelPath: local GGUF path inside the container.
	// Pre-populate the volume with PULL_MODEL before deploying.
	LlamaCppModelPath string `json:"llamacpp_model_path,omitempty"`
	// LlamaCppHFRepo + LlamaCppHFFile: download GGUF from HF at container startup.
	LlamaCppHFRepo string `json:"llamacpp_hf_repo,omitempty"`
	LlamaCppHFFile string `json:"llamacpp_hf_file,omitempty"`
	// LlamaCppCtxSize overrides the default context window (default: 4096).
	LlamaCppCtxSize int `json:"llamacpp_ctx_size,omitempty"`
	// LlamaCppNGPULayers: 0 = CPU-only, -1 = all layers on GPU (default when GPUs assigned).
	LlamaCppNGPULayers int `json:"llamacpp_n_gpu_layers,omitempty"`
	// LlamaCppModelsVolume: host path or Docker volume name mounted as /models.
	LlamaCppModelsVolume string `json:"llamacpp_models_volume,omitempty"`
}

// StopRuntimePayload is the payload for STOP_RUNTIME.
type StopRuntimePayload struct {
	RuntimeID   string `json:"runtime_id"`
	ContainerID string `json:"container_id"`
	DrainSecs   int    `json:"drain_secs"` // graceful drain window
}

// WarmRuntimePayload is the payload for WARM_RUNTIME.
// The container already exists (was stopped); the agent runs `docker start`.
type WarmRuntimePayload struct {
	RuntimeID   string `json:"runtime_id"`
	ContainerID string `json:"container_id"`
}

// RestartRuntimePayload for RESTART_RUNTIME.
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
	HFRepo    string `json:"hf_repo"`    // e.g. "Qwen/Qwen3-32B-Instruct"
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
	ID             string     `db:"id"              json:"id"`
	NodeID         string     `db:"node_id"         json:"node_id"`
	TaskType       TaskType   `db:"task_type"       json:"task_type"`
	Payload        []byte     `db:"payload"         json:"-"`
	PayloadRaw     json.RawMessage `json:"payload"`    // raw JSON — not a string
	Status         TaskStatus `db:"status"          json:"status"`
	Priority       int        `db:"priority"        json:"priority"`
	CreatedBy      string     `db:"created_by"      json:"created_by"`
	CreatedAt      time.Time  `db:"created_at"      json:"created_at"`
	ClaimedAt      *time.Time `db:"claimed_at"      json:"claimed_at,omitempty"`
	StartedAt      *time.Time `db:"started_at"      json:"started_at,omitempty"`
	CompletedAt    *time.Time `db:"completed_at"    json:"completed_at,omitempty"`
	TimeoutAt      *time.Time `db:"timeout_at"      json:"timeout_at,omitempty"`
	Result         []byte     `db:"result"          json:"-"`
	ResultJSON     string     `json:"result,omitempty"`
	ErrorMsg       string     `db:"error_msg"       json:"error_msg,omitempty"`
	RuntimeID      string     `db:"runtime_id"      json:"runtime_id,omitempty"`
	IdempotencyKey string     `db:"idempotency_key" json:"idempotency_key,omitempty"`
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
