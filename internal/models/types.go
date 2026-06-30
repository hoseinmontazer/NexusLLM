package models

import (
	"encoding/json"
	"time"
)

// NormalizeStreamChunk rewrites a raw SSE data payload to be strictly
// OpenAI-compatible by stripping non-standard fields produced by reasoning
// models (e.g. Qwen3 / llama.cpp).
//
// Specifically it:
//   - Removes delta.reasoning_content (llama.cpp thinking extension)
//   - Skips chunks where delta.content is null AND reasoning_content is
//     non-null (pure reasoning tokens that carry no visible content)
//
// Returns (rewritten JSON, true) when the chunk should be forwarded, or
// ("", false) when the chunk should be silently dropped.
func NormalizeStreamChunk(payload string) (string, bool) {
	// Fast path: no reasoning_content present → nothing to do.
	if !containsBytes(payload, "reasoning_content") {
		return payload, true
	}

	// Parse just enough to inspect / mutate delta fields.
	var chunk struct {
		ID                string          `json:"id"`
		Object            string          `json:"object"`
		Created           int64           `json:"created"`
		Model             string          `json:"model"`
		SystemFingerprint string          `json:"system_fingerprint,omitempty"`
		Choices           []chunkChoice   `json:"choices"`
		Usage             json.RawMessage `json:"usage,omitempty"`
	}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		// If we can't parse it, forward as-is and let the client deal.
		return payload, true
	}

	for i := range chunk.Choices {
		d := chunk.Choices[i].Delta
		if d == nil {
			continue
		}
		// Drop the non-standard field.
		d.ReasoningContent = nil

		// If content is also null/empty this was a pure reasoning chunk —
		// skip it entirely (return false so the caller doesn't forward it).
		if d.Content == nil {
			return "", false
		}
		if s, ok := d.Content.(string); ok && s == "" {
			return "", false
		}
	}

	out, err := json.Marshal(chunk)
	if err != nil {
		return payload, true
	}
	return string(out), true
}

// chunkChoice is the per-choice type used only for stream normalisation.
type chunkChoice struct {
	Index        int             `json:"index"`
	Delta        *chunkDelta     `json:"delta,omitempty"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}

// chunkDelta mirrors the OpenAI streaming delta, with the non-standard
// reasoning_content field added so we can unmarshal and then remove it.
type chunkDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          interface{}     `json:"content"` // string | null
	ToolCalls        json.RawMessage `json:"tool_calls,omitempty"`
	ReasoningContent interface{}     `json:"reasoning_content,omitempty"`
}

func containsBytes(s, sub string) bool {
	return len(s) >= len(sub) && func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}()
}

// InferenceRequest mirrors the OpenAI Chat Completions request body.
type InferenceRequest struct {
	Model            string        `json:"model"`
	Messages         []Message     `json:"messages"`
	Stream           bool          `json:"stream"`
	MaxTokens        *int          `json:"max_tokens,omitempty"`
	Temperature      *float64      `json:"temperature,omitempty"`
	TopP             *float64      `json:"top_p,omitempty"`
	N                *int          `json:"n,omitempty"`
	Stop             interface{}   `json:"stop,omitempty"`
	PresencePenalty  *float64      `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64      `json:"frequency_penalty,omitempty"`
	LogitBias        interface{}   `json:"logit_bias,omitempty"`
	User             string        `json:"user,omitempty"`
	Tools            []interface{} `json:"tools,omitempty"`
	ToolChoice       interface{}   `json:"tool_choice,omitempty"`
	ResponseFormat   interface{}   `json:"response_format,omitempty"`
	Seed             *int64        `json:"seed,omitempty"`
	StreamOptions    interface{}   `json:"stream_options,omitempty"`

	// Thinking/reasoning mode control.
	// When non-nil, overrides the model's deployment default.
	// Supported by llama.cpp (via chat_template_kwargs) and vLLM (via thinking field).
	Thinking *ThinkingControl `json:"thinking,omitempty"`

	// ChatTemplateKwargs passes extra template variables to llama.cpp.
	// Used to disable thinking: {"thinking": false}
	ChatTemplateKwargs map[string]interface{} `json:"chat_template_kwargs,omitempty"`
}

// ThinkingControl mirrors the Anthropic/vLLM thinking field format.
// {"type":"enabled","budget_tokens":1024} or {"type":"disabled"}
type ThinkingControl struct {
	// Type is "enabled" or "disabled".
	Type string `json:"type"`
	// BudgetTokens caps the internal reasoning token count (type=enabled only).
	BudgetTokens *int `json:"budget_tokens,omitempty"`
}

// ThinkingEnabled returns a ThinkingControl that enables reasoning.
func ThinkingEnabled(budgetTokens int) *ThinkingControl {
	return &ThinkingControl{Type: "enabled", BudgetTokens: &budgetTokens}
}

// ThinkingDisabled returns a ThinkingControl that disables reasoning.
func ThinkingDisabled() *ThinkingControl {
	return &ThinkingControl{Type: "disabled"}
}

// Message is a single chat message.
type Message struct {
	Role       string      `json:"role"`
	Content    interface{} `json:"content"` // string or array of content parts
	Name       string      `json:"name,omitempty"`
	ToolCalls  interface{} `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
}

// ChatCompletionResponse mirrors the OpenAI Chat Completions response.
type ChatCompletionResponse struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	Choices           []Choice `json:"choices"`
	Usage             Usage    `json:"usage"`
	SystemFingerprint string   `json:"system_fingerprint,omitempty"`
}

// Choice is one generation choice in a completion response.
type Choice struct {
	Index        int         `json:"index"`
	Message      *Message    `json:"message,omitempty"`
	Delta        *Delta      `json:"delta,omitempty"`
	FinishReason *string     `json:"finish_reason"`
	Logprobs     interface{} `json:"logprobs,omitempty"`
}

// Delta is the streaming delta object in a chat completion chunk.
// It extends the standard OpenAI delta with optional reasoning_content
// used by llama.cpp / Qwen3 thinking models. The gateway strips
// reasoning_content before forwarding to preserve strict OpenAI compatibility.
type Delta struct {
	Role             string      `json:"role,omitempty"`
	Content          interface{} `json:"content"` // string or null — always emitted so clients see null → ""
	ToolCalls        interface{} `json:"tool_calls,omitempty"`
	ReasoningContent interface{} `json:"reasoning_content,omitempty"` // Qwen3/llama.cpp extension — stripped by gateway
}

// Usage holds token counts for a completion.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// Reasoning-specific counts (populated by thinking-capable models).
	// ThinkingTokens is the number of tokens consumed by internal reasoning.
	// VisibleTokens is completion_tokens minus thinking_tokens.
	ThinkingTokens int `json:"thinking_tokens,omitempty"`
}

// EmbeddingRequest mirrors the OpenAI Embeddings request body.
type EmbeddingRequest struct {
	Model          string      `json:"model"`
	Input          interface{} `json:"input"` // string or []string
	EncodingFormat string      `json:"encoding_format,omitempty"`
	Dimensions     *int        `json:"dimensions,omitempty"`
	User           string      `json:"user,omitempty"`
}

// EmbeddingResponse mirrors the OpenAI Embeddings response.
type EmbeddingResponse struct {
	Object string          `json:"object"`
	Data   []EmbeddingData `json:"data"`
	Model  string          `json:"model"`
	Usage  EmbeddingUsage  `json:"usage"`
}

// EmbeddingData holds a single embedding vector.
type EmbeddingData struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

// EmbeddingUsage holds token counts for an embedding request.
type EmbeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ModelListResponse mirrors the OpenAI /v1/models response.
type ModelListResponse struct {
	Object string        `json:"object"`
	Data   []ModelObject `json:"data"`
}

// ModelObject describes a single model.
type ModelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ErrorResponse is the standard OpenAI-compatible error envelope.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail carries error code, message, and type.
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}

// Organization represents a top-level tenant.
type Organization struct {
	ID        string    `db:"id" json:"id"`
	Name      string    `db:"name" json:"name"`
	Slug      string    `db:"slug" json:"slug"`
	Active    bool      `db:"active" json:"active"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

// Team represents a sub-tenant within an organization.
type Team struct {
	ID        string    `db:"id" json:"id"`
	OrgID     string    `db:"org_id" json:"org_id"`
	Name      string    `db:"name" json:"name"`
	Slug      string    `db:"slug" json:"slug"`
	Priority  int       `db:"priority" json:"priority"` // 1-10
	Active    bool      `db:"active" json:"active"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

// Policy holds rate-limit and quota rules for a team.
type Policy struct {
	ID               string    `db:"id" json:"id"`
	TeamID           string    `db:"team_id" json:"team_id"`
	RPM              int       `db:"rpm" json:"rpm"` // requests per minute
	TPD              int       `db:"tpd" json:"tpd"` // tokens per day
	MaxConcurrent    int       `db:"max_concurrent" json:"max_concurrent"`
	MaxContextTokens int       `db:"max_context_tokens" json:"max_context_tokens"`
	AllowedModels    []string  `db:"allowed_models" json:"allowed_models"`
	CreatedAt        time.Time `db:"created_at" json:"created_at"`
	UpdatedAt        time.Time `db:"updated_at" json:"updated_at"`
}

// APIKey is the metadata stored in the DB (raw key is shown once).
type APIKey struct {
	ID         string     `db:"id" json:"id"`
	TeamID     string     `db:"team_id" json:"team_id"`
	Name       string     `db:"name" json:"name"`
	KeyHash    string     `db:"key_hash" json:"-"`
	KeyPrefix  string     `db:"key_prefix" json:"key_prefix"`
	Active     bool       `db:"active" json:"active"`
	LastUsedAt *time.Time `db:"last_used_at" json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `db:"expires_at" json:"expires_at,omitempty"`
	CreatedAt  time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt  time.Time  `db:"updated_at" json:"updated_at"`
}

// Model represents a registered inference model.
type Model struct {
	ID           string    `db:"id" json:"id"`
	Name         string    `db:"name" json:"name"`
	DisplayName  string    `db:"display_name" json:"display_name"`
	VLLMEndpoint string    `db:"vllm_endpoint" json:"vllm_endpoint"`
	MaxTokens    int       `db:"max_tokens" json:"max_tokens"`
	Active       bool      `db:"active" json:"active"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time `db:"updated_at" json:"updated_at"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Multi-Service API types (added for AI Platform expansion)
// ─────────────────────────────────────────────────────────────────────────────

// RerankRequest mirrors the Cohere / Jina rerank API.
type RerankRequest struct {
	Model           string   `json:"model"`
	Query           string   `json:"query"     binding:"required"`
	Documents       []string `json:"documents" binding:"required"`
	TopN            *int     `json:"top_n,omitempty"`
	ReturnDocuments *bool    `json:"return_documents,omitempty"`
}

// RerankResult is one ranked document.
type RerankResult struct {
	Index          int     `json:"index"`
	RelevanceScore float64 `json:"relevance_score"`
	Document       *string `json:"document,omitempty"` // only if return_documents=true
}

// RerankResponse is the response from a rerank endpoint.
type RerankResponse struct {
	Model   string         `json:"model"`
	Results []RerankResult `json:"results"`
	Usage   EmbeddingUsage `json:"usage"`
}

// TranscriptionRequest mirrors the OpenAI /v1/audio/transcriptions API.
// Note: in real usage this is multipart/form-data; we model the parsed fields.
type TranscriptionRequest struct {
	Model       string  `json:"model"`
	Language    string  `json:"language,omitempty"`
	Prompt      string  `json:"prompt,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	// File content is handled separately in the handler (multipart upload).
}

// TranscriptionResponse mirrors the OpenAI transcription response.
type TranscriptionResponse struct {
	Text string `json:"text"`
}

// SpeechRequest mirrors the OpenAI /v1/audio/speech API.
type SpeechRequest struct {
	Model          string  `json:"model"           binding:"required"`
	Input          string  `json:"input"           binding:"required"`
	Voice          string  `json:"voice,omitempty"`
	ResponseFormat string  `json:"response_format,omitempty"` // mp3, opus, aac, flac, wav
	Speed          float64 `json:"speed,omitempty"`
}

// OCRRequest describes a request to the /v1/ocr endpoint.
type OCRRequest struct {
	Model    string `json:"model"`
	ImageURL string `json:"image_url,omitempty"` // URL or base64 data URI
	Language string `json:"language,omitempty"`
}

// OCRResponse is the parsed text from an OCR request.
type OCRResponse struct {
	Text  string    `json:"text"`
	Model string    `json:"model"`
	Pages []OCRPage `json:"pages,omitempty"`
}

// OCRPage is text extracted from one page/region.
type OCRPage struct {
	Page int    `json:"page"`
	Text string `json:"text"`
}

// ServiceType constants mirror those in the placement and services packages.
// Reproduced here to keep models/ free of cross-package dependencies.
const (
	ServiceTypeChat      = "CHAT"
	ServiceTypeEmbedding = "EMBEDDING"
	ServiceTypeRerank    = "RERANK"
	ServiceTypeSTT       = "STT"
	ServiceTypeTTS       = "TTS"
	ServiceTypeOCR       = "OCR"
	ServiceTypeAgent     = "AGENT"
	ServiceTypeMCP       = "MCP"
)
