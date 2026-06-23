package handlers

// service.go — Admin API handlers for the AI Service Registry.
//
// Routes (to be registered in cmd/admin/main.go):
//   POST   /admin/v1/services               — register a new AI service
//   GET    /admin/v1/services               — list all services (?type=EMBEDDING etc.)
//   GET    /admin/v1/services/:id/reservation — get resource reservation
//   PUT    /admin/v1/services/:id/reservation — upsert resource reservation
//   POST   /admin/v1/services/deploy        — register + auto-place + deploy

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/controller"
	"github.com/nexusllm/nexusllm/internal/placement"
	"github.com/nexusllm/nexusllm/internal/runtime"
	"github.com/nexusllm/nexusllm/internal/services"
	"github.com/nexusllm/nexusllm/internal/taskmanager"
)

// ServiceHandler manages AI Service Registry admin operations.
type ServiceHandler struct {
	db        *sqlx.DB
	registry  *services.Registry
	placement *placement.Engine
	runtime   *runtime.Registry
	ctrl      *controller.ModelController
	taskMgr   *taskmanager.Manager // optional; nil = local Docker deployment only
}

// NewServiceHandler constructs a ServiceHandler.
func NewServiceHandler(
	db *sqlx.DB,
	svcRegistry *services.Registry,
	placementEng *placement.Engine,
	runtimeRegistry *runtime.Registry,
	ctrl *controller.ModelController,
) *ServiceHandler {
	return &ServiceHandler{
		db:        db,
		registry:  svcRegistry,
		placement: placementEng,
		runtime:   runtimeRegistry,
		ctrl:      ctrl,
	}
}

// WithTaskManager wires in a task manager so DeployService can use the
// node-agent pipeline (START_MODEL) instead of direct Docker calls.
func (h *ServiceHandler) WithTaskManager(tm *taskmanager.Manager) *ServiceHandler {
	h.taskMgr = tm
	return h
}

// RegisterService handles POST /admin/v1/services
// Registers an already-running external AI service (CPU or GPU).
func (h *ServiceHandler) RegisterService(c *gin.Context) {
	var req services.RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	modelID, epID, err := h.registry.Register(c.Request.Context(), req)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}

	_ = h.runtime.Reload(c.Request.Context())

	c.JSON(http.StatusCreated, gin.H{
		"model_id":     modelID,
		"endpoint_id":  epID,
		"service_type": req.ServiceType,
		"runtime_type": req.RuntimeType,
		"note":         "service registered and active in gateway routing",
	})
}

// DeployService handles POST /admin/v1/services/deploy
//
// Full orchestration path for launching a new AI service:
//  1. Placement engine chooses node/GPU/CPU/NUMA
//  2. Service is registered in the AI Service Registry
//  3. Container is started via ModelController (for GPU/CPU runtimes)
//  4. Gateway registry is reloaded
func (h *ServiceHandler) DeployService(c *gin.Context) {
	var input struct {
		services.RegisterRequest

		// Container / deployment
		Image          string   `json:"image"`
		HFModelID      string   `json:"hf_model_id"`
		HFToken        string   `json:"hf_token"`
		GPUCount       int      `json:"gpu_count"`
		ExtraArgs      []string `json:"extra_args"`
		GPUMemUtil     float64  `json:"gpu_memory_util"`
		TensorParallel int      `json:"tensor_parallel"`
		StartNow       *bool    `json:"start_now"`
	}

	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// ── 1. Run placement engine ────────────────────────────────────────────
	rType := placement.RuntimeGPU
	if input.RuntimeType == "CPU_RUNTIME" {
		rType = placement.RuntimeCPU
	}

	pReq := placement.Request{
		ModelName:   input.Name,
		ServiceType: placement.ServiceType(input.ServiceType),
		RuntimeType: rType,
		Priority:    placement.Priority(orDefaultStr(input.Priority, "normal")),
		MinVRAMMB:   input.MinVRAMMB,
		MaxVRAMMB:   input.MaxVRAMMB,
		GPUCount:    orDefaultInt(input.GPUCount, 1),
		CPUCores:    input.CPUCores,
		NUMANode:    orDefaultInt(input.NUMANode, -1),
		RAMMBLimit:  input.RAMMBLimit,
	}
	if rType == placement.RuntimeCPU {
		pReq.GPUCount = 0
	}

	dec, err := h.placement.Decide(c.Request.Context(), pReq)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{
			"error":  err.Error(),
			"detail": "placement engine could not find sufficient resources",
		})
		return
	}

	// ── 2. Register service ────────────────────────────────────────────────
	modelID, epID, err := h.registry.Register(c.Request.Context(), input.RegisterRequest)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}

	// ── 3. Apply placement decision ────────────────────────────────────────
	pReq.ModelID = modelID
	_ = h.placement.Apply(c.Request.Context(), dec, pReq, epID)

	// ── 4. Start container if image provided ───────────────────────────────
	shouldStart := true
	if input.StartNow != nil {
		shouldStart = *input.StartNow
	}

	var containerID string
	if shouldStart && input.Image != "" {
		env := map[string]string{}
		if input.HFToken != "" {
			env["HUGGING_FACE_HUB_TOKEN"] = input.HFToken
		}

		gpuDevices := dec.GPUDeviceIndices
		if rType == placement.RuntimeCPU {
			gpuDevices = []int{}
		}

		gpuMemUtil := input.GPUMemUtil
		if gpuMemUtil == 0 {
			gpuMemUtil = 0.90
		}
		tp := input.TensorParallel
		if tp == 0 {
			tp = len(gpuDevices)
			if tp < 1 {
				tp = 1
			}
		}

		hfModel := input.HFModelID
		if hfModel == "" {
			hfModel = input.Name
		}

		spec := controller.RuntimeSpec{
			ModelName:       hfModel,
			ServedModelName: input.Name,
			EndpointID:      epID,
			BackendType:     orDefaultStr(input.BackendType, "cpu_native"),
			Image:           input.Image,
			BindHost:        input.Host,
			BindPort:        input.Port,
			GPUDevices:      gpuDevices,
			TensorParallel:  tp,
			GPUMemoryUtil:   gpuMemUtil,
			ExtraArgs:       input.ExtraArgs,
			Env:             env,
		}

		// CPU_RUNTIME: set CPU/memory limits from placement decision
		if rType == placement.RuntimeCPU {
			if dec.CPUCores > 0 {
				spec.CPULimit = orDefaultStr("", containerCPUStr(dec.CPUCores))
			}
			if dec.RAMMBLimit > 0 {
				spec.MemoryLimit = containerMemStr(dec.RAMMBLimit)
			}
		}

		containerID, err = h.ctrl.StartRaw(c.Request.Context(), epID, modelID, spec, "admin")
		if err != nil {
			c.JSON(http.StatusAccepted, gin.H{
				"model_id":    modelID,
				"endpoint_id": epID,
				"placement":   dec,
				"warning":     "service registered but container failed: " + err.Error(),
			})
			return
		}
	}

	// ── 5. Reload gateway registry ─────────────────────────────────────────
	_ = h.runtime.Reload(c.Request.Context())

	c.JSON(http.StatusCreated, gin.H{
		"model_id":     modelID,
		"endpoint_id":  epID,
		"service_type": input.ServiceType,
		"runtime_type": input.RuntimeType,
		"placement": gin.H{
			"node_id":     dec.NodeID,
			"node_host":   dec.NodeHost,
			"gpu_devices": dec.GPUDeviceIndices,
			"cpu_cores":   dec.CPUCores,
			"numa_node":   dec.NUMANode,
			"vram_mb":     dec.TotalVRAMMB,
			"score":       dec.Score,
			"reason":      dec.Reason,
		},
		"container_id": containerID,
		"started":      containerID != "",
	})
}

// ListServices handles GET /admin/v1/services
func (h *ServiceHandler) ListServices(c *gin.Context) {
	serviceType := c.Query("type")
	svcs, err := h.registry.List(c.Request.Context(), serviceType)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": svcs, "total": len(svcs)})
}

// GetReservation handles GET /admin/v1/services/:id/reservation
func (h *ServiceHandler) GetReservation(c *gin.Context) {
	modelID := c.Param("id")
	rr, err := h.registry.GetReservation(c.Request.Context(), modelID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no reservation found for service " + modelID})
		return
	}
	c.JSON(http.StatusOK, rr)
}

// UpsertReservation handles PUT /admin/v1/services/:id/reservation
func (h *ServiceHandler) UpsertReservation(c *gin.Context) {
	modelID := c.Param("id")
	var req services.ReservationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.ModelID = modelID
	if req.Priority == "" {
		req.Priority = "normal"
	}
	if req.PreferredRuntime == "" {
		req.PreferredRuntime = "GPU_RUNTIME"
	}
	if err := h.registry.UpsertReservation(c.Request.Context(), req); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "reservation updated", "model_id": modelID})
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func orDefaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func orDefaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func containerCPUStr(cores int) string {
	if cores <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", cores)
}

func containerMemStr(mb int64) string {
	if mb <= 0 {
		return ""
	}
	return fmt.Sprintf("%dm", mb)
}
