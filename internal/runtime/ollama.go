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

// ollamaBackend implements Backend for Ollama servers.
// Ollama exposes an OpenAI-compatible /v1 API since v0.1.24, so most
// paths are identical to vLLM. The health endpoint differs.
type ollamaBackend struct {
	client *http.Client
}

// NewOllamaBackend constructs an Ollama backend.
func NewOllamaBackend(client *http.Client) Backend {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	return &ollamaBackend{client: client}
}

func (b *ollamaBackend) Type() BackendType { return BackendOllama }

// Health checks GET / (Ollama returns "Ollama is running").
func (b *ollamaBackend) Health(ctx context.Context, url string) EndpointHealth {
	h := EndpointHealth{URL: url, Status: StatusDown, CheckedAt: time.Now()}
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/", nil)
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
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "Ollama is running") {
		h.Status = StatusHealthy
	} else {
		h.Status = StatusDegraded
		h.Error = fmt.Sprintf("unexpected response: %s", string(body))
	}
	return h
}

func (b *ollamaBackend) Models(ctx context.Context, url string) ([]BackendModel, error) {
	// Ollama /api/tags returns the local model list.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s/api/tags: %w", url, err)
	}
	defer resp.Body.Close()

	var payload struct {
		Models []struct {
			Name       string    `json:"name"`
			ModifiedAt time.Time `json:"modified_at"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	out := make([]BackendModel, len(payload.Models))
	for i, m := range payload.Models {
		out[i] = BackendModel{
			ID:      m.Name,
			Object:  "model",
			Created: m.ModifiedAt.Unix(),
			OwnedBy: "ollama",
		}
	}
	return out, nil
}

func (b *ollamaBackend) Chat(ctx context.Context, r ChatRequest) (*BackendResponse, error) {
	// Ollama's /v1 OpenAI-compat layer is identical to vLLM's.
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
		return nil, fmt.Errorf("ollama chat: %w", err)
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

func (b *ollamaBackend) Embeddings(ctx context.Context, r EmbedRequest) (*models.EmbeddingResponse, error) {
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
