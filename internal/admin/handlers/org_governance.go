// Package handlers — org_governance.go
//
// Layer-2 org governance API.
//
// These endpoints control org-wide guardrails that sit above project policy.
// They are NOT for per-request rate limiting (that is project policy, Layer-1).
//
// Routes (registered in cmd/admin/main.go):
//
//	GET  /admin/v1/orgs/:id/governance         — get current governance state
//	PUT  /admin/v1/orgs/:id/governance         — update governance settings
//	POST /admin/v1/orgs/:id/governance/disable — disable org (billing/compliance)
//	POST /admin/v1/orgs/:id/governance/enable  — re-enable org
package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/policy"
	"github.com/redis/go-redis/v9"
)

// OrgGovernanceHandler manages Layer-2 org-wide guardrails.
type OrgGovernanceHandler struct {
	db     *sqlx.DB
	rdb    *redis.Client
	engine *policy.Engine
}

// NewOrgGovernanceHandler constructs an OrgGovernanceHandler.
func NewOrgGovernanceHandler(db *sqlx.DB, rdb *redis.Client, engine *policy.Engine) *OrgGovernanceHandler {
	return &OrgGovernanceHandler{db: db, rdb: rdb, engine: engine}
}

// GetGovernance handles GET /admin/v1/orgs/:id/governance
// Returns the current governance state: enabled/disabled flag, monthly token
// budget, and current month's usage from Redis.
func (h *OrgGovernanceHandler) GetGovernance(c *gin.Context) {
	orgID := c.Param("id")

	// Verify org exists
	var exists bool
	if err := h.db.GetContext(c.Request.Context(), &exists,
		`SELECT EXISTS(SELECT 1 FROM organizations WHERE id=$1)`, orgID); err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
		return
	}

	status := h.engine.GetOrgGovernanceStatus(c.Request.Context(), orgID)
	c.JSON(http.StatusOK, status)
}

// UpdateGovernance handles PUT /admin/v1/orgs/:id/governance
// Accepts:
//
//	{
//	  "monthly_token_budget": 1000000000,  // 0 = unlimited
//	  "disabled": false
//	}
//
// These are Layer-2 guardrails only. They do not affect per-project rate limits.
func (h *OrgGovernanceHandler) UpdateGovernance(c *gin.Context) {
	orgID := c.Param("id")

	var exists bool
	if err := h.db.GetContext(c.Request.Context(), &exists,
		`SELECT EXISTS(SELECT 1 FROM organizations WHERE id=$1)`, orgID); err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
		return
	}

	var input struct {
		MonthlyTokenBudget *int64 `json:"monthly_token_budget"`
		Disabled           *bool  `json:"disabled"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()

	if input.Disabled != nil {
		if err := h.engine.SetOrgDisabled(ctx, orgID, *input.Disabled); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update org disabled flag"})
			return
		}
	}

	if input.MonthlyTokenBudget != nil {
		if *input.MonthlyTokenBudget < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "monthly_token_budget must be >= 0 (0 = unlimited)"})
			return
		}
		if err := h.engine.SetOrgMonthlyBudget(ctx, orgID, *input.MonthlyTokenBudget); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update monthly token budget"})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "org governance updated",
		"org_id":  orgID,
	})
}

// DisableOrg handles POST /admin/v1/orgs/:id/governance/disable
// Immediately blocks all inference requests from this org.
// Use for billing suspension or compliance holds.
func (h *OrgGovernanceHandler) DisableOrg(c *gin.Context) {
	orgID := c.Param("id")
	if err := h.engine.SetOrgDisabled(c.Request.Context(), orgID, true); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "organization disabled", "org_id": orgID})
}

// EnableOrg handles POST /admin/v1/orgs/:id/governance/enable
// Lifts a billing/compliance hold and allows inference requests again.
func (h *OrgGovernanceHandler) EnableOrg(c *gin.Context) {
	orgID := c.Param("id")
	if err := h.engine.SetOrgDisabled(c.Request.Context(), orgID, false); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "organization enabled", "org_id": orgID})
}
