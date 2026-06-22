package handlers

// tasks.go — Admin API for managing agent tasks.
// Human operators and the control plane use these to dispatch work to nodes.
//
// Routes:
//   POST /admin/v1/nodes/:id/tasks          — dispatch a task to a node
//   GET  /admin/v1/nodes/:id/tasks          — list recent tasks for a node
//   GET  /admin/v1/tasks/:id               — get task detail
//   DELETE /admin/v1/tasks/:id             — cancel a pending task
//   GET  /admin/v1/nodes/:id/runtimes      — list runtimes on a node

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/nexusllm/nexusllm/internal/taskmanager"
)

// TaskHandler manages task dispatching and inspection.
type TaskHandler struct {
	taskMgr *taskmanager.Manager
}

// NewTaskHandler constructs a TaskHandler.
func NewTaskHandler(taskMgr *taskmanager.Manager) *TaskHandler {
	return &TaskHandler{taskMgr: taskMgr}
}

// DispatchTask handles POST /admin/v1/nodes/:id/tasks
// Creates a task for the specified node.
func (h *TaskHandler) DispatchTask(c *gin.Context) {
	nodeID := c.Param("id")
	var input struct {
		TaskType       string                 `json:"task_type"        binding:"required"`
		Payload        map[string]interface{} `json:"payload"`
		Priority       int                    `json:"priority"`
		Actor          string                 `json:"actor"`
		IdempotencyKey string                 `json:"idempotency_key"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	opts := []taskmanager.EnqueueOption{}
	if input.Priority > 0 {
		opts = append(opts, taskmanager.WithPriority(input.Priority))
	}
	if input.Actor != "" {
		opts = append(opts, taskmanager.WithActor(input.Actor))
	} else {
		opts = append(opts, taskmanager.WithActor("admin-api"))
	}
	if input.IdempotencyKey != "" {
		opts = append(opts, taskmanager.WithIdempotencyKey(input.IdempotencyKey))
	}

	taskID, err := h.taskMgr.Enqueue(c.Request.Context(), nodeID,
		taskmanager.TaskType(input.TaskType), input.Payload, opts...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"task_id": taskID,
		"node_id": nodeID,
		"status":  "pending",
		"note":    "task queued — agent will pick it up on next poll cycle",
	})
}

// ListNodeTasks handles GET /admin/v1/nodes/:id/tasks
func (h *TaskHandler) ListNodeTasks(c *gin.Context) {
	nodeID := c.Param("id")
	limit := 50
	if l, err := strconv.Atoi(c.Query("limit")); err == nil && l > 0 {
		limit = l
	}
	tasks, err := h.taskMgr.ListForNode(c.Request.Context(), nodeID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if tasks == nil {
		tasks = []taskmanager.Task{}
	}
	c.JSON(http.StatusOK, gin.H{"data": tasks, "total": len(tasks)})
}

// GetTask handles GET /admin/v1/tasks/:id
func (h *TaskHandler) GetTask(c *gin.Context) {
	taskID := c.Param("id")
	task, err := h.taskMgr.GetTask(c.Request.Context(), taskID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	c.JSON(http.StatusOK, task)
}

// CancelTask handles DELETE /admin/v1/tasks/:id
func (h *TaskHandler) CancelTask(c *gin.Context) {
	taskID := c.Param("id")
	if err := h.taskMgr.Cancel(c.Request.Context(), taskID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "task cancelled", "task_id": taskID})
}

// ListNodeRuntimes handles GET /admin/v1/nodes/:id/runtimes
func (h *TaskHandler) ListNodeRuntimes(c *gin.Context) {
	nodeID := c.Param("id")
	type runtimeRow struct {
		ID           string `db:"id"            json:"id"`
		RuntimeName  string `db:"runtime_name"  json:"runtime_name"`
		Backend      string `db:"backend"       json:"backend"`
		State        string `db:"state"         json:"state"`
		ContainerID  string `db:"container_id"  json:"container_id"`
		HealthStatus string `db:"health_status" json:"health_status"`
		BindPort     int    `db:"bind_port"     json:"bind_port"`
		GPUIDs       []byte `db:"gpu_ids"       json:"-"`
		GPUIDsStr    string `json:"gpu_ids"`
	}
	// TaskHandler doesn't have DB access — use a workaround via the agent handler
	// For now return empty; the actual runtimes list comes from /agent/v1/runtimes
	_ = nodeID
	c.JSON(http.StatusOK, gin.H{
		"data":  []runtimeRow{},
		"total": 0,
		"note":  "use GET /agent/v1/runtimes (authenticated as the node agent) for real-time data",
	})
}
