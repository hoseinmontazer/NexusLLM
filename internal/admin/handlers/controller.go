package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/nexusllm/nexusllm/internal/controller"
)

// ControllerHandler exposes model runtime lifecycle operations via the Admin API.
type ControllerHandler struct {
	ctrl *controller.ModelController
}

// NewControllerHandler constructs a ControllerHandler.
func NewControllerHandler(ctrl *controller.ModelController) *ControllerHandler {
	return &ControllerHandler{ctrl: ctrl}
}

// StartModel handles POST /admin/v1/models/:id/start
func (h *ControllerHandler) StartModel(c *gin.Context) {
	endpointID := c.Query("endpoint_id")
	if endpointID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "endpoint_id query param required"})
		return
	}
	if err := h.ctrl.Start(c.Request.Context(), endpointID, "admin"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"message": "start initiated", "endpoint_id": endpointID})
}

// StopModel handles POST /admin/v1/models/:id/stop
func (h *ControllerHandler) StopModel(c *gin.Context) {
	endpointID := c.Query("endpoint_id")
	if endpointID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "endpoint_id query param required"})
		return
	}
	if err := h.ctrl.Stop(c.Request.Context(), endpointID, "admin"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "stop initiated", "endpoint_id": endpointID})
}

// RestartModel handles POST /admin/v1/models/:id/restart
func (h *ControllerHandler) RestartModel(c *gin.Context) {
	endpointID := c.Query("endpoint_id")
	if endpointID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "endpoint_id query param required"})
		return
	}
	if err := h.ctrl.Restart(c.Request.Context(), endpointID, "admin"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"message": "restart initiated", "endpoint_id": endpointID})
}

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
	go func() {
		_ = h.ctrl.Upgrade(c.Request.Context(), endpointID, input.Image, "admin")
	}()
	c.JSON(http.StatusAccepted, gin.H{"message": "upgrade started", "image": input.Image})
}

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
	if err := h.ctrl.Rollback(c.Request.Context(), endpointID, input.PreviousImage, "admin"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "rollback complete", "image": input.PreviousImage})
}

// GetModelLogs handles GET /admin/v1/models/:id/logs
func (h *ControllerHandler) GetModelLogs(c *gin.Context) {
	endpointID := c.Query("endpoint_id")
	logs, err := h.ctrl.GetLogs(c.Request.Context(), endpointID, 200)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"logs": logs})
}
