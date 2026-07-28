package runtime

// teiBackend implements Backend for Hugging Face Text Embeddings Inference.
// https://github.com/huggingface/text-embeddings-inference
//
// TEI exposes an OpenAI-compatible /v1/embeddings endpoint and a /health
// endpoint that returns:
//   - 200 OK   when the model is fully loaded and ready
//   - 503      while the model is still loading
//
// TEI does NOT support chat completions — Chat() returns an error. All
// embedding requests are routed via Embeddings().
//
// Port: passed via --port CMD arg (same as TGI/llamacpp). No env var needed.
// Volume: the shared models volume is mounted at /data inside the container.
//   TEI uses /data as its Hugging Face hub cache (HUGGINGFACE_HUB_CACHE=/data).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/nexusllm/nexusllm/internal/models"
)

// BackendTEI identifies the Text Embeddings Inference backend.
const BackendTEI BackendType = "tei"

// teiBackend implements the Backend interface for TEI.
type teiBackend struct {
	client *http.Client
}

// NewTEIBackend constructs a TEI backend.
func NewTEIBackend(client *http.Client) Backend {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &teiBackend{client: client}
}

func (b *teiBackend) Type() BackendType { return BackendTEI }

// ContainerPort returns 0 — TEI's listen port is set via the --port CMD arg,
// not via an environment variable.
func (b *teiBackend) ContainerPort() int { return 0 }

// ContainerPortEnvVars returns nil — TEI is configured via --port CMD arg.
func (b *teiBackend) ContainerPortEnvVars(_ int) map[string]string { return nil }

// PrepareStartupArgs — TEI has no server-level reasoning flag; args unchanged.
func (b *teiBackend) PrepareStartupArgs(caps ModelStartupCaps, extraArgs []string) []string {
	return extraArgs
}

// Health probes GET /health.
// TEI returns 200 when ready and 503 while still loading the model weights.
func (b *teiBackend) Health(ctx context.Context, url string) EndpointHealth {
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
		// TEI returns 503 while the model is loading — not a fatal error.
		h.Status = StatusDegraded
		h.Error = "model loading"
	default:
		h.Status = StatusDown
		h.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return h
}

// Models returns a synthetic model list using the TEI /info endpoint.
// TEI doesn't expose /v1/models, but /info returns the model_id.
func (b *teiBackend) Models(ctx context.Context, url string) ([]BackendModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/info", nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var info struct {
		ModelID string `json:"model_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	if info.ModelID == "" {
		return nil, nil
	}
	return []BackendModel{{ID: info.ModelID, Object: "model", OwnedBy: "tei"}}, nil
}

// Chat is not supported by TEI — it is an embeddings-only server.
func (b *teiBackend) Chat(_ context.Context, _ ChatRequest) (*BackendResponse, error) {
	return nil, fmt.Errorf("tei backend does not support chat completions — use the /v1/embeddings endpoint")
}

// Embeddings sends an embedding request to TEI's /v1/embeddings endpoint.
func (b *teiBackend) Embeddings(ctx context.Context, r EmbedRequest) (*models.EmbeddingResponse, error) {
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

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tei embeddings: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tei embeddings: HTTP %d", resp.StatusCode)
	}

	var out models.EmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("tei embeddings decode: %w", err)
	}
	return &out, nil
}
