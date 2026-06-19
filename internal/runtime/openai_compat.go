package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nexusllm/nexusllm/internal/models"
)

// openAICompatBackend implements Backend for any OpenAI-compatible API
// (actual OpenAI, Azure OpenAI, Together, Groq, Anyscale, etc.).
// The API key is passed via X-API-Key header or Bearer token in extra_args.
type openAICompatBackend struct {
	client *http.Client
}

// NewOpenAICompatBackend constructs an OpenAI-compatible backend.
func NewOpenAICompatBackend(client *http.Client) Backend {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	return &openAICompatBackend{client: client}
}

func (b *openAICompatBackend) Type() BackendType { return BackendOpenAICompat }

// Health performs a lightweight GET /v1/models to verify the endpoint is alive.
func (b *openAICompatBackend) Health(ctx context.Context, url string) EndpointHealth {
	h := EndpointHealth{URL: url, Status: StatusDown, CheckedAt: time.Now()}
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/models", nil)
	if err != nil {
		h.Error = err.Error()
		return h
	}
	resp, err := b.client.Do(req)
	h.LatencyMs = int(time.Since(start).Milliseconds())
	if err != nil {
		h.Error = err.Error()
		return h
	}
	resp.Body.Close()

	if resp.StatusCode < 500 {
		h.Status = StatusHealthy
	} else {
		h.Status = StatusDegraded
		h.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return h
}

func (b *openAICompatBackend) Models(ctx context.Context, url string) ([]BackendModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(req)
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

func (b *openAICompatBackend) Chat(ctx context.Context, r ChatRequest) (*BackendResponse, error) {
	body, err := json.Marshal(r.Req)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.EndpointURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.Req.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai compat chat: %w", err)
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

func (b *openAICompatBackend) Embeddings(ctx context.Context, r EmbedRequest) (*models.EmbeddingResponse, error) {
	body, err := json.Marshal(r.Req)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.EndpointURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
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
