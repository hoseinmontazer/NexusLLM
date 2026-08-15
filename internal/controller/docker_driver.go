package controller

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/nexusllm/nexusllm/internal/runtime"
)

// dockerDriver runs model runtimes as plain Docker containers.
// It shells out to the docker CLI — no SDK dependency needed.
type dockerDriver struct {
	registry *runtime.Registry
}

// ─── Actual port discovery ───────────────────────────────────────────────────
//
// After docker run succeeds, the container may have bound to a different port
// than was requested (port conflict scan, backend ignoring --port, bind_port=0
// OS allocation, etc.). These helpers discover the real listening port by
// inspecting the running container, not by trusting the requested spec.BindPort.
//
// This mirrors the logic in internal/nodeagent/executor.go — both code paths
// must stay in sync. The controller runs inside the admin binary, the executor
// runs inside the node-agent binary, so the logic cannot be shared via import.

// discoverPortForBackend discovers the actual listening port for a just-started
// container. It uses a backend-aware strategy because different backends expose
// their port through different mechanisms:
//
//   - vllm, tgi, llamacpp: port passed via --port CMD arg → inspect .Args
//   - cpu_native: port via PORT/HTTP_PORT/UVICORN_PORT env vars → inspect .Env
//   - openai_compat: try PortBindings first, then --port arg, then PORT env
//   - ollama: OLLAMA_HOST env var (format "HOST:PORT" or ":PORT")
//
// Returns the discovered port, or fallback if all methods fail.
func (d *dockerDriver) discoverPortForBackend(ctx context.Context, containerName, backend string, fallback int) int {
	start := time.Now()
	const (
		maxAttempts = 15
		retryDelay  = 1 * time.Second
	)

	method := "none"
	attempts := 0

	for i := 0; i < maxAttempts; i++ {
		attempts = i + 1

		// Check container is running before probing.
		stateOut, err := exec.CommandContext(ctx, "docker", "inspect",
			"--format", "{{.State.Running}}", containerName).Output()
		if err != nil || strings.TrimSpace(string(stateOut)) != "true" {
			select {
			case <-ctx.Done():
				return fallback
			case <-time.After(retryDelay):
			}
			continue
		}

		var port int

		switch backend {
		case "ollama":
			if p := d.portFromPortBindings(ctx, containerName); p > 0 {
				port = p
				method = "port_bindings"
			} else {
				port, method = d.portFromEnvVar(ctx, containerName, []string{"OLLAMA_HOST"})
			}

		case "cpu_native":
			if p := d.portFromPortBindings(ctx, containerName); p > 0 {
				port = p
				method = "port_bindings"
			} else {
				port, method = d.portFromEnvVar(ctx, containerName, []string{"PORT", "HTTP_PORT", "UVICORN_PORT"})
			}

		case "vllm", "tgi", "llamacpp":
			// These pass port via --port CMD arg.
			// Try PortBindings first (bridge mode), then arg scan (host mode).
			if p := d.portFromPortBindings(ctx, containerName); p > 0 {
				port = p
				method = "port_bindings"
			} else {
				port, method = d.portFromArgFlag(ctx, containerName)
			}

		case "openai_compat":
			// Generic: try PortBindings → --port arg → PORT env var.
			if p := d.portFromPortBindings(ctx, containerName); p > 0 {
				port = p
				method = "port_bindings"
			} else if p, m := d.portFromArgFlag(ctx, containerName); p > 0 {
				port = p
				method = m
			} else {
				port, method = d.portFromEnvVar(ctx, containerName, []string{"PORT", "HTTP_PORT"})
			}

		default:
			// Unknown backend — try all methods.
			if p := d.portFromPortBindings(ctx, containerName); p > 0 {
				port = p
				method = "port_bindings"
			} else if p, m := d.portFromArgFlag(ctx, containerName); p > 0 {
				port = p
				method = m
			} else {
				port, method = d.portFromEnvVar(ctx, containerName, []string{"PORT", "HTTP_PORT", "OLLAMA_HOST"})
			}
		}

		if port > 0 {
			elapsed := time.Since(start)
			_ = elapsed // used in structured log in caller
			return port
		}

		select {
		case <-ctx.Done():
			return fallback
		case <-time.After(retryDelay):
		}
	}

	_ = method
	_ = attempts
	return fallback
}

// portFromPortBindings reads the HostPort from docker inspect HostConfig.PortBindings.
// Works for bridge-networked containers where docker manages the mapping.
func (d *dockerDriver) portFromPortBindings(ctx context.Context, containerName string) int {
	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", `{{range $p, $conf := .HostConfig.PortBindings}}{{if $conf}}{{(index $conf 0).HostPort}}{{end}}{{end}}`,
		containerName).Output()
	if err != nil {
		return 0
	}
	if p := strings.TrimSpace(string(out)); p != "" {
		if port, err := strconv.Atoi(p); err == nil && port > 0 {
			return port
		}
	}
	return 0
}

// portFromArgFlag reads the --port argument from the container's process args.
// Works for host-networked containers (vLLM, TGI, llamacpp) started with --port N.
func (d *dockerDriver) portFromArgFlag(ctx context.Context, containerName string) (int, string) {
	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", `{{join .Args " "}}`, containerName).Output()
	if err != nil {
		return 0, "none"
	}
	args := strings.Fields(strings.TrimSpace(string(out)))
	for i, arg := range args {
		if (arg == "--port" || arg == "-p") && i+1 < len(args) {
			if port, err := strconv.Atoi(args[i+1]); err == nil && port > 0 {
				return port, "arg_flag"
			}
		}
		if strings.HasPrefix(arg, "--port=") {
			if port, err := strconv.Atoi(strings.TrimPrefix(arg, "--port=")); err == nil && port > 0 {
				return port, "arg_flag"
			}
		}
	}
	return 0, "none"
}

// portFromEnvVar reads the named environment variables from the container and
// extracts the port. Handles "HOST:PORT" (Ollama format) and plain "PORT" values.
func (d *dockerDriver) portFromEnvVar(ctx context.Context, containerName string, vars []string) (int, string) {
	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", `{{range .Config.Env}}{{println .}}{{end}}`,
		containerName).Output()
	if err != nil {
		return 0, "none"
	}
	envMap := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		if idx := strings.IndexByte(line, '='); idx > 0 {
			envMap[line[:idx]] = strings.TrimSpace(line[idx+1:])
		}
	}
	for _, v := range vars {
		val := envMap[v]
		if val == "" {
			continue
		}
		// Strip host component: "0.0.0.0:7997" → "7997", ":7997" → "7997"
		if idx := strings.LastIndexByte(val, ':'); idx >= 0 {
			val = val[idx+1:]
		}
		if port, err := strconv.Atoi(val); err == nil && port > 0 {
			return port, "env_var:" + v
		}
	}
	return 0, "none"
}

// NewDockerDriver constructs a Docker driver.
// registry is required to obtain backend-specific port environment variables.
func NewDockerDriver(registry *runtime.Registry) Driver {
	return &dockerDriver{registry: registry}
}

func (d *dockerDriver) Type() DriverType { return DriverDocker }

func (d *dockerDriver) Start(ctx context.Context, spec RuntimeSpec) (string, error) {
	var args []string

	switch spec.BackendType {
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
//
// Port injection strategy (dual-path):
//  1. Env vars: PORT, HTTP_PORT, UVICORN_PORT are injected by
//     applyCommonResourceArgs → Backend.ContainerPortEnvVars() for servers
//     that read them (uvicorn-based: faster-whisper-server, Kokoro).
//  2. --port CMD arg: injected below for CLI-driven servers that ignore env
//     vars and only accept --port as a flag (Infinity, TEI-cpu, EasyOCR).
//     Only injected when BindPort > 0 and ExtraArgs doesn't already supply
//     --port (to avoid duplicates).
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

	if spec.BindPort > 0 {
		hasPortFlag := false
		for _, a := range spec.ExtraArgs {
			if a == "--port" || a == "-p" || strings.HasPrefix(a, "--port=") {
				hasPortFlag = true
				break
			}
		}
		if !hasPortFlag {
			spec.ExtraArgs = append(spec.ExtraArgs, "--port", strconv.Itoa(spec.BindPort))
		}
	}

	// Infinity model injection: infinity_emb serves its own built-in default
	// model (bge-small-en-v1.5) whenever it isn't told which model to load.
	// If the operator forgot to put --model-name-or-path/--model-id in
	// ExtraArgs, the container comes up healthy but silently serves the
	// wrong model. Auto-inject it from ModelName for Infinity images only —
	// other cpu_native services have their own model flag conventions and
	// must keep specifying them via ExtraArgs.
	//
	// ONLY inject when ModelName looks like a real HuggingFace repo id
	// ("org/repo"). Callers (e.g. DeployModel) fall back to the short display
	// name when hf_model_id wasn't set — injecting THAT as --model-id makes
	// infinity_emb fail to resolve the model and crash-loop on every start,
	// turning a harmless "serving the wrong default model" bug into a hard
	// outage. Skipping injection preserves the old (silently wrong but at
	// least running) behavior for misconfigured deploys.
	if strings.Contains(strings.ToLower(spec.Image), "infinity") && spec.ModelName != "" {
		hasModelFlag := false
		hasV2 := false
		for _, a := range spec.ExtraArgs {
			if a == "--model-name-or-path" || a == "--model-id" || strings.HasPrefix(a, "--model-name-or-path=") || strings.HasPrefix(a, "--model-id=") {
				hasModelFlag = true
			}
			if a == "v2" {
				hasV2 = true
			}
		}
		if !hasModelFlag && strings.Contains(spec.ModelName, "/") {
			if !hasV2 {
				spec.ExtraArgs = append([]string{"v2"}, spec.ExtraArgs...)
			}
			spec.ExtraArgs = append(spec.ExtraArgs, "--model-id", spec.ModelName)
		}
	}

	args = append(args, spec.ExtraArgs...)
	return args
}

// buildLlamaCppArgs builds docker run args for llama.cpp server.
// Reads from the generic RuntimeSpec fields (GGUFPath, HFRepo, HFFile, CtxSize,
// NGPULayers, ModelsVolume) — no longer has LlamaCpp-prefixed field names.
//
// Model loading (first match wins):
//
//	a) GGUFPath set → --model /models/file.gguf  (local GGUF on volume)
//	b) HFRepo + HFFile set → --hf-repo ORG/REPO --hf-file file.gguf
//	c) HFRepo only → --hf-repo ORG/REPO  (server picks default GGUF)
//	d) ModelName is an absolute path (starts with "/") → --model <ModelName>
func (d *dockerDriver) buildLlamaCppArgs(spec RuntimeSpec) []string {
	args := []string{"run", "-d",
		"--name", containerName(spec),
		"--restart", "unless-stopped",
		"--network", "host",
	}

	if spec.CPUSetCPUs != "" {
		args = append(args, "--cpuset-cpus", spec.CPUSetCPUs)
	}
	if spec.NUMANode >= 0 {
		args = append(args, "--cpuset-mems", strconv.Itoa(spec.NUMANode))
	}

	if len(spec.GPUDevices) > 0 {
		devList := make([]string, len(spec.GPUDevices))
		for i, idx := range spec.GPUDevices {
			devList[i] = strconv.Itoa(idx)
		}
		args = append(args, "--gpus", fmt.Sprintf("device=%s", strings.Join(devList, ",")))
	}

	args = d.applyCommonResourceArgs(args, spec)

	vol := spec.ModelsVolume
	if vol == "" {
		vol = "nexus_models"
	}
	args = append(args, "-v", vol+":/models")
	args = append(args, spec.Image)

	// ── Model source (first match wins) ───────────────────────────────────────
	switch {
	case spec.GGUFPath != "":
		args = append(args, "--model", spec.GGUFPath)
	case spec.HFRepo != "" && spec.HFFile != "":
		args = append(args, "--hf-repo", spec.HFRepo, "--hf-file", spec.HFFile)
	case spec.HFRepo != "":
		args = append(args, "--hf-repo", spec.HFRepo)
	case strings.HasPrefix(spec.ModelName, "/"):
		args = append(args, "--model", spec.ModelName)
	}

	ctxSize := spec.CtxSize
	if ctxSize == 0 {
		ctxSize = 4096
	}
	args = append(args,
		"--host", "0.0.0.0",
		"--port", strconv.Itoa(spec.BindPort),
		"--ctx-size", strconv.Itoa(ctxSize),
	)

	if spec.CPULimit != "" {
		args = append(args, "--threads", spec.CPULimit)
	}

	nGPULayers := spec.NGPULayers
	if nGPULayers == 0 && len(spec.GPUDevices) > 0 {
		nGPULayers = -1
	}
	if nGPULayers != 0 {
		args = append(args, "--n-gpu-layers", strconv.Itoa(nGPULayers))
	}

	args = append(args, spec.ExtraArgs...)
	return args
}

// applyCommonResourceArgs injects environment variables and resource limits
// to an args slice. Used by all backend builders.
//
// IMPORTANT: This is where backend-specific port environment variables are
// injected via the Backend interface, ensuring the legacy Docker controller
// path behaves identically to the node agent path.
func (d *dockerDriver) applyCommonResourceArgs(args []string, spec RuntimeSpec) []string {
	// ── Inject backend-specific port environment variables ──────────────────
	// Obtain PORT, HTTP_PORT, UVICORN_PORT (or nil) from the Backend interface.
	// This ensures cpu_native and openai_compat backends receive the correct
	// port env vars, identical to the node agent executor path.
	//
	// The Backend interface is the single source of truth for which env vars
	// each backend type requires. Never hardcode port env var names here.
	if d.registry != nil && spec.BindPort > 0 {
		backend := d.registry.BackendForType(spec.BackendType)
		for k, v := range backend.ContainerPortEnvVars(spec.BindPort) {
			// Unconditionally inject the correct backend port (system port wins).
			spec.Env[k] = v
		}
	}

	// ── Environment variables ────────────────────────────────────────────────
	// Now inject all env vars (backend port vars + user-supplied vars).
	for k, v := range spec.Env {
		args = append(args, "-e", k+"="+v)
	}

	// ── Resource limits ──────────────────────────────────────────────────────
	if spec.CPULimit != "" {
		args = append(args, "--cpus", spec.CPULimit)
	}
	if spec.MemoryLimit != "" {
		args = append(args, "--memory", spec.MemoryLimit)
	}
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
