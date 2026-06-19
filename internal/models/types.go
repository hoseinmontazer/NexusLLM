package models

import "time"

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
	Delta        *Message    `json:"delta,omitempty"`
	FinishReason *string     `json:"finish_reason"`
	Logprobs     interface{} `json:"logprobs,omitempty"`
}

// Usage holds token counts for a completion.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
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
	ID             string    `db:"id" json:"id"`
	OrgID          string    `db:"org_id" json:"org_id"`
	Name           string    `db:"name" json:"name"`
	Slug           string    `db:"slug" json:"slug"`
	Priority       int       `db:"priority" json:"priority"` // 1-10
	Active         bool      `db:"active" json:"active"`
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time `db:"updated_at" json:"updated_at"`
}

// Policy holds rate-limit and quota rules for a team.
type Policy struct {
	ID                 string    `db:"id" json:"id"`
	TeamID             string    `db:"team_id" json:"team_id"`
	RPM                int       `db:"rpm" json:"rpm"`                 // requests per minute
	TPD                int       `db:"tpd" json:"tpd"`                 // tokens per day
	MaxConcurrent      int       `db:"max_concurrent" json:"max_concurrent"`
	MaxContextTokens   int       `db:"max_context_tokens" json:"max_context_tokens"`
	AllowedModels      []string  `db:"allowed_models" json:"allowed_models"`
	CreatedAt          time.Time `db:"created_at" json:"created_at"`
	UpdatedAt          time.Time `db:"updated_at" json:"updated_at"`
}

// APIKey is the metadata stored in the DB (raw key is shown once).
type APIKey struct {
	ID             string    `db:"id" json:"id"`
	TeamID         string    `db:"team_id" json:"team_id"`
	Name           string    `db:"name" json:"name"`
	KeyHash        string    `db:"key_hash" json:"-"`
	KeyPrefix      string    `db:"key_prefix" json:"key_prefix"`
	Active         bool      `db:"active" json:"active"`
	LastUsedAt     *time.Time `db:"last_used_at" json:"last_used_at,omitempty"`
	ExpiresAt      *time.Time `db:"expires_at" json:"expires_at,omitempty"`
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time `db:"updated_at" json:"updated_at"`
}

// Model represents a registered inference model.
type Model struct {
	ID          string    `db:"id" json:"id"`
	Name        string    `db:"name" json:"name"`
	DisplayName string    `db:"display_name" json:"display_name"`
	VLLMEndpoint string   `db:"vllm_endpoint" json:"vllm_endpoint"`
	MaxTokens   int       `db:"max_tokens" json:"max_tokens"`
	Active      bool      `db:"active" json:"active"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}
