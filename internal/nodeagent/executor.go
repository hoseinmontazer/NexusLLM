// Package nodeagent contains the task executor that runs ON each node.
// The executor receives tasks from the control plane and executes them locally
// using the Docker driver.
//
// Key principle: The executor NEVER makes placement or scheduling decisions.
// It only executes what it's told by the control plane.
package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// ─────────────────────────────────────────────────────────────────────────────
// Task types (mirrors taskmanager package — no import to keep agent lean)
// ─────────────────────────────────────────────────────────────────────────────

type TaskType string

const (
	TaskDeployRuntime    TaskType = "DEPLOY_RUNTIME"
	TaskStopRuntime      TaskType = "STOP_RUNTIME"
	TaskRestartRuntime   TaskType = "RESTART_RUNTIME"
	TaskDeleteRuntime    TaskType = "DELETE_RUNTIME"
	TaskWarmRuntime      TaskType = "WARM_RUNTIME"
	TaskUnloadRuntime    TaskType = "UNLOAD_RUNTIME"
	TaskPullModel        TaskType = "PULL_MODEL"
	TaskDeleteModel      TaskType = "DELETE_MODEL"
	TaskVerifyModel      TaskType = "VERIFY_MODEL"
	TaskCollectInventory TaskType = "COLLECT_INVENTORY"
	TaskHealthCheck      TaskType = "HEALTH_CHECK"
)

// RemoteTask is a task as received from the control plane API.
type RemoteTask struct {
	ID          string          `json:"id"`
	TaskType    TaskType        `json:"task_type"`
	Payload     json.RawMessage `json:"payload"`
	Priority    int             `json:"priority"`
	CreatedAt   time.Time       `json:"created_at"`
}
type TaskResult struct {
	Success     bool              `json:"success"`
	RuntimeID   string            `json:"runtime_id,omitempty"`
	RuntimeState string           `json:"runtime_state,omitempty"`
	ContainerID string            `json:"container_id,omitempty"`
	Error       string            `json:"error,omitempty"`
	Data        map[string]interface{} `json:"data,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Executor
// ─────────────────────────────────────────────────────────────────────────────

// Executor runs tasks received from the control plane on the local node.
// It is stateless between task executions.
type Executor struct {
	log *zap.Logger
}

// NewExecutor constructs an Executor.
func NewExecutor(log *zap.Logger) *Executor {
	return &Executor{log: log}
}

// Execute dispatches a task to the appropriate handler.
// Returns a TaskResult to report back to the control plane.
func (e *Executor) Execute(ctx context.Context, task RemoteTask) TaskResult {
	e.log.Info("executing task",
		zap.String("task_id", task.ID),
		zap.String("task_type", string(task.TaskType)),
	)

	start := time.Now()
	var result TaskResult

	switch task.TaskType {
	case TaskDeployRuntime:
		result = e.deployRuntime(ctx, task)
	case TaskStopRuntime:
		result = e.stopRuntime(ctx, task)
	case TaskRestartRuntime:
		result = e.restartRuntime(ctx, task)
	case TaskDeleteRuntime:
		result = e.deleteRuntime(ctx, task)
	case TaskWarmRuntime:
		result = e.warmRuntime(ctx, task)
	case TaskUnloadRuntime:
		result = e.unloadRuntime(ctx, task)
	case TaskPullModel:
		result = e.pullModel(ctx, task)
	case TaskDeleteModel:
		result = e.deleteModel(ctx, task)
	case TaskCollectInventory:
		result = e.collectInventory(ctx)
	case TaskHealthCheck:
		result = e.healthCheck(ctx, task)
	default:
		result = TaskResult{Success: false, Error: fmt.Sprintf("unknown task type: %s", task.TaskType)}
	}

	elapsed := time.Since(start)
	if result.Success {
		e.log.Info("task completed",
			zap.String("task_id", task.ID),
			zap.Duration("elapsed", elapsed),
		)
	} else {
		e.log.Warn("task failed",
			zap.String("task_id", task.ID),
			zap.String("error", result.Error),
			zap.Duration("elapsed", elapsed),
		)
	}
	return result
}

// unmarshalPayload handles payloads that may be double-encoded
// (stored as a JSON string containing JSON) or normally encoded (raw JSON object).
func unmarshalPayload(raw json.RawMessage, out interface{}) error {
	if len(raw) == 0 {
		return nil
	}
	// Direct unmarshal first
	if err := json.Unmarshal(raw, out); err == nil {
		return nil
	}
	// Try unwrapping a JSON string
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("cannot unmarshal payload: %w", err)
	}
	return json.Unmarshal([]byte(s), out)
}

type deployPayload struct {
	RuntimeID      string            `json:"runtime_id"`
	EndpointID     string            `json:"endpoint_id"`
	ModelID        string            `json:"model_id"`
	RuntimeName    string            `json:"runtime_name"`
	Backend        string            `json:"backend"`
	Image          string            `json:"image"`
	ModelName      string            `json:"model_name"`
	ServedAs       string            `json:"served_as"`
	BindHost       string            `json:"bind_host"`
	BindPort       int               `json:"bind_port"`
	GPUDevices     []int             `json:"gpu_devices"`
	CPUSetCPUs     string            `json:"cpuset_cpus"`
	NUMANode       int               `json:"numa_node"`
	MemoryLimit    string            `json:"memory_limit"`
	CPULimit       string            `json:"cpu_limit"`
	TensorParallel int               `json:"tensor_parallel"`
	GPUMemoryUtil  float64           `json:"gpu_memory_util"`
	MaxModelLen    int               `json:"max_model_len"`
	Dtype          string            `json:"dtype"`
	Quantization   string            `json:"quantization"`
	ExtraArgs      []string          `json:"extra_args"`
	Env            map[string]string `json:"env"`
	HFToken        string            `json:"hf_token,omitempty"`

	// llamacpp-specific fields
	// LlamaCppModelPath is the local GGUF path inside the container (e.g. /models/7B/ggml-model-q4_0.gguf).
	// Takes precedence over HFRepo+HFFile when set.
	LlamaCppModelPath string `json:"llamacpp_model_path,omitempty"`
	// LlamaCppHFRepo + LlamaCppHFFile: if set, llama-server will download the GGUF
	// directly from HuggingFace at startup using the built-in --hf-repo / --hf-file flags.
	// Requires HUGGING_FACE_HUB_TOKEN for gated repos.
	LlamaCppHFRepo string `json:"llamacpp_hf_repo,omitempty"`
	LlamaCppHFFile string `json:"llamacpp_hf_file,omitempty"`
	// LlamaCppCtxSize overrides the default context window (default: 4096).
	LlamaCppCtxSize int `json:"llamacpp_ctx_size,omitempty"`
	// LlamaCppNGPULayers sets --n-gpu-layers for partial/full GPU offload.
	// 0 = CPU-only, -1 = all layers on GPU.
	LlamaCppNGPULayers int `json:"llamacpp_n_gpu_layers,omitempty"`
	// LlamaCppModelsVolume is the host-side bind-mount source for GGUF model files.
	// Defaults to the named Docker volume "llamacpp_models" if empty.
	// Set to an absolute host path (e.g. "/mnt/models") to use a bind-mount instead.
	LlamaCppModelsVolume string `json:"llamacpp_models_volume,omitempty"`
}

func (e *Executor) deployRuntime(ctx context.Context, task RemoteTask) TaskResult {
	var p deployPayload
	if err := unmarshalPayload(task.Payload, &p); err != nil {
		return TaskResult{Success: false, Error: "parse payload: " + err.Error()}
	}

	// Add HF token to env if provided
	env := p.Env
	if env == nil {
		env = make(map[string]string)
	}
	if p.HFToken != "" {
		env["HUGGING_FACE_HUB_TOKEN"] = p.HFToken
	}

	// ── Auto-download GGUF if needed (llamacpp only) ──────────────────────
	// When a local model path is requested but the file is missing on the
	// volume, attempt to download it before starting the container.
	// Priority: LlamaCppModelPath → LlamaCppHFRepo+File → ModelName fallback.
	if p.Backend == "llamacpp" {
		if result := e.ensureGGUFExists(ctx, &p); !result.Success {
			return result
		}
	}

	// ── Remove any existing container with the same name ─────────────────
	// This handles the case where a previous (possibly broken) container with
	// --restart unless-stopped is still present. Without this, docker run would
	// fail with "container name already in use" and the old broken container
	// would keep restarting indefinitely.
	if p.RuntimeName != "" {
		// Ignore errors — container may not exist, which is fine.
		rmOut, rmErr := exec.CommandContext(ctx, "docker", "rm", "-f", p.RuntimeName).CombinedOutput()
		if rmErr == nil {
			e.log.Info("removed existing container before redeploy",
				zap.String("name", p.RuntimeName),
			)
		} else if !strings.Contains(string(rmOut), "No such container") {
			e.log.Warn("docker rm pre-flight warning",
				zap.String("name", p.RuntimeName),
				zap.String("output", string(rmOut)),
			)
		}
	}

	// Build docker run args from the payload
	// The control plane has already decided ALL placement details.
	// We just execute.
	args := e.buildDockerArgs(p, env)

	e.log.Info("starting container",
		zap.String("runtime", p.RuntimeName),
		zap.String("backend", p.Backend),
		zap.String("image", p.Image),
		zap.Ints("gpu_devices", p.GPUDevices),
	)

	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return TaskResult{
			Success:     false,
			RuntimeID:   p.RuntimeID,
			RuntimeState: "failed",
			Error:       fmt.Sprintf("docker run: %s — output: %s", err.Error(), string(out)),
		}
	}

	containerID := strings.TrimSpace(string(out))

	// For Ollama, pull the model after the server starts
	if p.Backend == "ollama" && p.ServedAs != "" {
		time.Sleep(8 * time.Second)
		pullOut, pullErr := exec.CommandContext(ctx, "docker", "exec", containerID,
			"ollama", "pull", p.ServedAs).CombinedOutput()
		if pullErr != nil {
			return TaskResult{
				Success:     false,
				RuntimeID:   p.RuntimeID,
				ContainerID: containerID,
				RuntimeState: "failed",
				Error:       fmt.Sprintf("ollama pull: %s — %s", pullErr.Error(), string(pullOut)),
			}
		}
	}

	return TaskResult{
		Success:     true,
		RuntimeID:   p.RuntimeID,
		ContainerID: containerID,
		RuntimeState: "loading",
		Data: map[string]interface{}{
			"container_id": containerID,
			"bind_port":    p.BindPort,
		},
	}
}

// ensureGGUFExists checks whether the model file is present on the host volume
// and downloads it if not. It covers three cases:
//
//  1. LlamaCppModelPath is set → check the file exists on the host volume;
//     if missing and LlamaCppHFRepo+HFFile are also set, download via HF.
//  2. LlamaCppHFRepo+HFFile are set but no local path → nothing to pre-check;
//     llama-server will download at startup.
//  3. Neither is set and ModelName doesn't look like a real path → error early
//     with a clear message rather than letting llama-server fail cryptically.
func (e *Executor) ensureGGUFExists(ctx context.Context, p *deployPayload) TaskResult {
	// Case 3: no usable model source — fail fast with a clear error.
	if p.LlamaCppModelPath == "" && p.LlamaCppHFRepo == "" && !strings.HasPrefix(p.ModelName, "/") {
		return TaskResult{
			Success: false,
			Error: fmt.Sprintf(
				"llamacpp backend requires a model source: set llamacpp_model_path (local GGUF), "+
					"llamacpp_hf_repo+llamacpp_hf_file (HuggingFace download), or an absolute path in hf_model_id. "+
					"Got bare model name %q which is not a valid GGUF path.",
				p.ModelName,
			),
		}
	}

	if p.LlamaCppModelPath == "" {
		// No local path requested — HF download at startup or absolute ModelName;
		// llama-server handles it.
		return TaskResult{Success: true}
	}

	// Resolve the host-side path from the volume mount.
	// Inside the container the volume is always mounted at /models, so strip
	// the /models prefix and look under the host volume root.
	hostVolumeRoot := p.LlamaCppModelsVolume
	if hostVolumeRoot == "" {
		hostVolumeRoot = "llamacpp_models"
	}

	// For a named Docker volume we can resolve its host path via docker inspect.
	// For an absolute bind-mount path we use it directly.
	hostVolumeDir := hostVolumeRoot
	if !strings.HasPrefix(hostVolumeRoot, "/") {
		// Named volume — ask Docker for the mountpoint.
		out, err := exec.CommandContext(ctx, "docker", "volume", "inspect",
			"--format", "{{.Mountpoint}}", hostVolumeRoot).Output()
		if err != nil {
			// Volume doesn't exist yet — Docker will create it on container start.
			// We can't check the file, so proceed and let llama-server fail with a
			// clear error if the file is truly missing.
			e.log.Warn("volume not found, skipping GGUF pre-check",
				zap.String("volume", hostVolumeRoot),
				zap.Error(err),
			)
			return TaskResult{Success: true}
		}
		hostVolumeDir = strings.TrimSpace(string(out))
	}

	// Map container path → host path.
	// e.g. /models/gemma-2b/model.gguf → <hostVolumeDir>/gemma-2b/model.gguf
	containerPath := p.LlamaCppModelPath
	relPath := strings.TrimPrefix(containerPath, "/models/")
	if relPath == containerPath {
		// Path doesn't start with /models — unusual; try as-is relative to volume.
		relPath = strings.TrimPrefix(containerPath, "/")
	}
	hostFilePath := hostVolumeDir + "/" + relPath

	// Check if the file already exists.
	if _, err := exec.CommandContext(ctx, "test", "-f", hostFilePath).CombinedOutput(); err == nil {
		e.log.Info("GGUF file already present, skipping download",
			zap.String("path", hostFilePath),
		)
		return TaskResult{Success: true}
	}

	// File is missing. Decide how to get it.
	e.log.Info("GGUF file not found, attempting download",
		zap.String("host_path", hostFilePath),
		zap.String("hf_repo", p.LlamaCppHFRepo),
		zap.String("hf_file", p.LlamaCppHFFile),
	)

	switch {
	case p.LlamaCppHFRepo != "" && p.LlamaCppHFFile != "":
		result, actualHostPath := e.downloadGGUFFromHF(ctx, *p, hostVolumeDir, relPath)
		if !result.Success {
			return result
		}
		// Remap the container path to match the actual downloaded filename.
		// e.g. host: /var/lib/.../gemma-2b/gemma-2-2b-it-Q4_K_M.gguf
		//   → container: /models/gemma-2b/gemma-2-2b-it-Q4_K_M.gguf
		relActual := strings.TrimPrefix(actualHostPath, hostVolumeDir+"/")
		p.LlamaCppModelPath = "/models/" + relActual
		e.log.Info("resolved GGUF container path after download",
			zap.String("container_path", p.LlamaCppModelPath),
		)
		return result

	case p.LlamaCppHFRepo != "":
		result, actualHostPath := e.downloadGGUFFromHF(ctx, *p, hostVolumeDir, relPath)
		if !result.Success {
			return result
		}
		relActual := strings.TrimPrefix(actualHostPath, hostVolumeDir+"/")
		p.LlamaCppModelPath = "/models/" + relActual
		return result

	default:
		return TaskResult{
			Success: false,
			Error: fmt.Sprintf(
				"GGUF file not found at %s and no HF repo configured for auto-download. "+
					"Set llamacpp_hf_repo + llamacpp_hf_file to enable auto-download, "+
					"or pre-populate the volume manually.",
				containerPath,
			),
		}
	}
}

// downloadGGUFFromHF downloads a GGUF file from HuggingFace into the host volume.
// It uses `huggingface-cli` if available (fastest, respects LFS), falling back
// to `wget` for simple single-file GGUF repos.
// Returns the TaskResult and the actual host path where the file was saved.
// The caller should update LlamaCppModelPath to the container-relative path
// derived from the returned host path.
func (e *Executor) downloadGGUFFromHF(ctx context.Context, p deployPayload, hostVolumeDir, relPath string) (TaskResult, string) {
	// Derive the target directory on the host volume.
	// relPath is like "gemma-2b/model.gguf" — we want "gemma-2b/" as the target dir.
	targetDir := hostVolumeDir
	if idx := strings.LastIndex(relPath, "/"); idx >= 0 {
		targetDir = hostVolumeDir + "/" + relPath[:idx]
	}

	// Ensure the target directory exists.
	if out, err := exec.CommandContext(ctx, "mkdir", "-p", targetDir).CombinedOutput(); err != nil {
		return TaskResult{Success: false,
			Error: fmt.Sprintf("mkdir %s: %s — %s", targetDir, err, string(out))}, ""
	}

	// Try huggingface-cli first (handles auth, resumes, LFS).
	if path, err := exec.LookPath("huggingface-cli"); err == nil {
		e.log.Info("downloading GGUF via huggingface-cli",
			zap.String("hf_repo", p.LlamaCppHFRepo),
			zap.String("hf_file", p.LlamaCppHFFile),
			zap.String("target_dir", targetDir),
			zap.String("tool", path),
		)

		args := []string{"download", p.LlamaCppHFRepo}
		if p.LlamaCppHFFile != "" {
			args = append(args, p.LlamaCppHFFile)
		}
		args = append(args, "--local-dir", targetDir)

		cmd := exec.CommandContext(ctx, "huggingface-cli", args...)
		if p.HFToken != "" {
			cmd.Env = append(cmd.Environ(), "HUGGING_FACE_HUB_TOKEN="+p.HFToken)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			return TaskResult{Success: false,
				Error: fmt.Sprintf("huggingface-cli download: %s — %s", err, string(out))}, ""
		}
		actualPath := targetDir + "/" + p.LlamaCppHFFile
		e.log.Info("GGUF downloaded via huggingface-cli",
			zap.String("target_dir", targetDir),
			zap.String("file", actualPath),
		)
		return TaskResult{Success: true}, actualPath
	}

	// Fallback: wget for a direct GGUF file URL.
	if p.LlamaCppHFFile == "" {
		return TaskResult{Success: false,
			Error: "huggingface-cli not found and no specific hf_file set for wget fallback; " +
				"install huggingface-cli or specify llamacpp_hf_file"}, ""
	}

	url := fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s", p.LlamaCppHFRepo, p.LlamaCppHFFile)
	targetFile := targetDir + "/" + p.LlamaCppHFFile

	e.log.Info("downloading GGUF via wget",
		zap.String("url", url),
		zap.String("target", targetFile),
	)

	wgetArgs := []string{"-c", "-O", targetFile, url}
	if p.HFToken != "" {
		wgetArgs = append([]string{"--header", "Authorization: Bearer " + p.HFToken}, wgetArgs...)
	}

	out, err := exec.CommandContext(ctx, "wget", wgetArgs...).CombinedOutput()
	if err != nil {
		return TaskResult{Success: false,
			Error: fmt.Sprintf("wget download: %s — %s", err, string(out))}, ""
	}

	e.log.Info("GGUF downloaded via wget", zap.String("target", targetFile))
	return TaskResult{Success: true}, targetFile
}

func (e *Executor) buildDockerArgs(p deployPayload, env map[string]string) []string {
	args := []string{"run", "-d", "--name", p.RuntimeName, "--restart", "unless-stopped"}

	switch p.Backend {
	case "ollama":
		containerPort := 11434
		host := p.BindHost
		if host == "" {
			host = "127.0.0.1"
		}
		args = append(args, "-p", fmt.Sprintf("%s:%d:%d", host, p.BindPort, containerPort))
		args = append(args, "-v", "ollama_models:/root/.ollama")
	case "llamacpp":
		// Mount volume for GGUF model files.
		// Use a host bind-mount if LlamaCppModelsVolume is an absolute path,
		// otherwise fall back to the named Docker volume "llamacpp_models".
		vol := p.LlamaCppModelsVolume
		if vol == "" {
			vol = "llamacpp_models"
		}
		args = append(args, "-v", vol+":/models")
		// llamacpp uses host networking
		args = append(args, "--network", "host")
	default:
		// host networking — set for vllm/tgi
		args = append(args, "--network", "host")
	}

	// GPU assignment — provided by control plane, not decided here
	if len(p.GPUDevices) > 0 {
		devList := make([]string, len(p.GPUDevices))
		for i, idx := range p.GPUDevices {
			devList[i] = strconv.Itoa(idx)
		}
		args = append(args, "--gpus", "device="+strings.Join(devList, ","))
	}

	// CPU affinity — provided by control plane
	if p.CPUSetCPUs != "" {
		args = append(args, "--cpuset-cpus", p.CPUSetCPUs)
	}
	if p.NUMANode >= 0 {
		args = append(args, "--cpuset-mems", strconv.Itoa(p.NUMANode))
	}

	// Resource limits
	if p.CPULimit != "" {
		args = append(args, "--cpus", p.CPULimit)
	}
	if p.MemoryLimit != "" {
		args = append(args, "--memory", p.MemoryLimit)
	}

	// Environment
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}

	// Image
	args = append(args, p.Image)

	// Backend-specific args
	switch p.Backend {
	case "vllm":
		args = append(args, "--model", p.ModelName, "--port", strconv.Itoa(p.BindPort))
		if p.ServedAs != "" && p.ServedAs != p.ModelName {
			args = append(args, "--served-model-name", p.ServedAs)
		}
		if p.TensorParallel > 1 {
			args = append(args, "--tensor-parallel-size", strconv.Itoa(p.TensorParallel))
		}
		if p.GPUMemoryUtil > 0 {
			args = append(args, "--gpu-memory-utilization", fmt.Sprintf("%.2f", p.GPUMemoryUtil))
		}
		if p.MaxModelLen > 0 {
			args = append(args, "--max-model-len", strconv.Itoa(p.MaxModelLen))
		}
		if p.Dtype != "" && p.Dtype != "auto" {
			args = append(args, "--dtype", p.Dtype)
		}
		if p.Quantization != "" {
			args = append(args, "--quantization", p.Quantization)
		}
	case "tgi":
		args = append(args, "--model-id", p.ModelName, "--port", strconv.Itoa(p.BindPort))
		if p.Quantization != "" {
			args = append(args, "--quantize", p.Quantization)
		}
	case "llamacpp":
		// llama-server (ghcr.io/ggml-org/llama.cpp:server) exposes an OpenAI-compatible API.
		//
		// Model source priority (first match wins):
		//   1. LlamaCppModelPath — absolute path inside the container (/models/file.gguf)
		//   2. LlamaCppHFRepo + LlamaCppHFFile — download at startup via --hf-repo/--hf-file
		//   3. LlamaCppHFRepo alone — server picks the default GGUF from the repo
		//   4. ModelName starting with "/" — treat as an absolute container path
		//
		// A bare model name (e.g. "gemma-2") is NOT a valid source and is rejected
		// by ensureGGUFExists before we reach here.
		switch {
		case p.LlamaCppModelPath != "":
			args = append(args, "--model", p.LlamaCppModelPath)
		case p.LlamaCppHFRepo != "" && p.LlamaCppHFFile != "":
			args = append(args, "--hf-repo", p.LlamaCppHFRepo, "--hf-file", p.LlamaCppHFFile)
		case p.LlamaCppHFRepo != "":
			// repo only — llama-server will pick the default GGUF from the repo
			args = append(args, "--hf-repo", p.LlamaCppHFRepo)
		case strings.HasPrefix(p.ModelName, "/"):
			// explicit absolute path passed via ModelName
			args = append(args, "--model", p.ModelName)
		}
		// NOTE: a bare ModelName (no "/" prefix, not an HF repo) intentionally
		// produces no --model flag. ensureGGUFExists catches this case early and
		// returns an error before the container is started.

		ctxSize := p.LlamaCppCtxSize
		if ctxSize == 0 {
			ctxSize = 4096
		}
		args = append(args,
			"--host", "0.0.0.0",
			"--port", strconv.Itoa(p.BindPort),
			"--ctx-size", strconv.Itoa(ctxSize),
		)

		// Thread count from CPULimit (e.g. "4" = 4 threads)
		if p.CPULimit != "" {
			args = append(args, "--threads", p.CPULimit)
		}

		// GPU offload layers: -1 = all layers on GPU, 0 = CPU-only
		if len(p.GPUDevices) > 0 && p.LlamaCppNGPULayers == 0 {
			// default to full offload when GPUs are assigned
			args = append(args, "--n-gpu-layers", "-1")
		} else if p.LlamaCppNGPULayers != 0 {
			args = append(args, "--n-gpu-layers", strconv.Itoa(p.LlamaCppNGPULayers))
		}
	}

	args = append(args, p.ExtraArgs...)
	return args
}

// ─── STOP_RUNTIME ─────────────────────────────────────────────────────────────

type stopPayload struct {
	RuntimeID   string `json:"runtime_id"`
	ContainerID string `json:"container_id"`
	DrainSecs   int    `json:"drain_secs"`
}

func (e *Executor) stopRuntime(ctx context.Context, task RemoteTask) TaskResult {
	var p stopPayload
	if err := unmarshalPayload(task.Payload, &p); err != nil {
		return TaskResult{Success: false, Error: err.Error()}
	}
	secs := p.DrainSecs
	if secs <= 0 {
		secs = 30
	}
	out, err := exec.CommandContext(ctx, "docker", "stop", "-t", strconv.Itoa(secs), p.ContainerID).CombinedOutput()
	if err != nil {
		return TaskResult{Success: false, RuntimeID: p.RuntimeID, RuntimeState: "failed",
			Error: fmt.Sprintf("docker stop: %s — %s", err, string(out))}
	}
	return TaskResult{Success: true, RuntimeID: p.RuntimeID, RuntimeState: "stopped"}
}

// ─── RESTART_RUNTIME ──────────────────────────────────────────────────────────

type restartPayload struct {
	RuntimeID   string `json:"runtime_id"`
	ContainerID string `json:"container_id"`
}

func (e *Executor) restartRuntime(ctx context.Context, task RemoteTask) TaskResult {
	var p restartPayload
	if err := unmarshalPayload(task.Payload, &p); err != nil {
		return TaskResult{Success: false, Error: err.Error()}
	}
	out, err := exec.CommandContext(ctx, "docker", "restart", p.ContainerID).CombinedOutput()
	if err != nil {
		return TaskResult{Success: false, RuntimeID: p.RuntimeID, RuntimeState: "failed",
			Error: fmt.Sprintf("docker restart: %s — %s", err, string(out))}
	}
	return TaskResult{Success: true, RuntimeID: p.RuntimeID, RuntimeState: "loading",
		ContainerID: p.ContainerID}
}

// ─── DELETE_RUNTIME ───────────────────────────────────────────────────────────

type deleteRuntimePayload struct {
	RuntimeID   string `json:"runtime_id"`
	ContainerID string `json:"container_id"`
}

func (e *Executor) deleteRuntime(ctx context.Context, task RemoteTask) TaskResult {
	var p deleteRuntimePayload
	if err := unmarshalPayload(task.Payload, &p); err != nil {
		return TaskResult{Success: false, Error: err.Error()}
	}
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", p.ContainerID).CombinedOutput()
	if err != nil {
		return TaskResult{Success: false, RuntimeID: p.RuntimeID,
			Error: fmt.Sprintf("docker rm: %s — %s", err, string(out))}
	}
	return TaskResult{Success: true, RuntimeID: p.RuntimeID, RuntimeState: "deleted"}
}

// ─── WARM_RUNTIME ────────────────────────────────────────────────────────────

func (e *Executor) warmRuntime(ctx context.Context, task RemoteTask) TaskResult {
	var p struct {
		RuntimeID   string `json:"runtime_id"`
		ContainerID string `json:"container_id"`
	}
	if err := unmarshalPayload(task.Payload, &p); err != nil {
		return TaskResult{Success: false, Error: err.Error()}
	}
	out, err := exec.CommandContext(ctx, "docker", "start", p.ContainerID).CombinedOutput()
	if err != nil {
		return TaskResult{Success: false, RuntimeID: p.RuntimeID, RuntimeState: "failed",
			Error: fmt.Sprintf("docker start: %s — %s", err, string(out))}
	}
	return TaskResult{Success: true, RuntimeID: p.RuntimeID, RuntimeState: "loading"}
}

// ─── UNLOAD_RUNTIME ───────────────────────────────────────────────────────────

func (e *Executor) unloadRuntime(ctx context.Context, task RemoteTask) TaskResult {
	// Unload = stop the container but keep the model weights on disk
	return e.stopRuntime(ctx, task)
}

// ─── PULL_MODEL ───────────────────────────────────────────────────────────────

type pullModelPayload struct {
	ModelID   string `json:"model_id"`
	HFRepo    string `json:"hf_repo"`
	HFToken   string `json:"hf_token,omitempty"`
	Backend   string `json:"backend"`
	LocalPath string `json:"local_path,omitempty"`
}

func (e *Executor) pullModel(ctx context.Context, task RemoteTask) TaskResult {
	var p pullModelPayload
	if err := unmarshalPayload(task.Payload, &p); err != nil {
		return TaskResult{Success: false, Error: err.Error()}
	}

	switch p.Backend {
	case "ollama":
		out, err := exec.CommandContext(ctx, "ollama", "pull", p.HFRepo).CombinedOutput()
		if err != nil {
			return TaskResult{Success: false,
				Error: fmt.Sprintf("ollama pull: %s — %s", err, string(out))}
		}
		return TaskResult{Success: true, Data: map[string]interface{}{"pulled": p.HFRepo}}

	case "llamacpp":
		// Use the :full image to download a HuggingFace model, convert it to GGUF,
		// and quantize it — all in one step.
		//
		//   docker run --rm -v <localPath>:/models \
		//     ghcr.io/ggml-org/llama.cpp:full \
		//     --all-in-one "/models/" <hf-repo-or-model-size>
		//
		// After completion the quantized GGUF lands at:
		//   <localPath>/<repo>/ggml-model-q4_0.gguf  (7B default quantization)
		//
		// Alternatively, if the model is already a GGUF on HuggingFace
		// (e.g. a "*-GGUF" repo), skip the conversion step and let the
		// :server image download it at runtime via --hf-repo / --hf-file.
		localPath := p.LocalPath
		if localPath == "" {
			localPath = "/models"
		}

		// Choose image tag: caller may override via HFRepo suffix convention.
		// Default is :full which includes the converter.
		image := "ghcr.io/ggml-org/llama.cpp:full"

		args := []string{"run", "--rm",
			"-v", localPath + ":/models",
		}
		if p.HFToken != "" {
			args = append(args, "-e", "HUGGING_FACE_HUB_TOKEN="+p.HFToken)
		}
		args = append(args, image, "--all-in-one", "/models/", p.HFRepo)

		e.log.Info("converting model to GGUF",
			zap.String("hf_repo", p.HFRepo),
			zap.String("local_path", localPath),
		)

		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			return TaskResult{Success: false,
				Error: fmt.Sprintf("llamacpp convert: %s — %s", err, string(out))}
		}

		// The converted GGUF lands at <localPath>/<model-dir>/ggml-model-q4_0.gguf
		// (for 7B-class models). The exact filename depends on the quantization
		// step inside the :full image — q4_0 is the default.
		modelDir := p.HFRepo
		if strings.Contains(modelDir, "/") {
			// Strip org prefix: "meta-llama/Llama-2-7b" → "Llama-2-7b"
			parts := strings.SplitN(modelDir, "/", 2)
			modelDir = parts[1]
		}
		return TaskResult{Success: true, Data: map[string]interface{}{
			"model_path": localPath + "/" + modelDir + "/ggml-model-q4_0.gguf",
			"note":       "model downloaded and converted to GGUF via llama.cpp :full image",
		}}

	default:
		// For HuggingFace models loaded by vLLM — download happens on runtime start
		return TaskResult{Success: true,
			Data: map[string]interface{}{"model_id": p.ModelID, "note": "model will be downloaded on runtime start"}}
	}
}

// ─── DELETE_MODEL ─────────────────────────────────────────────────────────────

func (e *Executor) deleteModel(ctx context.Context, task RemoteTask) TaskResult {
	var p struct {
		ModelID string `json:"model_id"`
		Backend string `json:"backend"`
		HFRepo  string `json:"hf_repo"`
	}
	if err := unmarshalPayload(task.Payload, &p); err != nil {
		return TaskResult{Success: false, Error: err.Error()}
	}
	if p.Backend == "ollama" {
		out, err := exec.CommandContext(ctx, "ollama", "rm", p.HFRepo).CombinedOutput()
		if err != nil {
			return TaskResult{Success: false,
				Error: fmt.Sprintf("ollama rm: %s — %s", err, string(out))}
		}
	}
	return TaskResult{Success: true}
}

// ─── COLLECT_INVENTORY ────────────────────────────────────────────────────────

func (e *Executor) collectInventory(ctx context.Context) TaskResult {
	// This is handled by the main agent loop — just return success
	return TaskResult{Success: true, Data: map[string]interface{}{"note": "inventory pushed via heartbeat loop"}}
}

// ─── HEALTH_CHECK ─────────────────────────────────────────────────────────────

func (e *Executor) healthCheck(ctx context.Context, task RemoteTask) TaskResult {
	var p struct {
		RuntimeIDs []string `json:"runtime_ids"`
	}
	_ = json.Unmarshal(task.Payload, &p)

	results := make(map[string]string)

	// Check docker containers
	for _, rid := range p.RuntimeIDs {
		// We have the container name or ID — check docker inspect
		out, err := exec.CommandContext(ctx, "docker", "inspect",
			"--format", "{{.State.Status}}", rid).Output()
		if err != nil {
			results[rid] = "not_found"
		} else {
			results[rid] = strings.TrimSpace(string(out))
		}
	}

	return TaskResult{
		Success: true,
		Data:    map[string]interface{}{"runtime_health": results},
	}
}

// isHFLlamaRepo returns true if s looks like a HuggingFace repo ID (e.g. "org/repo").
// Local paths start with "/" and are not HF repos.
func isHFLlamaRepo(s string) bool {
	return len(s) > 0 &&
		strings.Count(s, "/") == 1 &&
		!strings.HasPrefix(s, "/")
}
