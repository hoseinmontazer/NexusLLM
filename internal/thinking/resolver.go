// Package thinking implements thinking/reasoning mode resolution for the
// NexusLLM gateway.
//
// Design:
//   - Each model has a deployment default: thinking_enabled (true|false)
//   - Each request can override: {"thinking": {"type":"enabled"}} or {"type":"disabled"}
//   - The gateway auto-disables thinking when max_tokens < min_thinking_tokens
//     to prevent empty responses caused by reasoning consuming the token budget
//   - On empty response with thinking active → retry once with thinking disabled
//   - Backend-specific injection:
//     llama.cpp:  chat_template_kwargs.thinking = false  OR  -thk 0 at startup
//     vLLM:       thinking.type = "disabled"
//     others:     system prompt directive
package thinking

import (
	"context"
	"encoding/json"

	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/models"
)

// ModelCaps holds the thinking capability flags for a deployed model.
type ModelCaps struct {
	SupportsThinking  bool
	ThinkingEnabled   bool // deployment default
	MinThinkingTokens int  // auto-disable if max_tokens < this
	BackendType       string
}

// Resolver reads model capability flags and resolves the effective thinking
// mode for each request.
type Resolver struct {
	db *sqlx.DB
}

// NewResolver constructs a Resolver.
func NewResolver(db *sqlx.DB) *Resolver { return &Resolver{db: db} }

// LoadCaps returns the thinking capabilities for a model.
// Returns zero-value ModelCaps (SupportsThinking=false) when the model is
// not found or the migration has not been applied.
func (r *Resolver) LoadCaps(ctx context.Context, modelName string) ModelCaps {
	var caps ModelCaps
	err := r.db.QueryRowContext(ctx, `
		SELECT
		    COALESCE(supports_thinking, FALSE),
		    COALESCE(thinking_enabled,  FALSE),
		    COALESCE(min_thinking_tokens, 500),
		    COALESCE(backend_type, '')
		FROM models
		WHERE name = $1 AND enabled = TRUE
		LIMIT 1`, modelName,
	).Scan(&caps.SupportsThinking, &caps.ThinkingEnabled,
		&caps.MinThinkingTokens, &caps.BackendType)
	if err != nil {
		// Column may not exist yet (pre-027 schema) — return safe defaults.
		return ModelCaps{}
	}
	return caps
}

// ResolveMode decides whether thinking should be active for this request.
//
// Resolution order (highest priority first):
//  1. Explicit request override via req.Thinking (vLLM-style)
//  2. Explicit request override via chat_template_kwargs.thinking (llama.cpp-style)
//  3. Auto-disable: max_tokens < caps.MinThinkingTokens
//  4. Model deployment default: caps.ThinkingEnabled
func ResolveMode(req *models.InferenceRequest, caps ModelCaps) bool {
	if !caps.SupportsThinking {
		return false
	}
	// 1. Explicit vLLM-style override: {"thinking": {"type": "enabled|disabled"}}
	if req.Thinking != nil {
		return req.Thinking.Type == "enabled"
	}
	// 2. Explicit llama.cpp-style override: {"chat_template_kwargs": {"thinking": false}}
	// Clients that talk directly to llama.cpp use this field. We must honour it
	// so that passing "thinking": false doesn't get overwritten by the gateway.
	if req.ChatTemplateKwargs != nil {
		if v, ok := req.ChatTemplateKwargs["thinking"]; ok {
			switch val := v.(type) {
			case bool:
				return val
			case float64: // JSON numbers decode as float64
				return val != 0
			case string:
				return val == "true" || val == "1" || val == "enabled"
			}
		}
	}
	// 3. Auto-disable when token budget is too tight for meaningful reasoning
	if req.MaxTokens != nil && caps.MinThinkingTokens > 0 &&
		*req.MaxTokens < caps.MinThinkingTokens {
		return false
	}
	// 4. Model deployment default
	return caps.ThinkingEnabled
}

// InjectThinkingControl modifies the request in-place to signal the backend
// about the resolved thinking mode. Returns a copy so the original is unchanged.
//
// llama.cpp: injects chat_template_kwargs.thinking = true/false
// vLLM:      injects thinking.type = "enabled"|"disabled"
// Others:    no injection (falls back to prompt directive in caller)
func InjectThinkingControl(req models.InferenceRequest, thinkingOn bool, caps ModelCaps) models.InferenceRequest {
	if !caps.SupportsThinking {
		return req
	}

	switch caps.BackendType {
	case "llamacpp":
		// llama.cpp uses chat_template_kwargs to pass thinking flag to the
		// Jinja template. {"thinking": false} disables <think> generation.
		if req.ChatTemplateKwargs == nil {
			req.ChatTemplateKwargs = make(map[string]interface{})
		} else {
			// Shallow copy to avoid mutating the original map.
			copied := make(map[string]interface{}, len(req.ChatTemplateKwargs))
			for k, v := range req.ChatTemplateKwargs {
				copied[k] = v
			}
			req.ChatTemplateKwargs = copied
		}
		req.ChatTemplateKwargs["thinking"] = thinkingOn

	case "vllm":
		// vLLM uses a top-level "thinking" object (Anthropic-style).
		if thinkingOn {
			budget := 2048
			if req.MaxTokens != nil && *req.MaxTokens > 100 {
				// Use up to half the token budget for reasoning.
				budget = *req.MaxTokens / 2
			}
			req.Thinking = models.ThinkingEnabled(budget)
		} else {
			req.Thinking = models.ThinkingDisabled()
		}

	default:
		// openai_compat, ollama, tgi, etc. — no native thinking control.
		// Caller should inject a system prompt directive if desired.
		if !thinkingOn {
			injectNoThinkDirective(&req)
		}
	}
	return req
}

// injectNoThinkDirective prepends a system message asking the model not to
// produce internal reasoning. Used for backends that have no native flag.
func injectNoThinkDirective(req *models.InferenceRequest) {
	directive := models.Message{
		Role:    "system",
		Content: "Do not produce internal reasoning or chain-of-thought steps. Return only the final answer directly.",
	}
	// If there's already a system message, prepend the directive as a prefix.
	if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
		if s, ok := req.Messages[0].Content.(string); ok {
			req.Messages[0].Content = directive.Content.(string) + "\n\n" + s
			return
		}
	}
	req.Messages = append([]models.Message{directive}, req.Messages...)
}

// EstimateThinkingTokens approximates the number of reasoning tokens in a
// completed response by counting content inside <think>...</think> tags.
// Returns (thinkingTokens, visibleTokens).
func EstimateThinkingTokens(completionText string) (thinking, visible int) {
	in := false
	thinkBuf := 0
	visibleBuf := 0
	runes := []rune(completionText)
	for i := 0; i < len(runes); i++ {
		if !in {
			if string(runes[i:min(i+7, len(runes))]) == "<think>" {
				in = true
				i += 6
				continue
			}
			visibleBuf++
		} else {
			if string(runes[i:min(i+8, len(runes))]) == "</think>" {
				in = false
				i += 7
				continue
			}
			thinkBuf++
		}
	}
	// Approximate tokens as chars/4 (rough heuristic matching GPT tokenization).
	return thinkBuf / 4, visibleBuf / 4
}

// IsEmptyVisible returns true when the visible (non-thinking) content is empty
// or blank — the signal that thinking consumed all tokens.
func IsEmptyVisible(completionContent string) bool {
	// Strip think blocks then check for non-whitespace.
	stripped := stripThinkBlocks(completionContent)
	for _, r := range stripped {
		if r != ' ' && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return true
}

func stripThinkBlocks(s string) string {
	result := make([]byte, 0, len(s))
	in := false
	for i := 0; i < len(s); i++ {
		if !in && i+7 <= len(s) && s[i:i+7] == "<think>" {
			in = true
			i += 6
			continue
		}
		if in && i+8 <= len(s) && s[i:i+8] == "</think>" {
			in = false
			i += 7
			continue
		}
		if !in {
			result = append(result, s[i])
		}
	}
	return string(result)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// MarshalRequest serialises req with all thinking fields populated.
// Ensures chat_template_kwargs and thinking fields are included even when zero.
func MarshalRequest(req models.InferenceRequest) ([]byte, error) {
	return json.Marshal(req)
}
