package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

// ContainerPort returns 0 — generic OpenAI-compatible servers have no single
// canonical default port.  The container port is driven entirely by
// ContainerPortEnvVars so the process binds to whatever host port was allocated.
func (b *openAICompatBackend) ContainerPort() int { return 0 }

// ContainerPortEnvVars injects the standard environment variables read by
// common OpenAI-compatible server frameworks to set their HTTP listen port.
//
// Variables injected (all set to the same value so any framework is covered):
//   - PORT       — generic convention used by many HTTP servers and PaaS runtimes
//   - HTTP_PORT  — used by some custom server wrappers
//
// Backend-specific variables (e.g. UVICORN_PORT for faster-whisper-server)
// are handled by their own dedicated backend adapters (cpu_native).
// Do NOT add UVICORN_PORT here — that would bleed cpu_native specifics into
// this generic adapter.
func (b *openAICompatBackend) ContainerPortEnvVars(port int) map[string]string {
	s := strconv.Itoa(port)
	return map[string]string{
		"PORT":      s,
		"HTTP_PORT": s,
	}
}

// PrepareStartupArgs — generic OpenAI-compat servers have no known
// server-level reasoning flag; args returned unchanged.
func (b *openAICompatBackend) PrepareStartupArgs(caps ModelStartupCaps, extraArgs []string) []string {
	return extraArgs
}

// clientFor returns r.Client when set, otherwise falls back to b.client.
// This lets the registry inject a per-endpoint client (e.g. proxy-aware)
// while keeping backward compat for callers that don't set r.Client.
func (b *openAICompatBackend) clientFor(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return b.client
}

// Health performs a lightweight GET /v1/models to verify the endpoint is alive.
func (b *openAICompatBackend) Health(ctx context.Context, url string, client *http.Client) EndpointHealth {
	h := EndpointHealth{URL: url, Status: StatusDown, CheckedAt: time.Now()}
	start := time.Now()
	c := b.clientFor(client)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/models", nil)
	if err != nil {
		h.Error = err.Error()
		return h
	}
	resp, err := c.Do(req)
	h.LatencyMs = int(time.Since(start).Milliseconds())
	if err != nil {
		h.Error = err.Error()
		return h
	}
	resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		h.Status = StatusHealthy
	} else if resp.StatusCode < 500 {
		h.Status = StatusDegraded
		h.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	} else {
		h.Status = StatusDown
		h.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return h
}

func (b *openAICompatBackend) Models(ctx context.Context, url string, client *http.Client) ([]BackendModel, error) {
	c := b.clientFor(client)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
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
	c := b.clientFor(r.Client)
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
	if r.UpstreamAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.UpstreamAPIKey)
	}
	if r.Req.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := c.Do(req)
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
	c := b.clientFor(r.Client)
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
	if r.UpstreamAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.UpstreamAPIKey)
	}
	resp, err := c.Do(req)
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
