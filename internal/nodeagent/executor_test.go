package nodeagent

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"
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

func hasFlagValue(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// TestBuildDockerArgs_VLLMExistingLocalPath is the regression test for the
// gpt-oss-120b-vllm deployment gap: a vLLM deployment pointed at a model
// directory that already exists in the shared volume (GGUFPath set, exactly
// as an admin would configure via "llamacpp_model_path" alongside
// "backend_type": "vllm" — the field name is a historical artifact, the
// plumbing is backend-agnostic) must (a) mount that volume at /models, and
// (b) pass the local path — not ModelName/hf_model_id — as vLLM's --model
// argument, so vLLM never attempts to download anything.
func TestBuildDockerArgs_VLLMExistingLocalPath(t *testing.T) {
	e := &Executor{log: zap.NewNop()}
	p := startModelPayload{
		RuntimeName:    "nexus-gpt-oss-120b-r0-abc123",
		Backend:        "vllm",
		Image:          "vllm/vllm-openai:latest",
		ModelName:      "gpt-oss-120b", // e.g. input.Name fallback — must NOT be used as --model
		ServedAs:       "gpt-oss-120b",
		BindPort:       8200,
		GGUFPath:       "/models/gpt-oss-120b-vllm", // resolved by resolveModelsVolumeMount's caller
		ModelsVolume:   "nexus_models",              // resolved volume name
		TensorParallel: 2,
		GPUMemoryUtil:  0.90,
		ExecutionMode:  "gpu",
		GPUDevices:     []int{0, 1},
	}

	args := e.buildDockerArgs(p)

	if !hasFlagValue(args, "-v", "nexus_models:/models") {
		t.Fatalf("expected the resolved models volume to be mounted at /models, got args: %v", args)
	}
	if !hasFlagValue(args, "--model", "/models/gpt-oss-120b-vllm") {
		t.Fatalf("expected --model to use the existing local path (GGUFPath), not ModelName, got args: %v", args)
	}
	if hasFlagValue(args, "--model", "gpt-oss-120b") {
		t.Fatalf("--model must not be set to ModelName when GGUFPath (a local path) is present, got args: %v", args)
	}
	if !hasFlagValue(args, "--served-model-name", "gpt-oss-120b") {
		t.Fatalf("expected --served-model-name to be set, got args: %v", args)
	}
}

// TestBuildDockerArgs_VLLMWithoutLocalPath is the non-regression check: a
// normal vLLM deployment sourced from an HF repo id (no GGUFPath/ModelsVolume
// set) must NOT get a /models mount at all, and --model must be ModelName
// (the HF repo id vLLM will download itself).
func TestBuildDockerArgs_VLLMWithoutLocalPath(t *testing.T) {
	e := &Executor{log: zap.NewNop()}
	p := startModelPayload{
		RuntimeName: "nexus-llama3-8b-r0-def456",
		Backend:     "vllm",
		Image:       "vllm/vllm-openai:v0.4.3",
		ModelName:   "meta-llama/Meta-Llama-3-8B-Instruct",
		ServedAs:    "llama3-8b",
		BindPort:    8100,
	}

	args := e.buildDockerArgs(p)

	if hasFlag(args, "-v") {
		t.Fatalf("expected no volume mount for a plain HF-repo vLLM deploy, got args: %v", args)
	}
	if !hasFlagValue(args, "--model", "meta-llama/Meta-Llama-3-8B-Instruct") {
		t.Fatalf("expected --model to be the HF repo id, got args: %v", args)
	}
}

// TestBuildDockerArgs_TEIExistingLocalPath proves the same volume-mount fix
// also closes the identical latent bug on the "tei" backend, which already
// correctly preferred GGUFPath for --model-id but had nothing mounting it.
func TestBuildDockerArgs_TEIExistingLocalPath(t *testing.T) {
	e := &Executor{log: zap.NewNop()}
	p := startModelPayload{
		RuntimeName:  "nexus-embed-r0-ghi789",
		Backend:      "tei",
		Image:        "ghcr.io/huggingface/text-embeddings-inference:latest",
		ModelName:    "multilingual-e5-large",
		GGUFPath:     "/models/multilingual-e5-large-onnx",
		ModelsVolume: "nexus_models",
		BindPort:     8300,
	}

	args := e.buildDockerArgs(p)

	if !hasFlagValue(args, "-v", "nexus_models:/models") {
		t.Fatalf("expected the resolved models volume to be mounted at /models for tei, got args: %v", args)
	}
	if !hasFlagValue(args, "--model-id", "/models/multilingual-e5-large-onnx") {
		t.Fatalf("expected --model-id to use the local path, got args: %v", args)
	}
}

// TestBuildDockerArgs_VLLMGPU_SetsShmSize is the regression test for the
// gpt-oss-120b-vllm multi-GPU startup crash: "RuntimeError: Insufficient
// space in /dev/shm: 160 MiB required, 64 MiB free". vLLM's tensor-parallel
// worker subprocesses communicate over POSIX shared memory, and Docker's
// default 64MB /dev/shm is nowhere near enough — vLLM's own docs recommend
// enlarging it (or --ipc=host) for GPU deployments.
func TestBuildDockerArgs_VLLMGPU_SetsShmSize(t *testing.T) {
	e := &Executor{log: zap.NewNop()}
	p := startModelPayload{
		RuntimeName:    "nexus-gpt-oss-120b-r0-abc123",
		Backend:        "vllm",
		Image:          "vllm/vllm-openai:latest",
		BindPort:       8200,
		TensorParallel: 2,
		ExecutionMode:  "gpu",
		GPUDevices:     []int{0, 1},
	}

	args := e.buildDockerArgs(p)

	if !hasFlagValue(args, "--shm-size", "4g") {
		t.Fatalf("expected --shm-size to be set for a GPU vLLM deploy, got args: %v", args)
	}
}

// TestBuildDockerArgs_VLLMCPU_NoShmSize proves the --shm-size override is
// scoped to GPU deployments only — a CPU-mode vLLM deploy (execution_mode
// explicitly "cpu") should not get it.
func TestBuildDockerArgs_VLLMCPU_NoShmSize(t *testing.T) {
	e := &Executor{log: zap.NewNop()}
	p := startModelPayload{
		RuntimeName:   "nexus-vllm-cpu-r0-abc123",
		Backend:       "vllm",
		Image:         "vllm/vllm-openai:latest",
		BindPort:      8200,
		ExecutionMode: "cpu",
	}

	args := e.buildDockerArgs(p)

	if hasFlag(args, "--shm-size") {
		t.Fatalf("expected no --shm-size for a CPU-mode vLLM deploy, got args: %v", args)
	}
}

// TestBuildDockerArgs_LlamaCppGPU_NoShmSize proves the --shm-size override is
// scoped to vllm/tgi — llamacpp does not use multiprocess tensor-parallel
// workers the same way and has never hit this /dev/shm limit in practice.
func TestBuildDockerArgs_LlamaCppGPU_NoShmSize(t *testing.T) {
	e := &Executor{log: zap.NewNop()}
	p := startModelPayload{
		RuntimeName:   "nexus-llamacpp-r0-abc123",
		Backend:       "llamacpp",
		Image:         "ghcr.io/ggerganov/llama.cpp:server",
		BindPort:      8300,
		ExecutionMode: "gpu",
		GPUDevices:    []int{0},
	}

	args := e.buildDockerArgs(p)

	if hasFlag(args, "--shm-size") {
		t.Fatalf("expected no --shm-size for llamacpp, got args: %v", args)
	}
}

// TestResolveModelsVolumeMount_NoLocalPathReturnsEmpty proves the resolver
// correctly no-ops for a plain HF-repo deployment (nothing to mount).
func TestResolveModelsVolumeMount_NoLocalPathReturnsEmpty(t *testing.T) {
	e := &Executor{log: zap.NewNop()}
	p := &startModelPayload{Backend: "vllm", ModelName: "org/repo"}
	if got := e.resolveModelsVolumeMount(context.Background(), p); got != "" {
		t.Fatalf("expected empty volume for a deployment with no GGUFPath/ModelsVolume, got %q", got)
	}
}

// TestResolveModelsVolumeMount_AbsoluteVolumeUsedDirectly proves an absolute
// host path passed as ModelsVolume (rather than a named Docker volume) is
// used as-is, without attempting `docker volume inspect`.
func TestResolveModelsVolumeMount_AbsoluteVolumeUsedDirectly(t *testing.T) {
	e := &Executor{log: zap.NewNop()}
	p := &startModelPayload{
		Backend:      "vllm",
		GGUFPath:     "/models/gpt-oss-120b-vllm",
		ModelsVolume: "/var/lib/docker/volumes/nexus_models/_data",
	}
	got := e.resolveModelsVolumeMount(context.Background(), p)
	if got != "/var/lib/docker/volumes/nexus_models/_data" {
		t.Fatalf("expected the absolute path to be used directly, got %q", got)
	}
	if strings.HasPrefix(got, "docker") {
		t.Fatalf("resolver should not have attempted docker volume inspect for an absolute path, got %q", got)
	}
}
