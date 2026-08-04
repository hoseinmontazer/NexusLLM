package runtime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNormalizeProviderEndpointURL(t *testing.T) {
	tests := []struct {
		baseURL  string
		target   string
		expected string
	}{
		{
			baseURL:  "https://openrouter.ai/api/v1",
			target:   "/api/v1/chat/completions",
			expected: "https://openrouter.ai/api/v1/chat/completions",
		},
		{
			baseURL:  "https://openrouter.ai/api/v1/",
			target:   "/api/v1/models",
			expected: "https://openrouter.ai/api/v1/models",
		},
		{
			baseURL:  "https://generativelanguage.googleapis.com/v1beta/openai",
			target:   "/v1beta/openai/chat/completions",
			expected: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
		},
		{
			baseURL:  "https://api.openai.com/v1",
			target:   "/v1/models",
			expected: "https://api.openai.com/v1/models",
		},
		{
			baseURL:  "https://api.openai.com",
			target:   "/v1/chat/completions",
			expected: "https://api.openai.com/v1/chat/completions",
		},
	}

	for _, tc := range tests {
		got := NormalizeProviderEndpointURL(tc.baseURL, tc.target)
		if got != tc.expected {
			t.Errorf("NormalizeProviderEndpointURL(%q, %q) = %q; want %q", tc.baseURL, tc.target, got, tc.expected)
		}
	}
}

func TestReadTimeoutTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello world"))
	}))
	defer server.Close()

	rt := &readTimeoutTransport{
		wrapped:     http.DefaultTransport,
		readTimeout: 5 * time.Second,
	}

	client := &http.Client{Transport: rt}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext failed: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll failed: %v (context was likely prematurely cancelled)", err)
	}

	if string(body) != "hello world" {
		t.Errorf("expected body %q, got %q", "hello world", string(body))
	}
}
