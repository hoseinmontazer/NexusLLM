package runtime

// Regression tests for a production bug: every backend adapter's Embeddings()
// decoded the HTTP response body as a models.EmbeddingResponse regardless of
// status code. An error body (or an empty one) decodes without error into a
// zero-value response — {"object":"","data":null,"usage":{"prompt_tokens":0,
// "total_tokens":0}} — which silently looks like a successful-but-empty
// embeddings call instead of the real upstream failure. Observed in
// production as an intermittent {"data":null} response from a model whose
// runtime was mid-restart. These tests prove a non-2xx status is now always
// surfaced as a Go error instead of a fake-success empty response.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nexusllm/nexusllm/internal/models"
)

func embedReq() EmbedRequest {
	return EmbedRequest{
		Req: &models.EmbeddingRequest{Model: "test-model", Input: "hello world"},
	}
}

// TestEmbeddings_NonOKStatus_ReturnsError proves every backend adapter
// returns a real error — not a nil-error zero-value response — when the
// upstream returns a non-2xx status, even with a JSON-parseable body.
func TestEmbeddings_NonOKStatus_ReturnsError(t *testing.T) {
	backends := map[string]func(*http.Client) Backend{
		"openai_compat": NewOpenAICompatBackend,
		"cpu_native":    NewCPUNativeBackend,
		"tgi":           NewTGIBackend,
		"vllm":          NewVLLMBackend,
	}

	statusBodies := []struct {
		name   string
		status int
		body   string
	}{
		{"503 with error body", http.StatusServiceUnavailable, `{"error":"model is loading"}`},
		{"500 with empty object", http.StatusInternalServerError, `{}`},
		{"502 with empty body", http.StatusBadGateway, ``},
	}

	for backendName, ctor := range backends {
		for _, sb := range statusBodies {
			t.Run(backendName+"/"+sb.name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(sb.status)
					_, _ = w.Write([]byte(sb.body))
				}))
				defer srv.Close()

				b := ctor(srv.Client())
				req := embedReq()
				req.EndpointURL = srv.URL
				req.Client = srv.Client()

				resp, err := b.Embeddings(context.Background(), req)
				if err == nil {
					t.Fatalf("%s: expected an error for HTTP %d, got nil error and response %+v — this is exactly the production bug (silent fake-success empty response)", backendName, sb.status, resp)
				}
				if resp != nil {
					t.Fatalf("%s: expected nil response alongside the error, got %+v", backendName, resp)
				}
			})
		}
	}
}

// TestEmbeddings_OKStatus_StillDecodesCorrectly is the non-regression check:
// a genuine 200 response with real embedding data must still decode and
// return successfully for every backend.
func TestEmbeddings_OKStatus_StillDecodesCorrectly(t *testing.T) {
	backends := map[string]func(*http.Client) Backend{
		"openai_compat": NewOpenAICompatBackend,
		"cpu_native":    NewCPUNativeBackend,
		"tgi":           NewTGIBackend,
		"vllm":          NewVLLMBackend,
	}

	const validBody = `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]}],"model":"test-model","usage":{"prompt_tokens":2,"total_tokens":2}}`

	for backendName, ctor := range backends {
		t.Run(backendName, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(validBody))
			}))
			defer srv.Close()

			b := ctor(srv.Client())
			req := embedReq()
			req.EndpointURL = srv.URL
			req.Client = srv.Client()

			resp, err := b.Embeddings(context.Background(), req)
			if err != nil {
				t.Fatalf("%s: unexpected error for a valid 200 response: %v", backendName, err)
			}
			if resp == nil || len(resp.Data) != 1 || resp.Usage.TotalTokens != 2 {
				t.Fatalf("%s: expected a decoded response with 1 embedding and usage.total_tokens=2, got %+v", backendName, resp)
			}
		})
	}
}

// TestCPUNativeEmbeddings_404FallsThroughToSecondPath proves cpu_native's
// dual-path lookup (/v1/embeddings then /embeddings) still works correctly
// alongside the new status-code check — 404 must still trigger a retry of
// the second path, not be treated as a hard error.
func TestCPUNativeEmbeddings_404FallsThroughToSecondPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/embeddings" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path == "/embeddings" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.5]}],"model":"test-model","usage":{"prompt_tokens":1,"total_tokens":1}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	b := NewCPUNativeBackend(srv.Client())
	req := embedReq()
	req.EndpointURL = srv.URL
	req.Client = srv.Client()

	resp, err := b.Embeddings(context.Background(), req)
	if err != nil {
		t.Fatalf("expected fallback to /embeddings to succeed, got error: %v", err)
	}
	if resp == nil || len(resp.Data) != 1 {
		t.Fatalf("expected a decoded response from the fallback path, got %+v", resp)
	}
}
