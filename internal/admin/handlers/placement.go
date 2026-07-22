package handlers

// placement.go — Admin API handler for placement operations.
//
// Routes:
//   POST /admin/v1/placement/simulate  — dry-run placement for a model
//   GET  /admin/v1/placement/decisions — recent placement decisions

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/project"
	"github.com/nexusllm/nexusllm/internal/scheduler"
)

// PlacementHandler exposes placement admin operations.
type PlacementHandler struct {
	db    *sqlx.DB
	sched *scheduler.Scheduler
}

// NewPlacementHandler constructs a PlacementHandler.
func NewPlacementHandler(db *sqlx.DB, sched *scheduler.Scheduler) *PlacementHandler {
	return &PlacementHandler{db: db, sched: sched}
}

// Simulate handles POST /admin/v1/placement/simulate
// Runs the placement algorithm without committing anything.
// Useful for capacity planning and pre-flight checks.
func (h *PlacementHandler) Simulate(c *gin.Context) {
	var input struct {
		ModelName      string `json:"model_name" binding:"required"`
		RuntimeType    string `json:"runtime_type"` // GPU_RUNTIME | CPU_RUNTIME (informational)
		PriorityWeight int    `json:"priority"`     // [0–1000], defaults to 500
		MinVRAMMB      int64  `json:"min_vram_mb"`
		GPUCount       int    `json:"gpu_count"`
		CPUCores       int    `json:"cpu_cores"`
		RAMMBLimit     int64  `json:"ram_mb"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.PriorityWeight == 0 {
		input.PriorityWeight = 500
	}
	if input.PriorityWeight < 0 || input.PriorityWeight > 1000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "priority must be between 0 and 1000"})
		return
	}

	execMode := "auto"
	if input.RuntimeType == "CPU_RUNTIME" {
		execMode = "cpu"
	} else if input.RuntimeType == "GPU_RUNTIME" {
		execMode = "gpu"
	}

	req := scheduler.PlacementRequest{
		ModelName:         input.ModelName,
		RequiredVRAMMB:    input.MinVRAMMB,
		RequiredGPUs:      orDefaultInt(input.GPUCount, 1),
		RequiredCPU:       input.CPUCores,
		RequiredRAMMB:     input.RAMMBLimit,
		ExecutionMode:     execMode,
		Mode:              scheduler.ModeAuto,
		PriorityWeight:    project.PriorityWeight(input.PriorityWeight),
		EffectivePriority: input.PriorityWeight,
	}
	if execMode == "cpu" {
		req.RequiredGPUs = 0
	}

	dec, err := h.sched.Decide(c.Request.Context(), req)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{
			"feasible": false,
			"error":    err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"feasible": true,
		"decision": gin.H{
			"node_id":     dec.NodeID,
			"node_host":   dec.NodeHostname,
			"gpu_devices": dec.GPUDeviceIndices,
			"numa_node":   dec.NUMANode,
			"strategy":    dec.Trace.Reason,
			"score":       dec.NodeScore,
			"reason":      dec.Reason,
			"decided_at":  dec.DecidedAt,
		},
	})
}

// ListDecisions handles GET /admin/v1/placement/decisions
func (h *PlacementHandler) ListDecisions(c *gin.Context) {
	type decRow struct {
		ID         string    `db:"id"          json:"id"`
		ModelID    string    `db:"model_id"    json:"model_id"`
		NodeID     *string   `db:"node_id"     json:"node_id"`
		GPUDevices []byte    `db:"gpu_devices" json:"-"`
		GPUStr     string    `json:"gpu_devices"`
		CPUCores   int       `db:"cpu_cores"   json:"cpu_cores"`
		NUMANode   int       `db:"numa_node"   json:"numa_node"`
		Strategy   string    `db:"strategy"    json:"strategy"`
		Score      float64   `db:"score"       json:"score"`
		Reason     string    `db:"reason"      json:"reason"`
		Applied    bool      `db:"applied"     json:"applied"`
		CreatedAt  time.Time `db:"created_at"  json:"created_at"`
	}
	var rows []decRow
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT id, model_id, node_id, gpu_devices, cpu_cores, numa_node,
		       strategy, score, COALESCE(reason,'') AS reason, applied, created_at
		FROM placement_decisions
		ORDER BY created_at DESC LIMIT 100`); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	for i := range rows {
		if rows[i].GPUDevices != nil {
			rows[i].GPUStr = string(rows[i].GPUDevices)
		} else {
			rows[i].GPUStr = "[]"
		}
	}
	if rows == nil {
		rows = []decRow{}
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": len(rows)})
}
