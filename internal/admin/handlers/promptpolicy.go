package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nexusllm/nexusllm/internal/promptpolicy"
)

// PromptPolicyHandler manages prompt policy CRUD.
type PromptPolicyHandler struct {
	engine *promptpolicy.Engine
}

// NewPromptPolicyHandler constructs a PromptPolicyHandler.
func NewPromptPolicyHandler(engine *promptpolicy.Engine) *PromptPolicyHandler {
	return &PromptPolicyHandler{engine: engine}
}

// CreatePolicy handles POST /admin/v1/prompt-policies
func (h *PromptPolicyHandler) CreatePolicy(c *gin.Context) {
	var input struct {
		Scope              string   `json:"scope"               binding:"required"`
		ScopeID            string   `json:"scope_id"            binding:"required"`
		Name               string   `json:"name"                binding:"required"`
		Priority           int      `json:"priority"`
		SystemPrompt       string   `json:"system_prompt"`
		SystemPromptMode   string   `json:"system_prompt_mode"`
		MaxTemperature     *float64 `json:"max_temperature"`
		MaxTokensOverride  *int     `json:"max_tokens_override"`
		EnablePII          bool     `json:"enable_pii_detection"`
		EnableModeration   bool     `json:"enable_moderation"`
		InputDenyList      []string `json:"input_deny_list"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.Priority == 0 {
		input.Priority = 100
	}
	if input.SystemPromptMode == "" {
		input.SystemPromptMode = "prepend"
	}

	p := promptpolicy.Policy{
		ID:                 uuid.New().String(),
		Scope:              input.Scope,
		ScopeID:            input.ScopeID,
		Name:               input.Name,
		Priority:           input.Priority,
		Enabled:            true,
		SystemPrompt:       input.SystemPrompt,
		SystemPromptMode:   input.SystemPromptMode,
		MaxTemperature:     input.MaxTemperature,
		MaxTokensOverride:  input.MaxTokensOverride,
		EnablePIIDetection: input.EnablePII,
		EnableModeration:   input.EnableModeration,
		InputDenyList:      input.InputDenyList,
	}
	if err := h.engine.CreatePolicy(c.Request.Context(), p); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": p.ID, "name": p.Name})
}
