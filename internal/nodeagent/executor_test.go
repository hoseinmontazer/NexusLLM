package nodeagent

import (
	"testing"
)

func TestBackendPortEnvVars(t *testing.T) {
	tests := []struct {
		backend  string
		port     int
		expected map[string]string
	}{
		{
			backend: "cpu_native",
			port:    8000,
			expected: map[string]string{
				"PORT":         "8000",
				"HTTP_PORT":    "8000",
				"UVICORN_PORT": "8000",
			},
		},
		{
			backend: "openai_compat",
			port:    8080,
			expected: map[string]string{
				"PORT":         "8080",
				"HTTP_PORT":    "8080",
				"UVICORN_PORT": "8080",
			},
		},
		{
			backend:  "vllm",
			port:     8000,
			expected: nil,
		},
		{
			backend:  "llamacpp",
			port:     8090,
			expected: nil,
		},
	}

	for _, tc := range tests {
		got := backendPortEnvVars(tc.backend, tc.port)
		if len(got) != len(tc.expected) {
			t.Errorf("backendPortEnvVars(%q, %d) len = %d; want %d", tc.backend, tc.port, len(got), len(tc.expected))
			continue
		}
		for k, v := range tc.expected {
			if got[k] != v {
				t.Errorf("backendPortEnvVars(%q, %d)[%q] = %q; want %q", tc.backend, tc.port, k, got[k], v)
			}
		}
	}
}
