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

// tgiBackend implements Backend for Hugging Face Text Generation Inference.
// TGI exposes an OpenAI-compatible /v1 API since v1.4. The health endpoint
// is GET /health (returns 200 OK when ready).
type tgiBackend struct {
	client *http.Client
}

// NewTGIBackend constructs a TGI backend.
func NewTGIBackend(client *http.Client) Backend {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	return &tgiBackend{client: client}
}

func (b *tgiBackend) Type() BackendType { return BackendTGI }

func (b *tgiBackend) Health(ctx context.Context, url string) EndpointHealth {
	h := EndpointHealth{URL: url, Status: StatusDown, CheckedAt: time.Now()}
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/health", nil)
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

	switch resp.StatusCode {
	case http.StatusOK:
		h.Status = StatusHealthy
	case http.StatusServiceUnavailable:
		// TGI returns 503 while warming up
		h.Status = StatusDegraded
		h.Error = "warming up"
	default:
		h.Status = StatusDown
		h.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return h
}

func (b *tgiBackend) Models(ctx context.Context, url string) ([]BackendModel, error) {
	// TGI serves a single model; /v1/models returns it.
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

func (b *tgiBackend) Chat(ctx context.Context, r ChatRequest) (*BackendResponse, error) {
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
		return nil, fmt.Errorf("tgi chat: %w", err)
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

func (b *tgiBackend) Embeddings(ctx context.Context, r EmbedRequest) (*models.EmbeddingResponse, error) {
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
