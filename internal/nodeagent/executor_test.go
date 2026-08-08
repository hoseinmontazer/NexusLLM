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

func TestSTTLazyRestartPortEnvOverride(t *testing.T) {
	// Simulate payload with stale operator hint UVICORN_PORT=8000
	payloadEnv := map[string]string{
		"WHISPER__MODEL": "Systran/faster-whisper-large-v3",
		"UVICORN_PORT":   "8000",
	}
	bindPort := 32781

	// Apply nodeagent backendPortEnvVars overwrite logic (executor.go:906-910)
	if bindPort > 0 {
		for k, v := range backendPortEnvVars("openai_compat", bindPort) {
			payloadEnv[k] = v
		}
	}

	if payloadEnv["UVICORN_PORT"] != "32781" {
		t.Errorf("expected UVICORN_PORT to be overwritten to 32781, got %s", payloadEnv["UVICORN_PORT"])
	}
	if payloadEnv["PORT"] != "32781" {
		t.Errorf("expected PORT to be 32781, got %s", payloadEnv["PORT"])
	}
	if payloadEnv["WHISPER__MODEL"] != "Systran/faster-whisper-large-v3" {
		t.Errorf("expected WHISPER__MODEL to be preserved, got %s", payloadEnv["WHISPER__MODEL"])
	}
}
