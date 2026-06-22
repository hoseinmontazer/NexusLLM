package controller

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// dockerDriver runs model runtimes as plain Docker containers.
// It shells out to the docker CLI — no SDK dependency needed.
type dockerDriver struct{}

// NewDockerDriver constructs a Docker driver.
func NewDockerDriver() Driver { return &dockerDriver{} }

func (d *dockerDriver) Type() DriverType { return DriverDocker }

func (d *dockerDriver) Start(ctx context.Context, spec RuntimeSpec) (string, error) {
	var args []string

	switch spec.BackendType {
	case "ollama":
		args = d.buildOllamaArgs(spec)
	case "tgi":
		args = d.buildTGIArgs(spec)
	case "cpu_native":
		args = d.buildCPUNativeArgs(spec)
	case "llamacpp":
		args = d.buildLlamaCppArgs(spec)
	default: // vllm and openai_compat
		args = d.buildVLLMArgs(spec)
	}

	// Remove any existing container with the same name before starting.
	// This prevents "container name already in use" errors when redeploying.
	name := containerName(spec)
	rmOut, rmErr := exec.CommandContext(ctx, "docker", "rm", "-f", name).CombinedOutput()
	if rmErr != nil && !strings.Contains(string(rmOut), "No such container") {
		// Non-fatal — log and continue; docker run will surface the real error if needed.
		_ = rmOut
	}

	// Run and capture container ID
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		// Log the full command for debugging
		return "", fmt.Errorf("docker run failed: %w\ncommand: docker %s\noutput: %s",
			err, strings.Join(args, " "), string(out))
	}
	containerID := strings.TrimSpace(string(out))
	if containerID == "" {
		return "", fmt.Errorf("docker run returned empty container ID\noutput: %s", string(out))
	}
	return containerID, nil
}

// buildVLLMArgs builds the docker run arguments for a vLLM container.
func (d *dockerDriver) buildVLLMArgs(spec RuntimeSpec) []string {
	args := []string{"run", "-d",
		"--name", containerName(spec),
		"--restart", "unless-stopped",
		// Use host networking so the container is reachable on localhost:port
		// without needing port mapping when running on the same machine.
		"--network", "host",
	}

	// GPU devices — correct format for docker --gpus flag (no extra quotes)
	if len(spec.GPUDevices) > 0 {
		devList := make([]string, len(spec.GPUDevices))
		for i, idx := range spec.GPUDevices {
			devList[i] = strconv.Itoa(idx)
		}
		args = append(args, "--gpus", fmt.Sprintf("device=%s", strings.Join(devList, ",")))
	}

	args = d.applyCommonResourceArgs(args, spec)

	// Image
	args = append(args, spec.Image)

	// vLLM args — ModelName is the HF model ID (e.g. "google/gemma-3-27b-it")
	// ServedModelName is what clients call it (e.g. "gemma-3-27b")
	args = append(args, "--model", spec.ModelName)
	args = append(args, "--port", strconv.Itoa(spec.BindPort))

	if spec.ServedModelName != "" && spec.ServedModelName != spec.ModelName {
		args = append(args, "--served-model-name", spec.ServedModelName)
	}
	if spec.TensorParallel > 1 {
		args = append(args, "--tensor-parallel-size", strconv.Itoa(spec.TensorParallel))
	}
	if spec.GPUMemoryUtil > 0 {
		args = append(args, "--gpu-memory-utilization", fmt.Sprintf("%.2f", spec.GPUMemoryUtil))
	}
	if spec.MaxModelLen > 0 {
		args = append(args, "--max-model-len", strconv.Itoa(spec.MaxModelLen))
	}
	if spec.Dtype != "" && spec.Dtype != "auto" {
		args = append(args, "--dtype", spec.Dtype)
	}
	if spec.Quantization != "" {
		args = append(args, "--quantization", spec.Quantization)
	}

	args = append(args, spec.ExtraArgs...)
	return args
}

// buildCPUNativeArgs builds docker run args for CPU-native services
// (embeddings, rerankers, STT, TTS, OCR, MCP servers, agent runtimes).
// These containers get CPU affinity via --cpuset-cpus and NUMA via --cpuset-mems,
// but NO --gpus flag.
func (d *dockerDriver) buildCPUNativeArgs(spec RuntimeSpec) []string {
	args := []string{"run", "-d",
		"--name", containerName(spec),
		"--restart", "unless-stopped",
		"--network", "host",
	}

	// CPU affinity — pin to specific logical CPUs
	if spec.CPUSetCPUs != "" {
		args = append(args, "--cpuset-cpus", spec.CPUSetCPUs)
	}

	// NUMA memory affinity — pin memory allocation to the same NUMA node
	if spec.NUMANode >= 0 {
		args = append(args, "--cpuset-mems", strconv.Itoa(spec.NUMANode))
	}

	args = d.applyCommonResourceArgs(args, spec)

	args = append(args, spec.Image)

	// Pass port if the image uses a configurable port via env
	// Most CPU-native images respect PORT or --port flag
	if spec.BindPort > 0 {
		args = append(args, "--port", strconv.Itoa(spec.BindPort))
	}

	args = append(args, spec.ExtraArgs...)
	return args
}

// buildLlamaCppArgs builds docker run args for llama.cpp server.
// Image: ghcr.io/ggml-org/llama.cpp:server
// The server binary is `llama-server` which exposes an OpenAI-compatible API.
//
// Model loading (first match wins):
//   a) LlamaCppModelPath set → --model /models/file.gguf  (local GGUF on volume)
//   b) LlamaCppHFRepo + LlamaCppHFFile set → --hf-repo ORG/REPO --hf-file file.gguf
//   c) LlamaCppHFRepo only → --hf-repo ORG/REPO  (server picks default GGUF)
//   d) ModelName is a local path (starts with "/") → --model <ModelName>
//
// For (b) and (c), set HUGGING_FACE_HUB_TOKEN in Env for gated repos.
// GPU offload is controlled by LlamaCppNGPULayers:
//   0   = CPU-only (default when no GPUDevices assigned)
//   -1  = all layers on GPU (default when GPUDevices is non-empty)
//   N>0 = offload N layers to GPU (partial offload)
func (d *dockerDriver) buildLlamaCppArgs(spec RuntimeSpec) []string {
	args := []string{"run", "-d",
		"--name", containerName(spec),
		"--restart", "unless-stopped",
		"--network", "host",
	}

	// CPU affinity from placement engine
	if spec.CPUSetCPUs != "" {
		args = append(args, "--cpuset-cpus", spec.CPUSetCPUs)
	}
	if spec.NUMANode >= 0 {
		args = append(args, "--cpuset-mems", strconv.Itoa(spec.NUMANode))
	}

	// GPU assignment — only set --gpus when devices are specified
	if len(spec.GPUDevices) > 0 {
		devList := make([]string, len(spec.GPUDevices))
		for i, idx := range spec.GPUDevices {
			devList[i] = strconv.Itoa(idx)
		}
		args = append(args, "--gpus", fmt.Sprintf("device=%s", strings.Join(devList, ",")))
	}

	args = d.applyCommonResourceArgs(args, spec)

	// Volume for GGUF model files.
	// Use an absolute host path as a bind-mount, or fall back to named volume.
	vol := spec.LlamaCppModelsVolume
	if vol == "" {
		vol = "llamacpp_models"
	}
	args = append(args, "-v", vol+":/models")

	// Image — must be the :server tag for serving
	args = append(args, spec.Image)

	// ── Model source (first match wins) ───────────────────────────────────────
	switch {
	case spec.LlamaCppModelPath != "":
		args = append(args, "--model", spec.LlamaCppModelPath)
	case spec.LlamaCppHFRepo != "" && spec.LlamaCppHFFile != "":
		args = append(args, "--hf-repo", spec.LlamaCppHFRepo, "--hf-file", spec.LlamaCppHFFile)
	case spec.LlamaCppHFRepo != "":
		args = append(args, "--hf-repo", spec.LlamaCppHFRepo)
	case strings.HasPrefix(spec.ModelName, "/"):
		// explicit absolute path in ModelName
		args = append(args, "--model", spec.ModelName)
	}
	// NOTE: a bare model name (no "/" prefix, not an HF repo) is intentionally
	// not passed as --model. Callers should set LlamaCppModelPath or LlamaCppHFRepo.

	// ── Server flags ──────────────────────────────────────────────────────────
	ctxSize := spec.LlamaCppCtxSize
	if ctxSize == 0 {
		ctxSize = 4096
	}
	args = append(args,
		"--host", "0.0.0.0",
		"--port", strconv.Itoa(spec.BindPort),
		"--ctx-size", strconv.Itoa(ctxSize),
	)

	// Thread count — use CPULimit if set (e.g. "8"), otherwise let llama-server auto-detect
	if spec.CPULimit != "" {
		args = append(args, "--threads", spec.CPULimit)
	}

	// GPU offload layers
	nGPULayers := spec.LlamaCppNGPULayers
	if nGPULayers == 0 && len(spec.GPUDevices) > 0 {
		nGPULayers = -1 // full GPU offload by default when GPUs are assigned
	}
	if nGPULayers != 0 {
		args = append(args, "--n-gpu-layers", strconv.Itoa(nGPULayers))
	}

	// Append any extra args last (overrides / additions from the operator)
	args = append(args, spec.ExtraArgs...)
	return args
}

// isHFRepo returns true if the model name looks like a HuggingFace repo ID (org/model).
func isHFRepo(s string) bool {
	return len(s) > 0 && strings.Count(s, "/") == 1 && !strings.HasPrefix(s, "/")
}
// to an args slice. Used by all backend builders.
func (d *dockerDriver) applyCommonResourceArgs(args []string, spec RuntimeSpec) []string {
	// Environment variables
	for k, v := range spec.Env {
		args = append(args, "-e", k+"="+v)
	}
	// Resource limits
	if spec.CPULimit != "" {
		args = append(args, "--cpus", spec.CPULimit)
	}
	if spec.MemoryLimit != "" {
		args = append(args, "--memory", spec.MemoryLimit)
	}
	return args
}

// buildOllamaArgs builds docker run args for Ollama.
// Uses bridge networking with port mapping instead of host networking
// to avoid conflicts with a native Ollama installation on the host.
func (d *dockerDriver) buildOllamaArgs(spec RuntimeSpec) []string {
	containerPort := 11434
	hostPort := spec.BindPort
	if hostPort == 0 {
		hostPort = 11434
	}
	image := spec.Image
	if image == "" {
		image = "ollama/ollama:latest"
	}

	args := []string{"run", "-d",
		"--name", containerName(spec),
		"--restart", "unless-stopped",
		// Port mapping — avoids conflict with native Ollama on host
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", hostPort, containerPort),
		"-v", "ollama_models:/root/.ollama",
	}

	// GPU support
	if len(spec.GPUDevices) > 0 {
		devList := make([]string, len(spec.GPUDevices))
		for i, idx := range spec.GPUDevices {
			devList[i] = strconv.Itoa(idx)
		}
		args = append(args, "--gpus", fmt.Sprintf("device=%s", strings.Join(devList, ",")))
	}

	args = d.applyCommonResourceArgs(args, spec)
	args = append(args, image)
	// No extra args — Ollama default entrypoint is `ollama serve`
	return args
}

// buildTGIArgs builds docker run args for HuggingFace TGI.
func (d *dockerDriver) buildTGIArgs(spec RuntimeSpec) []string {
	args := []string{"run", "-d",
		"--name", containerName(spec),
		"--restart", "unless-stopped",
		"--network", "host",
	}

	if len(spec.GPUDevices) > 0 {
		devList := make([]string, len(spec.GPUDevices))
		for i, idx := range spec.GPUDevices {
			devList[i] = strconv.Itoa(idx)
		}
		args = append(args, "--gpus", fmt.Sprintf("device=%s", strings.Join(devList, ",")))
	}

	args = d.applyCommonResourceArgs(args, spec)
	args = append(args, spec.Image)
	args = append(args, "--model-id", spec.ModelName)
	args = append(args, "--port", strconv.Itoa(spec.BindPort))

	if spec.Quantization != "" {
		args = append(args, "--quantize", spec.Quantization)
	}

	args = append(args, spec.ExtraArgs...)
	return args
}

func (d *dockerDriver) Stop(ctx context.Context, id string, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	if secs < 1 {
		secs = 30
	}
	out, err := exec.CommandContext(ctx, "docker", "stop", "-t", strconv.Itoa(secs), id).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker stop %s: %w — %s", id, err, string(out))
	}
	return nil
}

func (d *dockerDriver) Restart(ctx context.Context, id string, spec RuntimeSpec, timeout time.Duration) (string, error) {
	if err := d.Stop(ctx, id, timeout); err != nil {
		return "", err
	}
	if err := d.Remove(ctx, id); err != nil {
		return "", err
	}
	return d.Start(ctx, spec)
}

func (d *dockerDriver) Status(ctx context.Context, id string) (RuntimeStatus, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{.State.Running}}\t{{.State.StartedAt}}\t{{.State.ExitCode}}",
		id).Output()
	if err != nil {
		return RuntimeStatus{ContainerID: id, Running: false, Error: err.Error()}, nil
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "\t")
	rs := RuntimeStatus{ContainerID: id}
	if len(parts) >= 1 {
		rs.Running = parts[0] == "true"
	}
	if len(parts) >= 2 && parts[1] != "" {
		t, _ := time.Parse(time.RFC3339, parts[1])
		rs.StartedAt = &t
	}
	if len(parts) >= 3 {
		if code, err2 := strconv.Atoi(parts[2]); err2 == nil {
			rs.ExitCode = &code
		}
	}
	return rs, nil
}

func (d *dockerDriver) Logs(ctx context.Context, id string, tail int) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "logs",
		"--tail", strconv.Itoa(tail), id).CombinedOutput()
	return string(out), err
}

func (d *dockerDriver) Remove(ctx context.Context, id string) error {
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", id).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker rm %s: %w — %s", id, err, string(out))
	}
	return nil
}

func containerName(spec RuntimeSpec) string {
	// Use the served model name (short name) not the HF ID for the container name
	name := spec.ServedModelName
	if name == "" {
		name = spec.ModelName
	}
	// Make it safe for Docker: replace / : spaces with -
	r := strings.NewReplacer("/", "-", ":", "-", " ", "-", ".", "-")
	return "nexus-" + r.Replace(name)
}
