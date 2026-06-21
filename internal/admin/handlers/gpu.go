package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/nexusllm/nexusllm/internal/gpu"
)

// GPUHandler exposes GPU inventory and allocation APIs.
type GPUHandler struct {
	inv *gpu.Inventory
}

// NewGPUHandler constructs a GPUHandler.
func NewGPUHandler(inv *gpu.Inventory) *GPUHandler {
	return &GPUHandler{inv: inv}
}

// RegisterNode handles POST /admin/v1/gpu/nodes
func (h *GPUHandler) RegisterNode(c *gin.Context) {
	var input struct {
		Name          string  `json:"name"           binding:"required"`
		Host          string  `json:"host"           binding:"required"`
		DriverType    string  `json:"driver_type"`
		TotalVRAMMB   int     `json:"total_vram_mb"`
		// ClusterNodeID links this GPU node to the cluster node (from /admin/v1/nodes)
		// Set by the standalone node agent on auto-registration.
		ClusterNodeID string  `json:"node_id"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.DriverType == "" {
		input.DriverType = "docker"
	}
	node, err := h.inv.RegisterNodeWithCluster(c.Request.Context(),
		input.Name, input.Host, input.DriverType,
		input.TotalVRAMMB, input.ClusterNodeID,
	)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, node)
}

// RegisterDevice handles POST /admin/v1/gpu/nodes/:id/devices
func (h *GPUHandler) RegisterDevice(c *gin.Context) {
	nodeID := c.Param("id")
	var input struct {
		DeviceIndex int    `json:"device_index" binding:"required"`
		Name        string `json:"name"         binding:"required"`
		VRAMMB      int    `json:"vram_mb"      binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	dev, err := h.inv.RegisterDevice(c.Request.Context(), nodeID, input.DeviceIndex, input.Name, input.VRAMMB)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, dev)
}

// ListNodes handles GET /admin/v1/gpu/nodes
// Supports optional ?cluster_node_id= query parameter to filter by cluster node.
func (h *GPUHandler) ListNodes(c *gin.Context) {
	clusterNodeID := c.Query("cluster_node_id")
	nodes, err := h.inv.ListNodesByClusterNode(c.Request.Context(), clusterNodeID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if nodes == nil {
		nodes = []gpu.Node{}
	}
	c.JSON(http.StatusOK, gin.H{"data": nodes, "total": len(nodes)})
}

// ListDevices handles GET /admin/v1/gpu/nodes/:id/devices
func (h *GPUHandler) ListDevices(c *gin.Context) {
	nodeID := c.Param("id")
	devs, err := h.inv.ListDevices(c.Request.Context(), nodeID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if devs == nil {
		devs = []gpu.Device{}
	}
	c.JSON(http.StatusOK, gin.H{"data": devs, "total": len(devs)})
}

// PackModels handles POST /admin/v1/gpu/pack
// Simulates GPU packing for a set of models and returns the placement plan.
func (h *GPUHandler) PackModels(c *gin.Context) {
	var input struct {
		NodeID   string                       `json:"node_id"`
		Requests []gpu.ModelPlacementRequest  `json:"models" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	devices, err := h.inv.ListDevices(c.Request.Context(), input.NodeID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	result := gpu.PackModels(devices, input.Requests)
	c.JSON(http.StatusOK, gin.H{
		"assignments":  result.Assignments,
		"unscheduled":  result.Unscheduled,
		"explanation":  gpu.ExplainPacking(result),
	})
}
