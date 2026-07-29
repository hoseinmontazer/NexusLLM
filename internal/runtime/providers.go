// Package runtime — provider backends.
//
// External/cloud providers (OpenAI, Anthropic, Gemini, Azure, OpenRouter,
// Groq, Together, Mistral, Cohere, DeepSeek) implement the same Backend
// interface as every local runtime.
//
// Architecture contract:
//   - Lifecycle methods (ContainerPort, ContainerPortEnvVars,
//     PrepareStartupArgs) are no-ops — providers have no container lifecycle.
//   - Health() performs a lightweight API probe tailored to each provider.
//   - Chat() / Embeddings() forward the request to the provider API,
//     translating the wire format when needed (Anthropic, Azure).
//   - The gateway policy engine, rate limiter, quota, audit, and usage
//     tracker run identically for provider backends and local backends.
//     Nothing is bypassed.
package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nexusllm/nexusllm/internal/models"
)

// ─────────────────────────────────────────────────────────────────────────────
// providerNoopLifecycle — shared no-op mixin for all provider backends.
// Embed this in every provider struct to satisfy the container-lifecycle
// methods of the Backend interface with zero code duplication.
// ─────────────────────────────────────────────────────────────────────────────

type providerNoopLifecycle struct{}

// ContainerPort — providers have no container; always 0.
func (providerNoopLifecycle) ContainerPort() int { return 0 }

// ContainerPortEnvVars — providers have no container; always nil.
func (providerNoopLifecycle) ContainerPortEnvVars(_ int) map[string]string { return nil }

// PrepareStartupArgs — providers never start a server process; args unchanged.
func (providerNoopLifecycle) PrepareStartupArgs(_ ModelStartupCaps, args []string) []string {
	return args
}

// ─────────────────────────────────────────────────────────────────────────────
// openAIProviderBackend — native OpenAI API
// Default base URL: https://api.openai.com
// Wire format: OpenAI Chat Completions (identical to openai_compat).
// Health check: GET /v1/models with Bearer token.
// ─────────────────────────────────────────────────────────────────────────────

type openAIProviderBackend struct {
	providerNoopLifecycle
}

// NewOpenAIProviderBackend constructs the OpenAI provider backend.
func NewOpenAIProviderBackend(_ *http.Client) Backend {
	return &openAIProviderBackend{}
}

func (b *openAIProviderBackend) Type() BackendType { return BackendOpenAI }

func (b *openAIProviderBackend) Health(ctx context.Context, url string, client *http.Client) EndpointHealth {
	return providerHealthCheck(ctx, client, url, "/v1/models", "", BackendOpenAI)
}

func (b *openAIProviderBackend) Models(ctx context.Context, url string, client *http.Client) ([]BackendModel, error) {
	return openAICompatModels(ctx, client, url, "")
}

func (b *openAIProviderBackend) Chat(ctx context.Context, r ChatRequest) (*BackendResponse, error) {
	return openAICompatChat(ctx, r.Client, r)
}

func (b *openAIProviderBackend) Embeddings(ctx context.Context, r EmbedRequest) (*models.EmbeddingResponse, error) {
	return openAICompatEmbeddings(ctx, r.Client, r)
}

// ─────────────────────────────────────────────────────────────────────────────
// anthropicProviderBackend — Anthropic Messages API
// Default base URL: https://api.anthropic.com
// Wire format: Anthropic Messages API (NOT OpenAI-compatible natively).
// Translation: OpenAI request → Anthropic → OpenAI response.
// Health check: GET /v1/models with x-api-key header.
// ─────────────────────────────────────────────────────────────────────────────

type anthropicProviderBackend struct {
	providerNoopLifecycle
}

// NewAnthropicProviderBackend constructs the Anthropic provider backend.
func NewAnthropicProviderBackend(_ *http.Client) Backend {
	return &anthropicProviderBackend{}
}

func (b *anthropicProviderBackend) Type() BackendType { return BackendAnthropic }

func (b *anthropicProviderBackend) Health(ctx context.Context, url string, client *http.Client) EndpointHealth {
	h := EndpointHealth{URL: url, Status: StatusDown, CheckedAt: time.Now()}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/models", nil)
	if err != nil {
		h.Error = err.Error()
		return h
	}
	req.Header.Set("anthropic-version", anthropicVersion)
	// A health probe without a key returns 401, which still means the API is up.
	resp, err := client.Do(req)
	h.LatencyMs = int(time.Since(start).Milliseconds())
	if err != nil {
		h.Error = err.Error()
		return h
	}
	resp.Body.Close()
	// 200 = authenticated + models listed.
	// 401 = no key but API is reachable → healthy (key checked at request time).
	// 4xx other = degraded.
	// 5xx = down.
	switch {
	case resp.StatusCode == 200 || resp.StatusCode == 401:
		h.Status = StatusHealthy
	case resp.StatusCode < 500:
		h.Status = StatusDegraded
		h.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	default:
		h.Status = StatusDown
		h.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return h
}

func (b *anthropicProviderBackend) Models(ctx context.Context, url string, client *http.Client) ([]BackendModel, error) {
	// Anthropic /v1/models returns its own format — return a stub.
	return []BackendModel{{ID: "claude", OwnedBy: "anthropic"}}, nil
}

// anthropicVersion is the required Anthropic API version header.
const anthropicVersion = "2023-06-01"

// Chat translates an OpenAI Chat Completions request to the Anthropic Messages
// API and translates the response back to OpenAI format.
//
// Translation summary:
//   - messages[0].role=="system" → system parameter at top level
//   - max_tokens / max_completion_tokens → max_tokens (required by Anthropic)
//   - temperature, top_p, stop → passed through
//   - stream → stream
//   - Response: content[0].text → choices[0].message.content
//   - Streaming: delta.type=="content_block_delta" → SSE data line
func (b *anthropicProviderBackend) Chat(ctx context.Context, r ChatRequest) (*BackendResponse, error) {
	// Build Anthropic request body.
	system, msgs := extractAnthropicMessages(r.Req.Messages)

	maxTokens := 4096
	if r.Req.MaxTokens != nil {
		maxTokens = *r.Req.MaxTokens
	} else if r.Req.MaxCompletionTokens != nil {
		maxTokens = *r.Req.MaxCompletionTokens
	}

	// Determine model name to send to Anthropic.
	// UpstreamModelName allows the registry to store the Anthropic model ID
	// (e.g. "claude-sonnet-4-5") separately from the NexusLLM name.
	// The proxy handler sets EndpointURL and UpstreamAPIKey; we read the
	// upstream model name from the request if the registry substituted it.
	modelName := r.Req.Model

	anthropicReq := map[string]interface{}{
		"model":      modelName,
		"messages":   msgs,
		"max_tokens": maxTokens,
		"stream":     r.Req.Stream,
	}
	if system != "" {
		anthropicReq["system"] = system
	}
	if r.Req.Temperature != nil {
		anthropicReq["temperature"] = *r.Req.Temperature
	}
	if r.Req.TopP != nil {
		anthropicReq["top_p"] = *r.Req.TopP
	}
	if r.Req.Stop != nil {
		anthropicReq["stop_sequences"] = r.Req.Stop
	}
	if r.Req.Tools != nil {
		anthropicReq["tools"] = r.Req.Tools
	}

	body, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.EndpointURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersion)
	if r.UpstreamAPIKey != "" {
		req.Header.Set("x-api-key", r.UpstreamAPIKey)
	}
	if r.Req.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic chat: %w", err)
	}

	br := &BackendResponse{StatusCode: resp.StatusCode, Headers: map[string]string{}}
	if r.Req.Stream {
		// Wrap the Anthropic SSE stream in a translator that converts
		// Anthropic event format to OpenAI SSE format on-the-fly.
		br.Stream = &anthropicSSEStream{
			reader: bufio.NewReader(resp.Body),
			closer: resp.Body,
			model:  r.Req.Model,
		}
	} else {
		raw, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		// Non-2xx: proxy the error body directly so the client sees the
		// provider's error message.
		if resp.StatusCode >= 300 {
			br.Body = raw
			return br, nil
		}
		translated, transErr := translateAnthropicResponse(raw, r.Req.Model)
		if transErr != nil {
			br.Body = raw // fallback: proxy raw response
		} else {
			br.Body = translated
		}
	}
	return br, nil
}

func (b *anthropicProviderBackend) Embeddings(_ context.Context, _ EmbedRequest) (*models.EmbeddingResponse, error) {
	return nil, fmt.Errorf("anthropic does not support embeddings")
}

// ─── Anthropic translation helpers ────────────────────────────────────────────

// extractAnthropicMessages splits a system message out and converts remaining
// messages to the Anthropic format (role + content string or content blocks).
func extractAnthropicMessages(msgs []models.Message) (system string, out []map[string]interface{}) {
	for _, m := range msgs {
		if m.Role == "system" {
			if s, ok := m.Content.(string); ok {
				system = s
			}
			continue
		}
		out = append(out, map[string]interface{}{
			"role":    m.Role,
			"content": m.Content,
		})
	}
	return system, out
}

// anthropicResponse is the minimal structure of an Anthropic Messages response
// needed for OpenAI translation.
type anthropicResponse struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Role         string `json:"role"`
	Model        string `json:"model"`
	StopReason   string `json:"stop_reason"`
	StopSequence string `json:"stop_sequence"`
	Content      []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// translateAnthropicResponse converts an Anthropic Messages response body to
// an OpenAI Chat Completions response body.
func translateAnthropicResponse(raw []byte, nexusModelName string) ([]byte, error) {
	var ar anthropicResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return nil, err
	}

	text := ""
	if len(ar.Content) > 0 {
		text = ar.Content[0].Text
	}

	finishReason := "stop"
	switch ar.StopReason {
	case "end_turn":
		finishReason = "stop"
	case "max_tokens":
		finishReason = "length"
	case "tool_use":
		finishReason = "tool_calls"
	case "stop_sequence":
		finishReason = "stop"
	}

	oaiResp := models.ChatCompletionResponse{
		ID:      ar.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   nexusModelName,
		Choices: []models.Choice{
			{
				Index: 0,
				Message: &models.Message{
					Role:    "assistant",
					Content: text,
				},
				FinishReason: &finishReason,
			},
		},
		Usage: models.Usage{
			PromptTokens:     ar.Usage.InputTokens,
			CompletionTokens: ar.Usage.OutputTokens,
			TotalTokens:      ar.Usage.InputTokens + ar.Usage.OutputTokens,
		},
	}
	return json.Marshal(oaiResp)
}

// anthropicSSEStream translates Anthropic's SSE event format to OpenAI's SSE
// format so the gateway's existing streaming pipeline works unchanged.
//
// Anthropic SSE events relevant to translation:
//   event: content_block_delta
//   data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"..."}}
//
//   event: message_delta
//   data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":N}}
//
//   event: message_stop
//   data: {"type":"message_stop"}
//
// OpenAI SSE format emitted:
//   data: {"id":"...","object":"chat.completion.chunk","choices":[{"delta":{"content":"..."},...}]}
type anthropicSSEStream struct {
	reader    *bufio.Reader
	closer    io.Closer
	model     string
	messageID string
	done      bool
}

func (s *anthropicSSEStream) ReadLine() (string, error) {
	if s.done {
		return "data: [DONE]", nil
	}
	for {
		line, err := s.reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")

		if err != nil && err != io.EOF {
			return "", err
		}
		if err == io.EOF && line == "" {
			return "", io.EOF
		}

		// Skip empty lines and "event:" lines (we use "data:" only).
		if line == "" || strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		payload := strings.TrimPrefix(line, "data:")
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" || payload == "" {
			s.done = true
			return "data: [DONE]", nil
		}

		var raw map[string]json.RawMessage
		if jsonErr := json.Unmarshal([]byte(payload), &raw); jsonErr != nil {
			continue // skip malformed events
		}

		eventType := strings.Trim(string(raw["type"]), `"`)

		switch eventType {
		case "message_start":
			// Extract message ID for subsequent chunks.
			var ms struct {
				Message struct {
					ID string `json:"id"`
				} `json:"message"`
			}
			if jsonErr := json.Unmarshal([]byte(payload), &ms); jsonErr == nil {
				s.messageID = ms.Message.ID
			}
			continue

		case "content_block_delta":
			var cbd struct {
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			if jsonErr := json.Unmarshal([]byte(payload), &cbd); jsonErr != nil || cbd.Delta.Type != "text_delta" {
				continue
			}
			chunk := buildOAIStreamChunk(s.messageID, s.model, cbd.Delta.Text, nil)
			out, _ := json.Marshal(chunk)
			return "data: " + string(out), nil

		case "message_delta":
			var md struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if jsonErr := json.Unmarshal([]byte(payload), &md); jsonErr != nil {
				continue
			}
			finishReason := "stop"
			if md.Delta.StopReason == "max_tokens" {
				finishReason = "length"
			}
			chunk := buildOAIStreamChunk(s.messageID, s.model, "", &finishReason)
			out, _ := json.Marshal(chunk)
			return "data: " + string(out), nil

		case "message_stop":
			s.done = true
			return "data: [DONE]", nil
		}
	}
}

func (s *anthropicSSEStream) Close() error { return s.closer.Close() }

// buildOAIStreamChunk constructs a minimal OpenAI-compatible SSE chunk.
func buildOAIStreamChunk(id, model, content string, finishReason *string) map[string]interface{} {
	delta := map[string]interface{}{"content": content}
	if content == "" && finishReason != nil {
		delta = map[string]interface{}{"content": nil}
	}
	choice := map[string]interface{}{
		"index":         0,
		"delta":         delta,
		"finish_reason": finishReason,
	}
	return map[string]interface{}{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []interface{}{choice},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// geminiProviderBackend — Google Gemini API
// Default base URL: https://generativelanguage.googleapis.com
// Wire format: Gemini's OpenAI-compat endpoint at /v1beta/openai/
// Health check: GET /v1beta/openai/models with Bearer token.
// ─────────────────────────────────────────────────────────────────────────────

type geminiProviderBackend struct {
	providerNoopLifecycle
}

// NewGeminiProviderBackend constructs the Google Gemini provider backend.
func NewGeminiProviderBackend(_ *http.Client) Backend {
	return &geminiProviderBackend{}
}

func (b *geminiProviderBackend) Type() BackendType { return BackendGemini }

// Health checks Gemini's OpenAI-compat /v1beta/openai/models endpoint.
func (b *geminiProviderBackend) Health(ctx context.Context, url string, client *http.Client) EndpointHealth {
	return providerHealthCheck(ctx, client, url, "/v1beta/openai/models", "", BackendGemini)
}

func (b *geminiProviderBackend) Models(ctx context.Context, url string, client *http.Client) ([]BackendModel, error) {
	return openAICompatModels(ctx, client, url, "")
}

// Chat routes through Gemini's OpenAI-compat endpoint.
// The base URL stored in the endpoint row must be:
//   https://generativelanguage.googleapis.com
// Gemini's compat path is /v1beta/openai/chat/completions.
func (b *geminiProviderBackend) Chat(ctx context.Context, r ChatRequest) (*BackendResponse, error) {
	// Rewrite the endpoint URL to use Gemini's compat path prefix.
	r2 := r
	r2.EndpointURL = r.EndpointURL + "/v1beta/openai"
	// Delegate to the shared OpenAI-compat chat helper (appends /chat/completions).
	return openAICompatChatWithBase(ctx, r.Client, r2, "/chat/completions")
}

func (b *geminiProviderBackend) Embeddings(ctx context.Context, r EmbedRequest) (*models.EmbeddingResponse, error) {
	r2 := r
	r2.EndpointURL = r.EndpointURL + "/v1beta/openai"
	return openAICompatEmbeddings(ctx, r.Client, r2)
}

// ─────────────────────────────────────────────────────────────────────────────
// azureOpenAIProviderBackend — Azure OpenAI Service
// Default base URL: https://<resource>.openai.azure.com
// Wire format: Azure OpenAI REST API.
// Key difference: requests go to /openai/deployments/<model>/chat/completions
// with ?api-version=<version> query param and api-key header.
// ─────────────────────────────────────────────────────────────────────────────

type azureOpenAIProviderBackend struct {
	providerNoopLifecycle
}

// NewAzureOpenAIProviderBackend constructs the Azure OpenAI provider backend.
func NewAzureOpenAIProviderBackend(_ *http.Client) Backend {
	return &azureOpenAIProviderBackend{}
}

func (b *azureOpenAIProviderBackend) Type() BackendType { return BackendAzureOpenAI }

func (b *azureOpenAIProviderBackend) Health(ctx context.Context, url string, client *http.Client) EndpointHealth {
	// Azure: probe /openai/deployments?api-version=2024-02-01
	return providerHealthCheck(ctx, client, url,
		"/openai/deployments?api-version=2024-02-01", "", BackendAzureOpenAI)
}

func (b *azureOpenAIProviderBackend) Models(ctx context.Context, url string, client *http.Client) ([]BackendModel, error) {
	return []BackendModel{{ID: "azure-deployment", OwnedBy: "azure"}}, nil
}

// Chat routes to the Azure deployment-based chat completions endpoint.
// The UpstreamModelName field holds the Azure deployment name.
// The UpstreamAPIKey is sent as api-key header (Azure convention).
func (b *azureOpenAIProviderBackend) Chat(ctx context.Context, r ChatRequest) (*BackendResponse, error) {
	// Azure API version — can be overridden via provider_api_version in DB.
	apiVersion := "2024-08-01-preview"

	// Deployment name = the model's upstream_model_name (set at registration).
	// Falls back to the model name in the request.
	deploymentName := r.Req.Model

	body, err := json.Marshal(r.Req)
	if err != nil {
		return nil, err
	}

	azureURL := fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s",
		r.EndpointURL, deploymentName, apiVersion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, azureURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.UpstreamAPIKey != "" {
		req.Header.Set("api-key", r.UpstreamAPIKey)
	}
	if r.Req.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("azure openai chat: %w", err)
	}

	br := &BackendResponse{StatusCode: resp.StatusCode, Headers: map[string]string{}}
	if r.Req.Stream {
		br.Stream = &sseStream{reader: bufio.NewReader(resp.Body), closer: resp.Body}
	} else {
		br.Body, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	return br, err
}

func (b *azureOpenAIProviderBackend) Embeddings(ctx context.Context, r EmbedRequest) (*models.EmbeddingResponse, error) {
	deploymentName := r.Req.Model
	apiVersion := "2024-08-01-preview"
	azureURL := fmt.Sprintf("%s/openai/deployments/%s/embeddings?api-version=%s",
		r.EndpointURL, deploymentName, apiVersion)

	body, err := json.Marshal(r.Req)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, azureURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.UpstreamAPIKey != "" {
		req.Header.Set("api-key", r.UpstreamAPIKey)
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out models.EmbeddingResponse
	if jsonErr := json.NewDecoder(resp.Body).Decode(&out); jsonErr != nil {
		return nil, jsonErr
	}
	return &out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// OpenAI-compatible provider backends
// (OpenRouter, Groq, Together, Mistral, Cohere, DeepSeek)
//
// All of these expose an OpenAI-compatible API at /v1/chat/completions.
// They differ only in their default base URL and health-check endpoint.
// The implementation is a tiny wrapper around the shared helpers.
// ─────────────────────────────────────────────────────────────────────────────

// openRouterProviderBackend — OpenRouter (openrouter.ai)
type openRouterProviderBackend struct {
	providerNoopLifecycle
}

func NewOpenRouterProviderBackend(_ *http.Client) Backend {
	return &openRouterProviderBackend{}
}
func (b *openRouterProviderBackend) Type() BackendType { return BackendOpenRouter }
func (b *openRouterProviderBackend) Health(ctx context.Context, url string, client *http.Client) EndpointHealth {
	return providerHealthCheck(ctx, client, url, "/api/v1/models", "", BackendOpenRouter)
}
func (b *openRouterProviderBackend) Models(ctx context.Context, url string, client *http.Client) ([]BackendModel, error) {
	return openAICompatModels(ctx, client, url+"/api/v1", "")
}
func (b *openRouterProviderBackend) Chat(ctx context.Context, r ChatRequest) (*BackendResponse, error) {
	r2 := r
	r2.EndpointURL = strings.TrimSuffix(r.EndpointURL, "/v1")
	return openAICompatChatWithBase(ctx, r.Client, r2, "/api/v1/chat/completions")
}
func (b *openRouterProviderBackend) Embeddings(ctx context.Context, r EmbedRequest) (*models.EmbeddingResponse, error) {
	return openAICompatEmbeddings(ctx, r.Client, r)
}

// groqProviderBackend — Groq (api.groq.com)
type groqProviderBackend struct {
	providerNoopLifecycle
}

func NewGroqProviderBackend(_ *http.Client) Backend {
	return &groqProviderBackend{}
}
func (b *groqProviderBackend) Type() BackendType { return BackendGroq }
func (b *groqProviderBackend) Health(ctx context.Context, url string, client *http.Client) EndpointHealth {
	return providerHealthCheck(ctx, client, url, "/openai/v1/models", "", BackendGroq)
}
func (b *groqProviderBackend) Models(ctx context.Context, url string, client *http.Client) ([]BackendModel, error) {
	return openAICompatModels(ctx, client, url, "")
}
func (b *groqProviderBackend) Chat(ctx context.Context, r ChatRequest) (*BackendResponse, error) {
	return openAICompatChat(ctx, r.Client, r)
}
func (b *groqProviderBackend) Embeddings(ctx context.Context, r EmbedRequest) (*models.EmbeddingResponse, error) {
	return openAICompatEmbeddings(ctx, r.Client, r)
}

// togetherProviderBackend — Together AI (api.together.xyz)
type togetherProviderBackend struct {
	providerNoopLifecycle
}

func NewTogetherProviderBackend(_ *http.Client) Backend {
	return &togetherProviderBackend{}
}
func (b *togetherProviderBackend) Type() BackendType { return BackendTogether }
func (b *togetherProviderBackend) Health(ctx context.Context, url string, client *http.Client) EndpointHealth {
	return providerHealthCheck(ctx, client, url, "/v1/models", "", BackendTogether)
}
func (b *togetherProviderBackend) Models(ctx context.Context, url string, client *http.Client) ([]BackendModel, error) {
	return openAICompatModels(ctx, client, url, "")
}
func (b *togetherProviderBackend) Chat(ctx context.Context, r ChatRequest) (*BackendResponse, error) {
	return openAICompatChat(ctx, r.Client, r)
}
func (b *togetherProviderBackend) Embeddings(ctx context.Context, r EmbedRequest) (*models.EmbeddingResponse, error) {
	return openAICompatEmbeddings(ctx, r.Client, r)
}

// mistralProviderBackend — Mistral AI (api.mistral.ai)
type mistralProviderBackend struct {
	providerNoopLifecycle
}

func NewMistralProviderBackend(_ *http.Client) Backend {
	return &mistralProviderBackend{}
}
func (b *mistralProviderBackend) Type() BackendType { return BackendMistral }
func (b *mistralProviderBackend) Health(ctx context.Context, url string, client *http.Client) EndpointHealth {
	return providerHealthCheck(ctx, client, url, "/v1/models", "", BackendMistral)
}
func (b *mistralProviderBackend) Models(ctx context.Context, url string, client *http.Client) ([]BackendModel, error) {
	return openAICompatModels(ctx, client, url, "")
}
func (b *mistralProviderBackend) Chat(ctx context.Context, r ChatRequest) (*BackendResponse, error) {
	return openAICompatChat(ctx, r.Client, r)
}
func (b *mistralProviderBackend) Embeddings(ctx context.Context, r EmbedRequest) (*models.EmbeddingResponse, error) {
	return openAICompatEmbeddings(ctx, r.Client, r)
}

// cohereProviderBackend — Cohere (api.cohere.com)
type cohereProviderBackend struct {
	providerNoopLifecycle
}

func NewCohereProviderBackend(_ *http.Client) Backend {
	return &cohereProviderBackend{}
}
func (b *cohereProviderBackend) Type() BackendType { return BackendCohere }
func (b *cohereProviderBackend) Health(ctx context.Context, url string, client *http.Client) EndpointHealth {
	return providerHealthCheck(ctx, client, url, "/v1/models", "", BackendCohere)
}
func (b *cohereProviderBackend) Models(ctx context.Context, url string, client *http.Client) ([]BackendModel, error) {
	return openAICompatModels(ctx, client, url, "")
}
func (b *cohereProviderBackend) Chat(ctx context.Context, r ChatRequest) (*BackendResponse, error) {
	return openAICompatChat(ctx, r.Client, r)
}
func (b *cohereProviderBackend) Embeddings(ctx context.Context, r EmbedRequest) (*models.EmbeddingResponse, error) {
	return openAICompatEmbeddings(ctx, r.Client, r)
}

// deepSeekProviderBackend — DeepSeek (api.deepseek.com)
type deepSeekProviderBackend struct {
	providerNoopLifecycle
}

func NewDeepSeekProviderBackend(_ *http.Client) Backend {
	return &deepSeekProviderBackend{}
}
func (b *deepSeekProviderBackend) Type() BackendType { return BackendDeepSeek }
func (b *deepSeekProviderBackend) Health(ctx context.Context, url string, client *http.Client) EndpointHealth {
	return providerHealthCheck(ctx, client, url, "/v1/models", "", BackendDeepSeek)
}
func (b *deepSeekProviderBackend) Models(ctx context.Context, url string, client *http.Client) ([]BackendModel, error) {
	return openAICompatModels(ctx, client, url, "")
}
func (b *deepSeekProviderBackend) Chat(ctx context.Context, r ChatRequest) (*BackendResponse, error) {
	return openAICompatChat(ctx, r.Client, r)
}
func (b *deepSeekProviderBackend) Embeddings(ctx context.Context, r EmbedRequest) (*models.EmbeddingResponse, error) {
	return openAICompatEmbeddings(ctx, r.Client, r)
}

// ─────────────────────────────────────────────────────────────────────────────
// Shared provider helpers
// ─────────────────────────────────────────────────────────────────────────────

// providerHealthCheck performs a GET request to url+path and returns an
// EndpointHealth. Treats 200/401 as healthy (401 = reachable but no key),
// 4xx as degraded, 5xx as down.
func providerHealthCheck(
	ctx context.Context,
	client *http.Client,
	url, path, apiKey string,
	backend BackendType,
) EndpointHealth {
	h := EndpointHealth{URL: url, Status: StatusDown, CheckedAt: time.Now()}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+path, nil)
	if err != nil {
		h.Error = err.Error()
		return h
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	h.LatencyMs = int(time.Since(start).Milliseconds())
	if err != nil {
		h.Error = err.Error()
		return h
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == 200 || resp.StatusCode == 401:
		// 401 = API reachable but key invalid / absent — treated as healthy
		// because key correctness is validated per-request, not during health probes.
		h.Status = StatusHealthy
	case resp.StatusCode < 500:
		h.Status = StatusDegraded
		h.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	default:
		h.Status = StatusDown
		h.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return h
}

// openAICompatChat sends a chat completions request to baseURL+"/v1/chat/completions".
func openAICompatChat(ctx context.Context, client *http.Client, r ChatRequest) (*BackendResponse, error) {
	return openAICompatChatWithBase(ctx, client, r, "/v1/chat/completions")
}

// openAICompatChatWithBase sends to baseURL+path (allows Gemini /v1beta/openai/chat/completions etc.)
func openAICompatChatWithBase(ctx context.Context, client *http.Client, r ChatRequest, path string) (*BackendResponse, error) {
	body, err := json.Marshal(r.Req)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.EndpointURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.UpstreamAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.UpstreamAPIKey)
	}
	if r.Req.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("provider chat (%s): %w", path, err)
	}
	br := &BackendResponse{StatusCode: resp.StatusCode, Headers: map[string]string{}}
	if r.Req.Stream {
		br.Stream = &sseStream{reader: bufio.NewReader(resp.Body), closer: resp.Body}
	} else {
		br.Body, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	return br, err
}

// openAICompatEmbeddings posts to baseURL+"/v1/embeddings".
func openAICompatEmbeddings(ctx context.Context, client *http.Client, r EmbedRequest) (*models.EmbeddingResponse, error) {
	body, err := json.Marshal(r.Req)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.EndpointURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.UpstreamAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.UpstreamAPIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out models.EmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// openAICompatModels fetches /v1/models from a provider.
func openAICompatModels(ctx context.Context, client *http.Client, baseURL, apiKey string) ([]BackendModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var list models.ModelListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	out := make([]BackendModel, len(list.Data))
	for i, m := range list.Data {
		out[i] = BackendModel{ID: m.ID, Object: m.Object, Created: m.Created, OwnedBy: m.OwnedBy}
	}
	return out, nil
}
