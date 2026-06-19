// Package promptpolicy applies org → team → model prompt policies to every
// inference request before it reaches the backend. It handles system prompt
// injection, content filtering, PII detection stubs, tool restrictions,
// and output filtering.
package promptpolicy

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/models"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// ─────────────────────────────────────────────────────────────────────────────
// Policy model
// ─────────────────────────────────────────────────────────────────────────────

// Policy is the Go representation of a prompt_policies row.
type Policy struct {
	ID                  string    `db:"id"`
	Scope               string    `db:"scope"`
	ScopeID             string    `db:"scope_id"`
	Name                string    `db:"name"`
	Priority            int       `db:"priority"`
	Enabled             bool      `db:"enabled"`
	SystemPrompt        string    `db:"system_prompt"`
	SystemPromptMode    string    `db:"system_prompt_mode"`
	MaxTemperature      *float64  `db:"max_temperature"`
	MaxTokensOverride   *int      `db:"max_tokens_override"`
	EnablePIIDetection  bool      `db:"enable_pii_detection"`
	EnableModeration    bool      `db:"enable_moderation"`
	AllowedTools        []string  `db:"-"` // unmarshalled from JSONB
	DeniedTools         []string  `db:"-"`
	OutputFilters       []Filter  `db:"-"`
	InputDenyList       []string  `db:"-"`
	InputAllowList      []string  `db:"-"`
	UpdatedAt           time.Time `db:"updated_at"`
}

// Filter is a single output filter rule (regex or keyword).
type Filter struct {
	Type    string `json:"type"`    // "regex" | "keyword"
	Pattern string `json:"pattern"`
	Action  string `json:"action"`  // "block" | "redact"
}

// Decision is the result of evaluating all policies for a request.
type Decision struct {
	Allowed         bool
	BlockReason     string
	ModifiedRequest *models.InferenceRequest // nil = unchanged
}

// ─────────────────────────────────────────────────────────────────────────────
// Engine
// ─────────────────────────────────────────────────────────────────────────────

const policyCacheTTL = 2 * time.Minute

// Engine evaluates prompt policies in org → team → model order.
type Engine struct {
	db  *sqlx.DB
	rdb *redis.Client
	log *zap.Logger
}

// NewEngine constructs a prompt policy Engine.
func NewEngine(db *sqlx.DB, rdb *redis.Client, log *zap.Logger) *Engine {
	return &Engine{db: db, rdb: rdb, log: log}
}

// Evaluate applies all policies for the given org/team/model scope chain
// and returns a Decision. The request may be mutated (system prompt injected,
// temperature clamped, etc.) before reaching vLLM.
func (e *Engine) Evaluate(
	ctx context.Context,
	orgID, teamID, modelID string,
	req *models.InferenceRequest,
) Decision {
	policies := e.loadPolicies(ctx, orgID, teamID, modelID)
	if len(policies) == 0 {
		return Decision{Allowed: true}
	}

	// Work on a shallow copy so callers can compare with original
	modified := *req

	for _, p := range policies {
		if !p.Enabled {
			continue
		}

		// 1. Input deny list
		if blocked, reason := e.checkDenyList(modified.Messages, p.InputDenyList); blocked {
			return Decision{Allowed: false, BlockReason: "input_deny_list: " + reason}
		}

		// 2. Allow list enforcement (if non-empty, only allow listed content)
		if len(p.InputAllowList) > 0 {
			if !e.checkAllowList(modified.Messages, p.InputAllowList) {
				return Decision{Allowed: false, BlockReason: "input_allow_list: content not permitted"}
			}
		}

		// 3. PII detection stub
		if p.EnablePIIDetection {
			if hasPII(modified.Messages) {
				return Decision{Allowed: false, BlockReason: "pii_detected"}
			}
		}

		// 4. Tool restrictions
		if len(p.DeniedTools) > 0 {
			for _, tool := range modified.Tools {
				toolName := extractToolName(tool)
				for _, denied := range p.DeniedTools {
					if strings.EqualFold(toolName, denied) {
						return Decision{Allowed: false, BlockReason: "tool_denied: " + denied}
					}
				}
			}
		}

		// 5. Clamp temperature
		if p.MaxTemperature != nil && modified.Temperature != nil {
			if *modified.Temperature > *p.MaxTemperature {
				clamped := *p.MaxTemperature
				modified.Temperature = &clamped
			}
		}

		// 6. Override max_tokens
		if p.MaxTokensOverride != nil {
			if modified.MaxTokens == nil || *modified.MaxTokens > *p.MaxTokensOverride {
				override := *p.MaxTokensOverride
				modified.MaxTokens = &override
			}
		}

		// 7. System prompt injection
		if p.SystemPrompt != "" {
			modified.Messages = injectSystemPrompt(modified.Messages, p.SystemPrompt, p.SystemPromptMode)
		}
	}

	return Decision{Allowed: true, ModifiedRequest: &modified}
}

// FilterOutput applies output_filters from all policies to the response text.
// Returns the (possibly redacted) text and a flag indicating if it was blocked.
func (e *Engine) FilterOutput(ctx context.Context, orgID, teamID, modelID, text string) (string, bool) {
	policies := e.loadPolicies(ctx, orgID, teamID, modelID)
	for _, p := range policies {
		for _, f := range p.OutputFilters {
			switch f.Type {
			case "regex":
				re, err := regexp.Compile(f.Pattern)
				if err != nil {
					continue
				}
				if re.MatchString(text) {
					if f.Action == "block" {
						return "", true
					}
					text = re.ReplaceAllString(text, "[REDACTED]")
				}
			case "keyword":
				if strings.Contains(strings.ToLower(text), strings.ToLower(f.Pattern)) {
					if f.Action == "block" {
						return "", true
					}
					text = strings.ReplaceAll(text, f.Pattern, "[REDACTED]")
				}
			}
		}
	}
	return text, false
}

// ─── Admin helpers ────────────────────────────────────────────────────────────

// CreatePolicy persists a new prompt policy.
func (e *Engine) CreatePolicy(ctx context.Context, p Policy) error {
	_, err := e.db.ExecContext(ctx, `
		INSERT INTO prompt_policies
		  (id, scope, scope_id, name, priority, enabled,
		   system_prompt, system_prompt_mode, max_temperature, max_tokens_override,
		   enable_pii_detection, enable_moderation)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		p.ID, p.Scope, p.ScopeID, p.Name, p.Priority, p.Enabled,
		p.SystemPrompt, p.SystemPromptMode, p.MaxTemperature, p.MaxTokensOverride,
		p.EnablePIIDetection, p.EnableModeration,
	)
	if err == nil {
		e.invalidateCache(ctx, p.Scope, p.ScopeID)
	}
	return err
}

// ─── private ──────────────────────────────────────────────────────────────────

func (e *Engine) loadPolicies(ctx context.Context, orgID, teamID, modelID string) []Policy {
	// Execution order: org → team → model, sorted by priority ASC within each scope
	var rows []struct {
		ID               string   `db:"id"`
		Scope            string   `db:"scope"`
		ScopeID          string   `db:"scope_id"`
		Name             string   `db:"name"`
		Priority         int      `db:"priority"`
		Enabled          bool     `db:"enabled"`
		SystemPrompt     string   `db:"system_prompt"`
		SystemPromptMode string   `db:"system_prompt_mode"`
		MaxTemperature   *float64 `db:"max_temperature"`
		MaxTokensOverride *int    `db:"max_tokens_override"`
		EnablePII        bool     `db:"enable_pii_detection"`
		EnableMod        bool     `db:"enable_moderation"`
	}

	_ = e.db.SelectContext(ctx, &rows, `
		SELECT id, scope, scope_id, name, priority, enabled,
		       COALESCE(system_prompt,'') AS system_prompt,
		       system_prompt_mode,
		       max_temperature, max_tokens_override,
		       enable_pii_detection, enable_moderation
		FROM prompt_policies
		WHERE enabled = TRUE
		  AND (
		        (scope = 'org'   AND scope_id = $1)
		     OR (scope = 'team'  AND scope_id = $2)
		     OR (scope = 'model' AND scope_id = $3)
		      )
		ORDER BY
		  CASE scope WHEN 'org' THEN 1 WHEN 'team' THEN 2 ELSE 3 END,
		  priority ASC`, orgID, teamID, modelID)

	policies := make([]Policy, len(rows))
	for i, r := range rows {
		policies[i] = Policy{
			ID: r.ID, Scope: r.Scope, ScopeID: r.ScopeID,
			Name: r.Name, Priority: r.Priority, Enabled: r.Enabled,
			SystemPrompt: r.SystemPrompt, SystemPromptMode: r.SystemPromptMode,
			MaxTemperature: r.MaxTemperature, MaxTokensOverride: r.MaxTokensOverride,
			EnablePIIDetection: r.EnablePII, EnableModeration: r.EnableMod,
		}
	}
	return policies
}

func (e *Engine) invalidateCache(ctx context.Context, scope, scopeID string) {
	key := fmt.Sprintf("nexus:promptpolicy:%s:%s", scope, scopeID)
	_ = e.rdb.Del(ctx, key).Err()
}

func (e *Engine) checkDenyList(messages []models.Message, denyList []string) (bool, string) {
	for _, m := range messages {
		text := messageText(m)
		for _, term := range denyList {
			if strings.Contains(strings.ToLower(text), strings.ToLower(term)) {
				return true, term
			}
		}
	}
	return false, ""
}

func (e *Engine) checkAllowList(messages []models.Message, allowList []string) bool {
	for _, m := range messages {
		text := strings.ToLower(messageText(m))
		matched := false
		for _, term := range allowList {
			if strings.Contains(text, strings.ToLower(term)) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func messageText(m models.Message) string {
	if s, ok := m.Content.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", m.Content)
}

func injectSystemPrompt(messages []models.Message, prompt, mode string) []models.Message {
	sysMsg := models.Message{Role: "system", Content: prompt}
	switch mode {
	case "replace":
		out := []models.Message{sysMsg}
		for _, m := range messages {
			if m.Role != "system" {
				out = append(out, m)
			}
		}
		return out
	case "append":
		return append(messages, sysMsg)
	default: // prepend
		return append([]models.Message{sysMsg}, messages...)
	}
}

// hasPII is a stub — production would call an actual PII detection service.
func hasPII(messages []models.Message) bool {
	piiPatterns := []string{
		`\b\d{3}-\d{2}-\d{4}\b`,           // SSN
		`\b4[0-9]{12}(?:[0-9]{3})?\b`,     // Visa
		`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`, // email
	}
	for _, m := range messages {
		text := messageText(m)
		for _, pattern := range piiPatterns {
			if matched, _ := regexp.MatchString(pattern, text); matched {
				return true
			}
		}
	}
	return false
}

func extractToolName(tool interface{}) string {
	if m, ok := tool.(map[string]interface{}); ok {
		if fn, ok := m["function"].(map[string]interface{}); ok {
			if name, ok := fn["name"].(string); ok {
				return name
			}
		}
		if name, ok := m["name"].(string); ok {
			return name
		}
	}
	return ""
}
