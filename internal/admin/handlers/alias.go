package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/nexusllm/nexusllm/internal/alias"
)

// AliasHandler manages virtual model name aliases.
type AliasHandler struct {
	resolver *alias.Resolver
}

// NewAliasHandler constructs an AliasHandler.
func NewAliasHandler(resolver *alias.Resolver) *AliasHandler {
	return &AliasHandler{resolver: resolver}
}

// CreateAlias handles POST /admin/v1/aliases
func (h *AliasHandler) CreateAlias(c *gin.Context) {
	var input struct {
		Alias   string `json:"alias"    binding:"required"`
		ModelID string `json:"model_id" binding:"required"`
		Scope   string `json:"scope"    binding:"required"` // global | org | team
		ScopeID string `json:"scope_id"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.resolver.CreateAlias(c.Request.Context(), input.Alias, input.ModelID, input.Scope, input.ScopeID); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"alias": input.Alias, "model_id": input.ModelID,
		"scope": input.Scope, "scope_id": input.ScopeID,
	})
}

// DeleteAlias handles DELETE /admin/v1/aliases
func (h *AliasHandler) DeleteAlias(c *gin.Context) {
	var input struct {
		Alias   string `json:"alias"   binding:"required"`
		Scope   string `json:"scope"   binding:"required"`
		ScopeID string `json:"scope_id"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	_ = h.resolver.DeleteAlias(c.Request.Context(), input.Alias, input.Scope, input.ScopeID)
	c.JSON(http.StatusOK, gin.H{"message": "alias deleted"})
}

// ListAliases handles GET /admin/v1/aliases?team_id=...&org_id=...
func (h *AliasHandler) ListAliases(c *gin.Context) {
	teamID := c.Query("team_id")
	orgID := c.Query("org_id")
	rows, err := h.resolver.ListAliases(c.Request.Context(), teamID, orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": len(rows)})
}

// ResolveAlias handles GET /admin/v1/aliases/resolve?alias=gpt-4o&team_id=...&org_id=...
func (h *AliasHandler) ResolveAlias(c *gin.Context) {
	aliasName := c.Query("alias")
	teamID := c.Query("team_id")
	orgID := c.Query("org_id")
	if aliasName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "alias param required"})
		return
	}
	resolved, err := h.resolver.Resolve(c.Request.Context(), aliasName, teamID, orgID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"alias": aliasName, "model_name": resolved})
}
