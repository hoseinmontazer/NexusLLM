package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/modelguard"
	"github.com/nexusllm/nexusllm/internal/nodeaddr"
	"github.com/nexusllm/nexusllm/internal/replicaguard"
	"github.com/nexusllm/nexusllm/internal/taskmanager"
	"go.uber.org/zap"
)

// ControllerHandler exposes model runtime lifecycle operations via the Admin API.
// All operations are dispatched as tasks to the node agent via the task manager —
// the admin server never runs Docker commands directly.
type ControllerHandler struct {
	db      *sqlx.DB
	taskMgr *taskmanager.Manager
	log     *zap.Logger
}

// NewControllerHandler constructs a ControllerHandler.
func NewControllerHandler(db *sqlx.DB, taskMgr *taskmanager.Manager, log *zap.Logger) *ControllerHandler {
	if log == nil {
		log, _ = zap.NewProduction()
	}
	return &ControllerHandler{db: db, taskMgr: taskMgr, log: log}
}

// runtimeRow holds the fields needed to build start/stop task payloads.
type runtimeRow struct {
	RuntimeID      string  `db:"runtime_id"`
	NodeID         string  `db:"node_id"`
	ContainerID    string  `db:"container_id"`
	RuntimeName    string  `db:"runtime_name"`
	Backend        string  `db:"backend"`
	Image          string  `db:"image"`
	ModelName      string  `db:"model_name"`
	BindHost       string  `db:"bind_host"`
	BindPort       int     `db:"bind_port"`
	GPUIDsJSON     string  `db:"gpu_ids_json"`
	GPUDevicesJSON string  `db:"gpu_devices_json"`
	TensorParallel int     `db:"tensor_parallel"`
	GPUMemoryUtil  float64 `db:"gpu_memory_util"`
	MaxModelLen    int     `db:"max_model_len"`
	Dtype          string  `db:"dtype"`
	Quantization   string  `db:"quantization"`
	ExecutionMode  string  `db:"execution_mode"`
	GGUFPath       string  `db:"gguf_path"`
	HFRepo         string  `db:"hf_repo"`
	HFFile         string  `db:"hf_file"`
	CtxSize        int     `db:"ctx_size"`
	NGPULayers     int     `db:"n_gpu_layers"`
	MemoryLimit    string  `db:"memory_limit"`
	ModelsVolume   string  `db:"models_volume"`
	ExtraArgsJSON  string  `db:"extra_args_json"`
	EnvJSON        string  `db:"env_json"`
	EndpointID     string  `db:"endpoint_id"`
	ModelID        string  `db:"model_id"`
	ModelEnabled   bool    `db:"model_enabled"`
	ModelLifecycle string  `db:"model_lifecycle"`
}

// loadRuntime fetches the best runtime row for an endpoint.
// It joins model_endpoints for image and node_id, and model_runtime_configs
// for backend parameters. Includes failed/stopped runtimes so restart works
// after a crash or manual docker rm.
func (h *ControllerHandler) loadRuntime(c *gin.Context, endpointID string) (*runtimeRow, bool) {
	var row runtimeRow
	err := h.db.GetContext(c.Request.Context(), &row, `
		SELECT
			COALESCE(ar.id::text, '')                             AS runtime_id,
			COALESCE(me.node_id::text, ar.node_id::text, '')      AS node_id,
			COALESCE(ar.container_id, '')                         AS container_id,
			COALESCE(ar.runtime_name, m.name)                     AS runtime_name,
			COALESCE(ar.backend, m.backend_type, 'vllm')          AS backend,
			COALESCE(me.runtime_image, 'vllm/vllm-openai:latest') AS image,
			m.name                                                 AS model_name,
			COALESCE(NULLIF(ar.bind_host, ''), me.host, '')       AS bind_host,
			COALESCE(NULLIF(ar.bind_port, 0), me.port, 0)         AS bind_port,
			COALESCE(ar.gpu_ids::text, '[]')                      AS gpu_ids_json,
			COALESCE(mrc.gpu_devices::text, ar.gpu_ids::text, '[]') AS gpu_devices_json,
			COALESCE(mrc.tensor_parallel,  1)                     AS tensor_parallel,
			COALESCE(mrc.gpu_memory_util,  0.9)                   AS gpu_memory_util,
			COALESCE(mrc.max_model_len,    0)                     AS max_model_len,
			COALESCE(mrc.dtype,            'auto')                AS dtype,
			COALESCE(mrc.quantization,     '')                    AS quantization,
			COALESCE(mrc.execution_mode,   'auto')                AS execution_mode,
			COALESCE(mrc.gguf_path,        '')                    AS gguf_path,
			COALESCE(mrc.hf_repo,          '')                    AS hf_repo,
			COALESCE(mrc.hf_file,          '')                    AS hf_file,
			COALESCE(mrc.ctx_size,         4096)                  AS ctx_size,
			COALESCE(mrc.n_gpu_layers,     0)                     AS n_gpu_layers,
			COALESCE(mrc.memory_limit,     '')                    AS memory_limit,
			COALESCE(mrc.models_volume,    '')                    AS models_volume,
			COALESCE(mrc.extra_args::text, '[]')                  AS extra_args_json,
			COALESCE(mrc.env::text,        '{}')                  AS env_json,
			me.id::text                                           AS endpoint_id,
			m.id::text                                            AS model_id,
			m.enabled                                             AS model_enabled,
			COALESCE(m.lifecycle,'active')                        AS model_lifecycle
		FROM model_endpoints me
		JOIN models m ON m.id = me.model_id
		LEFT JOIN model_runtime_configs mrc ON mrc.model_id = me.model_id
		LEFT JOIN agent_runtimes ar
		       ON ar.endpoint_id = me.id
		      AND ar.state NOT IN ('deleted')
		WHERE me.id = $1
		ORDER BY
			(ar.state IN ('ready','active','warm','idle')) DESC,
			(ar.state IN ('loading','starting','pending'))  DESC,
			ar.created_at DESC NULLS LAST
		LIMIT 1`, endpointID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "endpoint not found: " + err.Error()})
		return nil, false
	}
	// A model can have enabled=TRUE while lifecycle='deleted' (EnableModel
	// does not clear a stale 'deleted' lifecycle when re-enabling), so
	// enabled alone is not a sufficient eligibility check — see
	// internal/modelguard (forensic audit, Case File 003, round 6). Without
	// this, Start/Restart/Upgrade/Rollback could recreate a runtime for a
	// model an admin had soft-deleted.
	if !modelguard.Eligible(row.ModelEnabled, row.ModelLifecycle) {
		c.JSON(http.StatusConflict, gin.H{"error": "model has been deleted or disabled — redeploy it instead of starting/restarting"})
		return nil, false
	}
	if row.NodeID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "endpoint has no assigned node — deploy to a node first"})
		return nil, false
	}
	// The query above prefers an EXISTING ar.bind_host/me.host, which can
	// perpetuate a stale or wrong value indefinitely across restart/upgrade/
	// rollback (forensic audit, Case File 003). Since the node is known,
	// always resolve its current canonical reachable address instead of
	// trusting whatever was stored before — this is the single choke point
	// every caller (Start/Restart/Upgrade/Rollback) goes through, so fixing
	// it here fixes it for all four.
	row.BindHost = nodeaddr.CanonicalHost(c.Request.Context(), h.db, row.NodeID)
	return &row, true
}

// buildStartPayload builds a StartModelPayload from a runtimeRow.
func buildStartPayload(runtimeID string, row *runtimeRow) taskmanager.StartModelPayload {
	var env map[string]string
	if row.EnvJSON != "" && row.EnvJSON != "{}" {
		_ = json.Unmarshal([]byte(row.EnvJSON), &env)
	}
	if env == nil {
		env = map[string]string{}
	}

	var extraArgs []string
	if row.ExtraArgsJSON != "" && row.ExtraArgsJSON != "[]" {
		_ = json.Unmarshal([]byte(row.ExtraArgsJSON), &extraArgs)
	}

	var gpuDevices []int
	if row.GPUDevicesJSON != "" && row.GPUDevicesJSON != "[]" {
		_ = json.Unmarshal([]byte(row.GPUDevicesJSON), &gpuDevices)
	}

	return taskmanager.StartModelPayload{
		RuntimeID:      runtimeID,
		EndpointID:     row.EndpointID,
		ModelID:        row.ModelID,
		RuntimeName:    row.RuntimeName,
		Backend:        row.Backend,
		Image:          row.Image,
		ModelName:      row.ModelName,
		BindHost:       row.BindHost,
		BindPort:       row.BindPort,
		GPUDevices:     gpuDevices,
		TensorParallel: row.TensorParallel,
		GPUMemoryUtil:  row.GPUMemoryUtil,
		MaxModelLen:    row.MaxModelLen,
		Dtype:          row.Dtype,
		Quantization:   row.Quantization,
		ExecutionMode:  row.ExecutionMode,
		GGUFPath:       row.GGUFPath,
		HFRepo:         row.HFRepo,
		HFFile:         row.HFFile,
		CtxSize:        row.CtxSize,
		NGPULayers:     row.NGPULayers,
		MemoryLimit:    row.MemoryLimit,
		ModelsVolume:   row.ModelsVolume,
		ExtraArgs:      extraArgs,
		Env:            env,
	}
}

// ensureRuntimeRow creates a fresh agent_runtimes row for the given runtimeID.
//
// This is the only one of the runtime-creation paths (DeployModel, the
// cold-start activator, the HA reconciler, the stuck-runtime sweeper) that
// did not call replicaguard.ClaimSlot before this fix (forensic audit, Case
// File 003, round 6) — meaning concurrent Start/Restart/Upgrade/Rollback
// calls for the same model could each independently insert a row with no
// shared capacity check. ClaimSlot's advisory lock now guards the INSERT the
// same way it guards every other creation path: claim the slot and insert
// inside one transaction, so the lock covers both the decision and the
// write.
func (h *ControllerHandler) ensureRuntimeRow(c *gin.Context, runtimeID string, row *runtimeRow) bool {
	ctx := c.Request.Context()

	desired, desErr := replicaguard.DesiredReplicas(ctx, h.db, row.ModelID)
	if desErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load desired_replicas: " + desErr.Error()})
		return false
	}
	var maxSurge int
	_ = h.db.QueryRowContext(ctx,
		`SELECT COALESCE(max_surge,1) FROM model_replica_specs WHERE model_id=$1`, row.ModelID,
	).Scan(&maxSurge)
	if maxSurge < 1 {
		maxSurge = 1
	}

	tx, txErr := h.db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if txErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to begin claim transaction: " + txErr.Error()})
		return false
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	slotOK, slotErr := replicaguard.ClaimSlot(ctx, tx, row.ModelID, desired, maxSurge)
	if slotErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "claim_replica_slot error: " + slotErr.Error()})
		return false
	}
	if !slotOK {
		c.JSON(http.StatusConflict, gin.H{"error": "model already at desired_replicas capacity — a concurrent Start/Restart/Upgrade/Rollback claimed the slot first"})
		return false
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO agent_runtimes
		  (id, node_id, endpoint_id, model_id, runtime_name, backend,
		   state, gpu_ids, bind_host, bind_port, cpu_affinity, numa_node)
		VALUES ($1,$2,$3,$4,$5,$6,'pending',$7::jsonb,$8,$9,'',-1)
		ON CONFLICT DO NOTHING`,
		runtimeID, row.NodeID, row.EndpointID, row.ModelID,
		row.RuntimeName, row.Backend,
		row.GPUIDsJSON, row.BindHost, row.BindPort,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create runtime record: " + err.Error()})
		return false
	}
	if commitErr := tx.Commit(); commitErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to commit claimed runtime row: " + commitErr.Error()})
		return false
	}
	committed = true
	return true
}

// ─── StartModel ───────────────────────────────────────────────────────────────

// StartModel handles POST /admin/v1/models/:id/start
func (h *ControllerHandler) StartModel(c *gin.Context) {
	endpointID := c.Query("endpoint_id")
	if endpointID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "endpoint_id query param required"})
		return
	}

	row, ok := h.loadRuntime(c, endpointID)
	if !ok {
		return
	}

	newRuntimeID := uuid.New().String()
	if !h.ensureRuntimeRow(c, newRuntimeID, row) {
		return
	}

	// Mark any old failed/stuck runtime as deleted so it doesn't confuse the agent.
	if row.RuntimeID != "" {
		_, _ = h.db.ExecContext(c.Request.Context(), `
			UPDATE agent_runtimes
			SET state = 'deleted', updated_at = NOW()
			WHERE endpoint_id = $1
			  AND id != $2
			  AND state IN ('failed', 'stopped', 'unloaded')`,
			endpointID, newRuntimeID)
	}

	payload := buildStartPayload(newRuntimeID, row)

	_, _ = h.db.ExecContext(c.Request.Context(),
		`UPDATE model_endpoints SET lifecycle_state='loading', updated_at=NOW() WHERE id=$1`, endpointID)

	taskID, err := h.taskMgr.Enqueue(
		c.Request.Context(), row.NodeID,
		taskmanager.TaskStartModel, payload,
		taskmanager.WithPriority(85),
		taskmanager.WithActor("admin-start"),
		taskmanager.WithRuntimeID(newRuntimeID),
		taskmanager.WithTimeout(20*time.Minute),
		taskmanager.WithIdempotencyKey("start:"+row.NodeID+":"+newRuntimeID),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "dispatch START_MODEL task: " + err.Error()})
		return
	}

	h.log.Info("START_MODEL dispatched via admin",
		zap.String("endpoint_id", endpointID),
		zap.String("node_id", row.NodeID),
		zap.String("runtime_id", newRuntimeID),
		zap.String("task_id", taskID),
	)
	c.JSON(http.StatusAccepted, gin.H{
		"message":     "start task dispatched",
		"endpoint_id": endpointID,
		"runtime_id":  newRuntimeID,
		"task_id":     taskID,
	})
}

// ─── StopModel ────────────────────────────────────────────────────────────────

// StopModel handles POST /admin/v1/models/:id/stop
func (h *ControllerHandler) StopModel(c *gin.Context) {
	endpointID := c.Query("endpoint_id")
	if endpointID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "endpoint_id query param required"})
		return
	}

	row, ok := h.loadRuntime(c, endpointID)
	if !ok {
		return
	}

	if row.RuntimeID == "" {
		c.JSON(http.StatusOK, gin.H{"message": "no active runtime found — nothing to stop", "endpoint_id": endpointID})
		return
	}

	// Mark draining immediately so the gateway stops routing.
	_, _ = h.db.ExecContext(c.Request.Context(),
		`UPDATE model_endpoints SET lifecycle_state='draining', updated_at=NOW() WHERE id=$1`, endpointID)
	_, _ = h.db.ExecContext(c.Request.Context(),
		`UPDATE agent_runtimes SET state='draining', updated_at=NOW() WHERE id=$1`, row.RuntimeID)

	payload := taskmanager.StopRuntimePayload{
		RuntimeID:   row.RuntimeID,
		ContainerID: row.ContainerID,
		DrainSecs:   30,
	}

	taskID, err := h.taskMgr.Enqueue(
		c.Request.Context(), row.NodeID,
		taskmanager.TaskStopRuntime, payload,
		taskmanager.WithPriority(90),
		taskmanager.WithActor("admin-stop"),
		taskmanager.WithRuntimeID(row.RuntimeID),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "dispatch STOP_RUNTIME task: " + err.Error()})
		return
	}

	h.log.Info("STOP_RUNTIME dispatched via admin",
		zap.String("endpoint_id", endpointID),
		zap.String("node_id", row.NodeID),
		zap.String("runtime_id", row.RuntimeID),
		zap.String("task_id", taskID),
	)
	c.JSON(http.StatusOK, gin.H{
		"message":     "stop task dispatched",
		"endpoint_id": endpointID,
		"runtime_id":  row.RuntimeID,
		"task_id":     taskID,
	})
}

// ─── RestartModel ─────────────────────────────────────────────────────────────

// RestartModel handles POST /admin/v1/models/:id/restart
func (h *ControllerHandler) RestartModel(c *gin.Context) {
	endpointID := c.Query("endpoint_id")
	if endpointID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "endpoint_id query param required"})
		return
	}

	row, ok := h.loadRuntime(c, endpointID)
	if !ok {
		return
	}

	newRuntimeID := uuid.New().String()
	if !h.ensureRuntimeRow(c, newRuntimeID, row) {
		return
	}

	// Mark the old runtime as stopping (if it exists and is running).
	if row.RuntimeID != "" {
		_, _ = h.db.ExecContext(c.Request.Context(), `
			UPDATE agent_runtimes SET state='stopping', updated_at=NOW()
			WHERE id=$1 AND state NOT IN ('deleted','stopped','failed')`, row.RuntimeID)
	}
	_, _ = h.db.ExecContext(c.Request.Context(),
		`UPDATE model_endpoints SET lifecycle_state='loading', updated_at=NOW() WHERE id=$1`, endpointID)

	var stopTaskID string
	// Only dispatch a stop if there's actually a running container to stop.
	if row.RuntimeID != "" && row.ContainerID != "" {
		stopPayload := taskmanager.StopRuntimePayload{
			RuntimeID:   row.RuntimeID,
			ContainerID: row.ContainerID,
			DrainSecs:   15,
		}
		stopTaskID, _ = h.taskMgr.Enqueue(
			c.Request.Context(), row.NodeID,
			taskmanager.TaskStopRuntime, stopPayload,
			taskmanager.WithPriority(90),
			taskmanager.WithActor("admin-restart"),
			taskmanager.WithRuntimeID(row.RuntimeID),
		)
	}

	startPayload := buildStartPayload(newRuntimeID, row)
	startPayload.StaleContainerNames = []string{row.RuntimeName}

	startTaskID, err := h.taskMgr.Enqueue(
		c.Request.Context(), row.NodeID,
		taskmanager.TaskStartModel, startPayload,
		taskmanager.WithPriority(85),
		taskmanager.WithActor("admin-restart"),
		taskmanager.WithRuntimeID(newRuntimeID),
		taskmanager.WithTimeout(20*time.Minute),
		taskmanager.WithIdempotencyKey("restart:"+row.NodeID+":"+newRuntimeID),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "dispatch START_MODEL task: " + err.Error()})
		return
	}

	h.log.Info("restart dispatched via admin",
		zap.String("endpoint_id", endpointID),
		zap.String("node_id", row.NodeID),
		zap.String("old_runtime_id", row.RuntimeID),
		zap.String("new_runtime_id", newRuntimeID),
		zap.String("stop_task_id", stopTaskID),
		zap.String("start_task_id", startTaskID),
	)
	c.JSON(http.StatusAccepted, gin.H{
		"message":        "restart tasks dispatched",
		"endpoint_id":    endpointID,
		"old_runtime_id": row.RuntimeID,
		"new_runtime_id": newRuntimeID,
		"stop_task_id":   stopTaskID,
		"start_task_id":  startTaskID,
	})
}

// ─── UpgradeModel ─────────────────────────────────────────────────────────────

// UpgradeModel handles POST /admin/v1/models/:id/upgrade
func (h *ControllerHandler) UpgradeModel(c *gin.Context) {
	endpointID := c.Query("endpoint_id")
	var input struct {
		Image string `json:"image" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	row, ok := h.loadRuntime(c, endpointID)
	if !ok {
		return
	}

	newRuntimeID := uuid.New().String()
	if !h.ensureRuntimeRow(c, newRuntimeID, row) {
		return
	}

	if row.RuntimeID != "" {
		_, _ = h.db.ExecContext(c.Request.Context(), `
			UPDATE agent_runtimes SET state='stopping', updated_at=NOW()
			WHERE id=$1 AND state NOT IN ('deleted','stopped','failed')`, row.RuntimeID)
		stopPayload := taskmanager.StopRuntimePayload{
			RuntimeID:   row.RuntimeID,
			ContainerID: row.ContainerID,
			DrainSecs:   15,
		}
		_, _ = h.taskMgr.Enqueue(
			c.Request.Context(), row.NodeID,
			taskmanager.TaskStopRuntime, stopPayload,
			taskmanager.WithPriority(90),
			taskmanager.WithActor("admin-upgrade"),
			taskmanager.WithRuntimeID(row.RuntimeID),
		)
	}

	_, _ = h.db.ExecContext(c.Request.Context(),
		`UPDATE model_endpoints SET lifecycle_state='loading', runtime_image=$1, updated_at=NOW() WHERE id=$2`,
		input.Image, endpointID)

	startPayload := buildStartPayload(newRuntimeID, row)
	startPayload.Image = input.Image
	startPayload.StaleContainerNames = []string{row.RuntimeName}

	taskID, err := h.taskMgr.Enqueue(
		c.Request.Context(), row.NodeID,
		taskmanager.TaskStartModel, startPayload,
		taskmanager.WithPriority(85),
		taskmanager.WithActor("admin-upgrade"),
		taskmanager.WithRuntimeID(newRuntimeID),
		taskmanager.WithTimeout(20*time.Minute),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "dispatch upgrade task: " + err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"message": "upgrade started", "image": input.Image, "task_id": taskID})
}

// ─── RollbackModel ────────────────────────────────────────────────────────────

// RollbackModel handles POST /admin/v1/models/:id/rollback
func (h *ControllerHandler) RollbackModel(c *gin.Context) {
	endpointID := c.Query("endpoint_id")
	var input struct {
		PreviousImage string `json:"previous_image" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	row, ok := h.loadRuntime(c, endpointID)
	if !ok {
		return
	}

	newRuntimeID := uuid.New().String()
	if !h.ensureRuntimeRow(c, newRuntimeID, row) {
		return
	}

	if row.RuntimeID != "" {
		_, _ = h.db.ExecContext(c.Request.Context(), `
			UPDATE agent_runtimes SET state='stopping', updated_at=NOW()
			WHERE id=$1 AND state NOT IN ('deleted','stopped','failed')`, row.RuntimeID)
		stopPayload := taskmanager.StopRuntimePayload{
			RuntimeID:   row.RuntimeID,
			ContainerID: row.ContainerID,
			DrainSecs:   15,
		}
		_, _ = h.taskMgr.Enqueue(
			c.Request.Context(), row.NodeID,
			taskmanager.TaskStopRuntime, stopPayload,
			taskmanager.WithPriority(90),
			taskmanager.WithActor("admin-rollback"),
			taskmanager.WithRuntimeID(row.RuntimeID),
		)
	}

	_, _ = h.db.ExecContext(c.Request.Context(),
		`UPDATE model_endpoints SET lifecycle_state='loading', runtime_image=$1, updated_at=NOW() WHERE id=$2`,
		input.PreviousImage, endpointID)

	startPayload := buildStartPayload(newRuntimeID, row)
	startPayload.Image = input.PreviousImage
	startPayload.StaleContainerNames = []string{row.RuntimeName}

	taskID, err := h.taskMgr.Enqueue(
		c.Request.Context(), row.NodeID,
		taskmanager.TaskStartModel, startPayload,
		taskmanager.WithPriority(85),
		taskmanager.WithActor("admin-rollback"),
		taskmanager.WithRuntimeID(newRuntimeID),
		taskmanager.WithTimeout(20*time.Minute),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "dispatch rollback task: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "rollback started", "image": input.PreviousImage, "task_id": taskID})
}

// ─── GetModelLogs ─────────────────────────────────────────────────────────────

// GetModelLogs handles GET /admin/v1/models/:id/logs
func (h *ControllerHandler) GetModelLogs(c *gin.Context) {
	endpointID := c.Query("endpoint_id")
	if endpointID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "endpoint_id query param required"})
		return
	}
	var errMsg string
	_ = h.db.QueryRowContext(c.Request.Context(), `
		SELECT COALESCE(error_msg, '')
		FROM agent_runtimes
		WHERE endpoint_id = $1
		ORDER BY created_at DESC LIMIT 1`, endpointID,
	).Scan(&errMsg)
	c.JSON(http.StatusOK, gin.H{
		"logs":        errMsg,
		"endpoint_id": endpointID,
		"note":        "full container logs are available on the node agent",
	})
}
