package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/nexusllm/nexusllm/internal/usage"
)

// UsageHandler exposes usage query and aggregation APIs.
type UsageHandler struct {
	tracker *usage.Tracker
}

// NewUsageHandler constructs a UsageHandler.
func NewUsageHandler(tracker *usage.Tracker) *UsageHandler {
	return &UsageHandler{tracker: tracker}
}

// GetTeamUsage handles GET /admin/v1/usage/teams/:id
// Query params: from=2026-01-01 to=2026-01-31
func (h *UsageHandler) GetTeamUsage(c *gin.Context) {
	teamID := c.Param("id")
	from := c.DefaultQuery("from", "2026-01-01")
	to := c.DefaultQuery("to", "2026-12-31")

	rows, err := h.tracker.GetTeamDailyUsage(c.Request.Context(), teamID, from, to)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"team_id": teamID, "from": from, "to": to, "data": rows})
}

// GetOrgSpend handles GET /admin/v1/usage/orgs/:id/monthly-spend
func (h *UsageHandler) GetOrgSpend(c *gin.Context) {
	orgID := c.Param("id")
	spend, err := h.tracker.GetOrgMonthlySpend(c.Request.Context(), orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"org_id": orgID, "monthly_spend_usd": spend})
}

// TriggerAggregation handles POST /admin/v1/usage/aggregate
func (h *UsageHandler) TriggerAggregation(c *gin.Context) {
	go h.tracker.Aggregate(c.Request.Context())
	c.JSON(http.StatusAccepted, gin.H{"message": "aggregation triggered"})
}
