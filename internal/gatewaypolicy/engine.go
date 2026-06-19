// Package gatewaypolicy enforces AI gateway controls — temperature limits,
// context/output token caps, tool restrictions, and stream/function-call
// permissions — at the org → team → api_key scope hierarchy.
package gatewaypolicy

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/models"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// GatewayPolicy is the Go representation of a gateway_policies row.
type GatewayPolicy struct {
	Scope                string   `db:"scope"`
	MaxTemperature       *float64 `db:"max_temperature"`
	MaxContextTokens     *int     `db:"max_context_tokens"`
	MaxOutputTokens      *int     `db:"max_output_tokens"`
	StreamAllowed        bool     `db:"stream_allowed"`
	FunctionCallAllowed  bool     `db:"function_call_allowed"`
	AllowedModels        []string `db:"-"`
	DeniedModels         []string `db:"-"`
	AllowedToolNames     []string `db:"-"`
	DeniedToolNames      []string `db:"-"`
	Enabled              bool     `db:"enabled"`
}

// Violation is a policy breach with a machine-readable code.
type Violation struct {
	Code    string
	Message string
}

func (v Violation) Error() string { return fmt.Sprintf("%s: %s", v.Code, v.Message) }

// Engine evaluates gateway policies in org → team → api_key order.
type Engine struct {
	db  *sqlx.DB
	rdb *redis.Client
	log *zap.Logger
}

// NewEngine constructs a gateway policy Engine.
func NewEngine(db *sqlx.DB, rdb *redis.Client, log *zap.Logger) *Engine {
	return &Engine{db: db, rdb: rdb, log: log}
}

// Enforce checks the request against all applicable gateway policies and
// either passes (returning nil) or returns a Violation.
// It also mutates the request to clamp parameters within allowed bounds.
func (e *Engine) Enforce(
	ctx context.Context,
	orgID, teamID, apiKeyID string,
	req *models.InferenceRequest,
	inputTokenEstimate int,
) *Violation {
	policies := e.loadPolicies(ctx, orgID, teamID, apiKeyID)

	for _, p := range policies {
		if !p.Enabled {
			continue
		}

		// Model restriction
		if len(p.AllowedModels) > 0 && !contains(p.AllowedModels, req.Model) {
			return &Violation{Code: "model_not_allowed_by_gateway",
				Message: fmt.Sprintf("model %q is not in allowed list for this %s", req.Model, p.Scope)}
		}
		if contains(p.DeniedModels, req.Model) {
			return &Violation{Code: "model_denied_by_gateway",
				Message: fmt.Sprintf("model %q is denied for this %s", req.Model, p.Scope)}
		}

		// Streaming
		if req.Stream && !p.StreamAllowed {
			return &Violation{Code: "stream_not_allowed",
				Message: "streaming is not permitted for this " + p.Scope}
		}

		// Function calls / tools
		if len(req.Tools) > 0 && !p.FunctionCallAllowed {
			return &Violation{Code: "function_call_not_allowed",
				Message: "tool/function calls are not permitted for this " + p.Scope}
		}

		// Tool name restrictions
		for _, tool := range req.Tools {
			name := extractToolName(tool)
			if contains(p.DeniedToolNames, name) {
				return &Violation{Code: "tool_denied",
					Message: fmt.Sprintf("tool %q is denied for this %s", name, p.Scope)}
			}
			if len(p.AllowedToolNames) > 0 && !contains(p.AllowedToolNames, name) {
				return &Violation{Code: "tool_not_allowed",
					Message: fmt.Sprintf("tool %q is not in allowed list", name)}
			}
		}

		// Context length
		if p.MaxContextTokens != nil && inputTokenEstimate > *p.MaxContextTokens {
			return &Violation{Code: "context_too_long",
				Message: fmt.Sprintf("input tokens %d exceeds gateway limit %d", inputTokenEstimate, *p.MaxContextTokens)}
		}

		// Clamp output tokens
		if p.MaxOutputTokens != nil {
			if req.MaxTokens == nil || *req.MaxTokens > *p.MaxOutputTokens {
				clamped := *p.MaxOutputTokens
				req.MaxTokens = &clamped
			}
		}

		// Clamp temperature
		if p.MaxTemperature != nil && req.Temperature != nil && *req.Temperature > *p.MaxTemperature {
			clamped := *p.MaxTemperature
			req.Temperature = &clamped
		}
	}
	return nil
}

// ─── private ──────────────────────────────────────────────────────────────────

func (e *Engine) loadPolicies(ctx context.Context, orgID, teamID, apiKeyID string) []GatewayPolicy {
	var rows []struct {
		Scope               string   `db:"scope"`
		MaxTemperature      *float64 `db:"max_temperature"`
		MaxContextTokens    *int     `db:"max_context_tokens"`
		MaxOutputTokens     *int     `db:"max_output_tokens"`
		StreamAllowed       bool     `db:"stream_allowed"`
		FunctionCallAllowed bool     `db:"function_call_allowed"`
		Enabled             bool     `db:"enabled"`
	}

	_ = e.db.SelectContext(ctx, &rows, `
		SELECT scope, max_temperature, max_context_tokens, max_output_tokens,
		       stream_allowed, function_call_allowed, enabled
		FROM gateway_policies
		WHERE enabled = TRUE
		  AND (
		        (scope = 'org'     AND scope_id = $1)
		     OR (scope = 'team'    AND scope_id = $2)
		     OR (scope = 'api_key' AND scope_id = $3)
		      )
		ORDER BY CASE scope WHEN 'org' THEN 1 WHEN 'team' THEN 2 ELSE 3 END`,
		orgID, teamID, apiKeyID)

	out := make([]GatewayPolicy, len(rows))
	for i, r := range rows {
		out[i] = GatewayPolicy{
			Scope:               r.Scope,
			MaxTemperature:      r.MaxTemperature,
			MaxContextTokens:    r.MaxContextTokens,
			MaxOutputTokens:     r.MaxOutputTokens,
			StreamAllowed:       r.StreamAllowed,
			FunctionCallAllowed: r.FunctionCallAllowed,
			Enabled:             r.Enabled,
		}
	}
	return out
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
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
