package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
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
	RuntimeID     string   `db:"runtime_id"`
	NodeID        string   `db:"node_id"`
	ContainerID   string   `db:"container_id"`
	RuntimeName   string   `db:"runtime_name"`
	Backend       string   `db:"backend"`
	Image         string   `db:"image"`
	ModelName     string   `db:"model_name"`
	BindHost      string   `db:"bind_host"`
	BindPort      int      `db:"bind_port"`
	GPUDevices    []int    // decoded from gpu_ids JSON
	GPUIDsJSON    string   `db:"gpu_ids_json"`
	TensorParallel int     `db:"tensor_parallel"`
	GPUMemoryUtil float64  `db:"gpu_memory_util"`
	MaxModelLen   int      `db:"max_model_len"`
	Dtype         string   `db:"dtype"`
	Quantization  string   `db:"quantization"`
	ExecutionMode string   `db:"execution_mode"`
	GGUFPath      string   `db:"gguf_path"`
	HFRepo        string   `db:"hf_repo"`
	HFFile        string   `db:"hf_file"`
	CtxSize       int      `db:"ctx_size"`
	NGPULayers    int      `db:"n_gpu_layers"`
	MemoryLimit   string   `db:"memory_limit"`
	EndpointID    string   `db:"endpoint_id"`
	ModelID       string   `db:"model_id"`
}

// loadRuntime fetches the agent_runtime row for an endpoint, joined with
// model_runtime_configs for the backend parameters the agent needs.
func (h *ControllerHandler) loadRuntime(c *gin.Context, endpointID string) (*runtimeRow, bool) {
	var row runtimeRow
	err := h.db.GetContext(c.Request.Context(), &row, `
		SELECT
			ar.id                                         AS runtime_id,
			ar.node_id::text                              AS node_id,
			COALESCE(ar.container_id, '')                 AS container_id,
			ar.runtime_name                               AS runtime_name,
			ar.backend                                    AS backend,
			COALESCE(ar.image, '')                        AS image,
			ar.model_name                                 AS model_name,
			COALESCE(ar.bind_host, '')                    AS bind_host,
			COALESCE(ar.bind_port, 0)                     AS bind_port,
			COALESCE(ar.gpu_ids::text,  '[]')             AS gpu_ids_json,
			COALESCE(mrc.tensor_parallel,  1)             AS tensor_parallel,
			COALESCE(mrc.gpu_memory_util,  0.9)           AS gpu_memory_util,
			COALESCE(mrc.max_model_len,    0)             AS max_model_len,
			COALESCE(mrc.dtype,            'auto')        AS dtype,
			COALESCE(mrc.quantization,     '')            AS quantization,
			COALESCE(mrc.execution_mode,   'auto')        AS execution_mode,
			COALESCE(mrc.gguf_path,        '')            AS gguf_path,
			COALESCE(mrc.hf_repo,          '')            AS hf_repo,
			COALESCE(mrc.hf_file,          '')            AS hf_file,
			COALESCE(mrc.ctx_size,         4096)          AS ctx_size,
			COALESCE(mrc.n_gpu_layers,     0)             AS n_gpu_layers,
			COALESCE(mrc.memory_limit,     '')            AS memory_limit,
			COALESCE(ar.endpoint_id::text, '')            AS endpoint_id,
			ar.model_id::text                             AS model_id
		FROM agent_runtimes ar
		LEFT JOIN model_runtime_configs mrc ON mrc.model_id = ar.model_id
		WHERE ar.endpoint_id = $1
		  AND ar.state NOT IN ('deleted', 'stopped', 'failed', 'archived')
		ORDER BY ar.created_at DESC
		LIMIT 1`, endpointID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no active runtime for endpoint: " + err.Error()})
		return nil, false
	}
	if row.NodeID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "runtime has no assigned node — cannot dispatch task"})
		return nil, false
	}
	return &row, true
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

	// Allocate a fresh runtime ID so the agent creates a new runtime record
	// rather than reusing the stale one (which may be in an error state).
	newRuntimeID := uuid.New().String()

	// Insert a fresh agent_runtimes row so the agent has a target to update.
	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO agent_runtimes
		  (id, node_id, endpoint_id, model_id, runtime_name, backend,
		   state, gpu_ids, bind_host, bind_port, cpu_affinity, numa_node)
		VALUES ($1,$2,$3,$4,$5,$6,'pending',$7::jsonb,$8,$9,'',-1)
		ON CONFLICT DO NOTHING`,
		newRuntimeID, row.NodeID, endpointID, row.ModelID,
		row.RuntimeName, row.Backend,
		row.GPUIDsJSON, row.BindHost, row.BindPort,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create runtime record: " + err.Error()})
		return
	}

	payload := taskmanager.StartModelPayload{
		RuntimeID:      newRuntimeID,
		EndpointID:     endpointID,
		ModelID:        row.ModelID,
		RuntimeName:    row.RuntimeName,
		Backend:        row.Backend,
		Image:          row.Image,
		ModelName:      row.ModelName,
		BindHost:       row.BindHost,
		BindPort:       row.BindPort,
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
		Env:            map[string]string{},
	}

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
// Dispatches a STOP_RUNTIME followed by a START_MODEL to the node agent.
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

	// Insert new runtime record for the restart.
	_, _ = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO agent_runtimes
		  (id, node_id, endpoint_id, model_id, runtime_name, backend,
		   state, gpu_ids, bind_host, bind_port, cpu_affinity, numa_node)
		VALUES ($1,$2,$3,$4,$5,$6,'pending',$7::jsonb,$8,$9,'',-1)
		ON CONFLICT DO NOTHING`,
		newRuntimeID, row.NodeID, endpointID, row.ModelID,
		row.RuntimeName, row.Backend,
		row.GPUIDsJSON, row.BindHost, row.BindPort,
	)

	// Mark the old runtime as stopping.
	_, _ = h.db.ExecContext(c.Request.Context(),
		`UPDATE agent_runtimes SET state='stopping', updated_at=NOW() WHERE id=$1`, row.RuntimeID)
	_, _ = h.db.ExecContext(c.Request.Context(),
		`UPDATE model_endpoints SET lifecycle_state='loading', updated_at=NOW() WHERE id=$1`, endpointID)

	// Stop the old container.
	stopPayload := taskmanager.StopRuntimePayload{
		RuntimeID:   row.RuntimeID,
		ContainerID: row.ContainerID,
		DrainSecs:   15,
	}
	stopTaskID, stopErr := h.taskMgr.Enqueue(
		c.Request.Context(), row.NodeID,
		taskmanager.TaskStopRuntime, stopPayload,
		taskmanager.WithPriority(90),
		taskmanager.WithActor("admin-restart"),
		taskmanager.WithRuntimeID(row.RuntimeID),
	)
	if stopErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "dispatch STOP_RUNTIME task: " + stopErr.Error()})
		return
	}

	// Start the new container.
	startPayload := taskmanager.StartModelPayload{
		RuntimeID:      newRuntimeID,
		EndpointID:     endpointID,
		ModelID:        row.ModelID,
		RuntimeName:    row.RuntimeName,
		Backend:        row.Backend,
		Image:          row.Image,
		ModelName:      row.ModelName,
		BindHost:       row.BindHost,
		BindPort:       row.BindPort,
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
		Env:            map[string]string{},
		StaleContainerNames: []string{row.RuntimeName},
	}
	startTaskID, startErr := h.taskMgr.Enqueue(
		c.Request.Context(), row.NodeID,
		taskmanager.TaskStartModel, startPayload,
		taskmanager.WithPriority(85),
		taskmanager.WithActor("admin-restart"),
		taskmanager.WithRuntimeID(newRuntimeID),
		taskmanager.WithTimeout(20*time.Minute),
		taskmanager.WithIdempotencyKey("restart:"+row.NodeID+":"+newRuntimeID),
	)
	if startErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "dispatch START_MODEL task: " + startErr.Error()})
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
// Dispatches a restart with a new image.
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
	_, _ = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO agent_runtimes
		  (id, node_id, endpoint_id, model_id, runtime_name, backend,
		   state, gpu_ids, bind_host, bind_port, cpu_affinity, numa_node)
		VALUES ($1,$2,$3,$4,$5,$6,'pending',$7::jsonb,$8,$9,'',-1)
		ON CONFLICT DO NOTHING`,
		newRuntimeID, row.NodeID, endpointID, row.ModelID,
		row.RuntimeName, row.Backend,
		row.GPUIDsJSON, row.BindHost, row.BindPort,
	)

	_, _ = h.db.ExecContext(c.Request.Context(),
		`UPDATE agent_runtimes SET state='stopping', updated_at=NOW() WHERE id=$1`, row.RuntimeID)
	_, _ = h.db.ExecContext(c.Request.Context(),
		`UPDATE model_endpoints SET lifecycle_state='loading', runtime_image=$1, updated_at=NOW() WHERE id=$2`,
		input.Image, endpointID)

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

	startPayload := taskmanager.StartModelPayload{
		RuntimeID:      newRuntimeID,
		EndpointID:     endpointID,
		ModelID:        row.ModelID,
		RuntimeName:    row.RuntimeName,
		Backend:        row.Backend,
		Image:          input.Image, // new image
		ModelName:      row.ModelName,
		BindHost:       row.BindHost,
		BindPort:       row.BindPort,
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
		Env:            map[string]string{},
		StaleContainerNames: []string{row.RuntimeName},
	}
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
	_, _ = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO agent_runtimes
		  (id, node_id, endpoint_id, model_id, runtime_name, backend,
		   state, gpu_ids, bind_host, bind_port, cpu_affinity, numa_node)
		VALUES ($1,$2,$3,$4,$5,$6,'pending',$7::jsonb,$8,$9,'',-1)
		ON CONFLICT DO NOTHING`,
		newRuntimeID, row.NodeID, endpointID, row.ModelID,
		row.RuntimeName, row.Backend,
		row.GPUIDsJSON, row.BindHost, row.BindPort,
	)

	_, _ = h.db.ExecContext(c.Request.Context(),
		`UPDATE agent_runtimes SET state='stopping', updated_at=NOW() WHERE id=$1`, row.RuntimeID)
	_, _ = h.db.ExecContext(c.Request.Context(),
		`UPDATE model_endpoints SET lifecycle_state='loading', runtime_image=$1, updated_at=NOW() WHERE id=$2`,
		input.PreviousImage, endpointID)

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

	startPayload := taskmanager.StartModelPayload{
		RuntimeID:      newRuntimeID,
		EndpointID:     endpointID,
		ModelID:        row.ModelID,
		RuntimeName:    row.RuntimeName,
		Backend:        row.Backend,
		Image:          input.PreviousImage,
		ModelName:      row.ModelName,
		BindHost:       row.BindHost,
		BindPort:       row.BindPort,
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
		Env:            map[string]string{},
		StaleContainerNames: []string{row.RuntimeName},
	}
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
// Returns recent log lines stored in the agent_runtimes or endpoint tables.
func (h *ControllerHandler) GetModelLogs(c *gin.Context) {
	endpointID := c.Query("endpoint_id")
	if endpointID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "endpoint_id query param required"})
		return
	}
	// Return the error message from the most recent runtime as a fallback.
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
