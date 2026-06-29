// Package nodeagent — executor.go
//
// The Executor receives START_MODEL (and legacy DEPLOY_RUNTIME / WARM_RUNTIME)
// tasks from the control plane and runs the unified model startup pipeline:
//
//	CREATED → VALIDATING → DOWNLOADING → STARTING →
//	LOADING_MODEL → WAITING_READY → (control plane polls → READY)
//
// Key design rules:
//   - The executor NEVER makes placement or scheduling decisions.
//   - Connection refused during LOADING_MODEL is expected — NOT a failure.
//   - The container is always recreated fresh (docker rm -f + docker run).
//   - All startup triggers (deploy, cold start, re-deploy, recovery, lazy load)
//     execute the same pipeline via startModel().
//
// State transitions reported back to the control plane:
//   - On task claim:     PUT /agent/v1/runtimes/:id  {state: "validating"}
//   - After download:    PUT /agent/v1/runtimes/:id  {state: "starting"}
//   - After docker run:  POST /agent/v1/tasks/:id/complete  {state: "loading_model"}
//   - On failure:        POST /agent/v1/tasks/:id/fail      {state: "failed"}
package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// ─────────────────────────────────────────────────────────────────────────────
// Port management helpers
// ─────────────────────────────────────────────────────────────────────────────

// allocateFreePort asks the OS for a free TCP port by binding to :0.
// The kernel assigns an ephemeral port atomically — no TOCTOU race.
// This is the preferred method when bind_port == 0 in the task payload,
// meaning the control plane delegated port selection to the agent.
func allocateFreePort() (int, error) {
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return 0, fmt.Errorf("allocateFreePort: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	// Brief gap between Close and the container binding — acceptable in practice
	// because the port was ephemeral and no other service is targeting it.
	return port, nil
}

// isPortAvailable returns true when no process is listening on host:port.
func isPortAvailable(port int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// findAvailablePort returns the first free TCP port in [start, start+maxSearch).
func findAvailablePort(start, maxSearch int) (int, bool) {
	for i := 0; i < maxSearch; i++ {
		candidate := start + i
		if candidate > 65535 {
			break
		}
		if isPortAvailable(candidate) {
			return candidate, true
		}
	}
	return 0, false
}

// runningContainerPort returns the host port that a running container named
// `name` is actually bound to, or 0 if the container is not running or has
// no port mapping that can be detected.
//
// For containers started with --network host (llamacpp, vllm, tgi) this
// inspects the command-line arguments inside the container to find the --port
// flag value, since host-networked containers don't show port bindings via
// docker inspect PortBindings.
func (e *Executor) runningContainerPort(ctx context.Context, name string) int {
	// Check container state first — only inspect running containers.
	stateOut, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{.State.Running}}", name).Output()
	if err != nil || strings.TrimSpace(string(stateOut)) != "true" {
		return 0
	}

	// For bridge-networked containers, check PortBindings.
	bindOut, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", `{{range $p, $conf := .HostConfig.PortBindings}}{{if $conf}}{{(index $conf 0).HostPort}}{{end}}{{end}}`,
		name).Output()
	if err == nil {
		if p := strings.TrimSpace(string(bindOut)); p != "" {
			if port, convErr := strconv.Atoi(p); convErr == nil && port > 0 {
				return port
			}
		}
	}

	// For host-networked containers, extract --port from the container args.
	argsOut, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", `{{join .Args " "}}`, name).Output()
	if err != nil {
		return 0
	}
	args := strings.Fields(strings.TrimSpace(string(argsOut)))
	for i, arg := range args {
		if (arg == "--port" || arg == "-p") && i+1 < len(args) {
			if port, convErr := strconv.Atoi(args[i+1]); convErr == nil && port > 0 {
				return port
			}
		}
		// Also handle --port=NNNN form.
		if strings.HasPrefix(arg, "--port=") {
			if port, convErr := strconv.Atoi(strings.TrimPrefix(arg, "--port=")); convErr == nil && port > 0 {
				return port
			}
		}
	}
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// Task types (mirrors taskmanager — no import to keep agent binary lean)
// ─────────────────────────────────────────────────────────────────────────────

type TaskType string

const (
	// TaskStartModel is the single unified startup task.
	// All startup triggers dispatch this task type exclusively.
	TaskStartModel TaskType = "START_MODEL"

	TaskStopRuntime      TaskType = "STOP_RUNTIME"
	TaskUnloadRuntime    TaskType = "UNLOAD_RUNTIME"
	TaskDeleteRuntime    TaskType = "DELETE_RUNTIME"
	TaskPullModel        TaskType = "PULL_MODEL"
	TaskDeleteModel      TaskType = "DELETE_MODEL"
	TaskVerifyModel      TaskType = "VERIFY_MODEL"
	TaskCollectInventory TaskType = "COLLECT_INVENTORY"
	TaskHealthCheck      TaskType = "HEALTH_CHECK"

	// Deprecated: accepted for backward compat with in-flight tasks.
	// All three are routed to startModel().
	TaskDeployRuntime  TaskType = "DEPLOY_RUNTIME"
	TaskWarmRuntime    TaskType = "WARM_RUNTIME"
	TaskRestartRuntime TaskType = "RESTART_RUNTIME"
)

// RemoteTask is a task as received from the control plane API.
type RemoteTask struct {
	ID        string          `json:"id"`
	TaskType  TaskType        `json:"task_type"`
	Payload   json.RawMessage `json:"payload"`
	Priority  int             `json:"priority"`
	CreatedAt time.Time       `json:"created_at"`
}

// TaskResult is reported back to the control plane after execution.
type TaskResult struct {
	Success      bool                   `json:"success"`
	RuntimeID    string                 `json:"runtime_id,omitempty"`
	RuntimeState string                 `json:"runtime_state,omitempty"`
	ContainerID  string                 `json:"container_id,omitempty"`
	Error        string                 `json:"error,omitempty"`
	Data         map[string]interface{} `json:"data,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Executor
// ─────────────────────────────────────────────────────────────────────────────

// Executor runs tasks received from the control plane on the local node.
// Stateless between executions.
type Executor struct {
	log *zap.Logger
}

// NewExecutor constructs an Executor.
func NewExecutor(log *zap.Logger) *Executor {
	return &Executor{log: log}
}

// Execute dispatches a task to the appropriate handler.
func (e *Executor) Execute(ctx context.Context, task RemoteTask) TaskResult {
	e.log.Info("executing task",
		zap.String("task_id", task.ID),
		zap.String("task_type", string(task.TaskType)),
	)

	start := time.Now()
	var result TaskResult

	switch task.TaskType {
	// Unified startup pipeline — all triggers use startModel.
	case TaskStartModel, TaskDeployRuntime, TaskWarmRuntime, TaskRestartRuntime:
		result = e.startModel(ctx, task)
	case TaskStopRuntime:
		result = e.stopRuntime(ctx, task)
	case TaskUnloadRuntime:
		result = e.unloadRuntime(ctx, task)
	case TaskDeleteRuntime:
		result = e.deleteRuntime(ctx, task)
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

// unmarshalPayload handles payloads that may be double-encoded.
func unmarshalPayload(raw json.RawMessage, out interface{}) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err == nil {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("cannot unmarshal payload: %w", err)
	}
	return json.Unmarshal([]byte(s), out)
}

// ─────────────────────────────────────────────────────────────────────────────
// startModel payload — unified struct for START_MODEL and legacy task types
// ─────────────────────────────────────────────────────────────────────────────

// startModelPayload is the unified payload schema accepted by all startup task
// types (START_MODEL, DEPLOY_RUNTIME, WARM_RUNTIME, RESTART_RUNTIME).
// Legacy LlamaCpp-prefixed fields are accepted as fallbacks for backward compat
// with DEPLOY_RUNTIME tasks that may still be in the queue.
type startModelPayload struct {
	RuntimeID      string            `json:"runtime_id"`
	EndpointID     string            `json:"endpoint_id,omitempty"`
	ModelID        string            `json:"model_id"`
	RuntimeName    string            `json:"runtime_name"`
	Backend        string            `json:"backend"`
	Image          string            `json:"image"`
	ModelName      string            `json:"model_name"`
	ServedAs       string            `json:"served_as,omitempty"`
	BindHost       string            `json:"bind_host"`
	BindPort       int               `json:"bind_port"`
	GPUDevices     []int             `json:"gpu_devices"`
	CPUSetCPUs     string            `json:"cpuset_cpus"`
	NUMANode       int               `json:"numa_node"`
	MemoryLimit    string            `json:"memory_limit"`
	CPULimit       string            `json:"cpu_limit"`
	GGUFPath       string            `json:"gguf_path,omitempty"`
	HFRepo         string            `json:"hf_repo,omitempty"`
	HFFile         string            `json:"hf_file,omitempty"`
	HFToken        string            `json:"hf_token,omitempty"`
	ModelsVolume   string            `json:"models_volume,omitempty"`
	CtxSize        int               `json:"ctx_size"`
	NGPULayers     int               `json:"n_gpu_layers"`
	TensorParallel int               `json:"tensor_parallel,omitempty"`
	GPUMemoryUtil  float64           `json:"gpu_memory_util,omitempty"`
	MaxModelLen    int               `json:"max_model_len,omitempty"`
	Dtype          string            `json:"dtype,omitempty"`
	Quantization   string            `json:"quantization,omitempty"`
	ExtraArgs      []string          `json:"extra_args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	// ExecutionMode is resolved by the control plane before dispatch.
	//   "cpu" — never request --gpus; n_gpu_layers is 0
	//   "gpu" — always request --gpus
	//   "auto" or "" — legacy: agent decides from GPUDevices/NGPULayers
	ExecutionMode string `json:"execution_mode,omitempty"`
	// Legacy DEPLOY_RUNTIME aliases
	LlamaCppModelPath    string `json:"llamacpp_model_path,omitempty"`
	LlamaCppHFRepo       string `json:"llamacpp_hf_repo,omitempty"`
	LlamaCppHFFile       string `json:"llamacpp_hf_file,omitempty"`
	LlamaCppCtxSize      int    `json:"llamacpp_ctx_size,omitempty"`
	LlamaCppNGPULayers   int    `json:"llamacpp_n_gpu_layers,omitempty"`
	LlamaCppModelsVolume string `json:"llamacpp_models_volume,omitempty"`
}

// resolveModelSource normalises legacy DEPLOY_RUNTIME fields into the canonical
// GGUFPath / HFRepo / HFFile / ModelsVolume fields.
func (p *startModelPayload) resolveModelSource() {
	if p.GGUFPath == "" && p.LlamaCppModelPath != "" {
		p.GGUFPath = p.LlamaCppModelPath
	}
	if p.HFRepo == "" && p.LlamaCppHFRepo != "" {
		p.HFRepo = p.LlamaCppHFRepo
	}
	if p.HFFile == "" && p.LlamaCppHFFile != "" {
		p.HFFile = p.LlamaCppHFFile
	}
	if p.ModelsVolume == "" && p.LlamaCppModelsVolume != "" {
		p.ModelsVolume = p.LlamaCppModelsVolume
	}
	if p.CtxSize == 0 && p.LlamaCppCtxSize != 0 {
		p.CtxSize = p.LlamaCppCtxSize
	}
	if p.NGPULayers == 0 && p.LlamaCppNGPULayers != 0 {
		p.NGPULayers = p.LlamaCppNGPULayers
	}
	if p.ServedAs == "" {
		p.ServedAs = p.ModelName
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ─────────────────────────────────────────────────────────────────────────────
// startModel — the single unified startup pipeline
// ─────────────────────────────────────────────────────────────────────────────

// startModel executes the full startup pipeline for ALL model start scenarios:
// initial deploy, cold start, lazy load, re-deploy, crash recovery.
//
// Pipeline stages and state transitions reported to the control plane:
//
//	CREATED      (row exists before this function is called)
//	VALIDATING   ← reported on task claim (see agent.go executeTask)
//	DOWNLOADING  ← reported if model weights are missing and need downloading
//	STARTING     ← reported after download, before docker run
//	LOADING_MODEL ← returned as the final task result (container running)
//	WAITING_READY  (control plane polls /health, transitions to READY)
//
// Connection refused during LOADING_MODEL is EXPECTED.  It is NOT a failure.
// Failure only occurs if:
//   - Required fields (runtime_name, image, bind_port) are missing.
//   - Model source validation fails.
//   - docker run exits with an error.
//   - Ollama model pull fails.
func (e *Executor) startModel(ctx context.Context, task RemoteTask) TaskResult {
	var p startModelPayload
	if err := unmarshalPayload(task.Payload, &p); err != nil {
		return TaskResult{Success: false, Error: "parse payload: " + err.Error()}
	}

	// Normalise legacy field aliases.
	p.resolveModelSource()

	// ── Stage: VALIDATING ────────────────────────────────────────────────────
	// Validate all required fields before touching Docker.
	if p.RuntimeName == "" {
		return TaskResult{
			Success: false, RuntimeID: p.RuntimeID, RuntimeState: "failed",
			Error: "validation failed: runtime_name is required",
		}
	}
	if p.Image == "" {
		return TaskResult{
			Success: false, RuntimeID: p.RuntimeID, RuntimeState: "failed",
			Error: "validation failed: container image is required",
		}
	}
	if p.BindPort < 0 {
		return TaskResult{
			Success: false, RuntimeID: p.RuntimeID, RuntimeState: "failed",
			Error: "validation failed: bind_port must be 0 (auto) or a positive port number",
		}
	}
	if p.Backend == "" {
		p.Backend = "llamacpp" // backward-compat default
	}
	if p.CtxSize == 0 {
		p.CtxSize = 4096
	}

	// Prepare env map.
	if p.Env == nil {
		p.Env = make(map[string]string)
	}
	if p.HFToken != "" {
		p.Env["HUGGING_FACE_HUB_TOKEN"] = p.HFToken
	}

	e.log.Info("startModel: VALIDATING",
		zap.String("runtime", p.RuntimeName),
		zap.String("backend", p.Backend),
		zap.String("image", p.Image),
		zap.Int("port", p.BindPort),
	)

	// ── Stage: DOWNLOADING ──────────────────────────────────────────────────
	// For llamacpp: verify model in shared cache; download if missing.
	// If only HFRepo+HFFile are set, llama-server downloads at startup — no
	// pre-download step is needed (and connection refused is not a failure).
	if p.Backend == "llamacpp" {
		if result := e.ensureModelCached(ctx, &p); !result.Success {
			return TaskResult{
				Success: false, RuntimeID: p.RuntimeID, RuntimeState: "failed",
				Error: "downloading/validation failed: " + result.Error,
			}
		}
	}

	// ── Stage: STARTING ─────────────────────────────────────────────────────
	// Port resolution — three cases handled in priority order:
	//
	//   Case A: bind_port == 0  → control plane delegated port selection to the
	//           agent. Use allocateFreePort() which asks the OS for a free port
	//           atomically via :0 bind. This is the preferred production path.
	//
	//   Case B: bind_port > 0 AND container already running on that port →
	//           reuse the existing instance (avoids duplicate containers).
	//
	//   Case C: bind_port > 0 AND port is occupied by something else →
	//           scan forward up to 50 ports. This is a recovery fallback for
	//           legacy deployments that had a fixed port configured.

	// Case A — zero means "agent picks"
	if p.BindPort == 0 {
		freePort, allocErr := allocateFreePort()
		if allocErr != nil {
			return TaskResult{
				Success:      false,
				RuntimeID:    p.RuntimeID,
				RuntimeState: "failed",
				Error:        "port allocation failed: " + allocErr.Error(),
			}
		}
		p.BindPort = freePort
		e.log.Info("startModel: OS-allocated free port",
			zap.String("runtime", p.RuntimeName),
			zap.Int("port", p.BindPort),
		)
	} else {
		// Case B — check if the exact container is already running on this port
		if existingPort := e.runningContainerPort(ctx, p.RuntimeName); existingPort > 0 {
			cl := &net.Dialer{Timeout: 2 * time.Second}
			if conn, dialErr := cl.DialContext(ctx, "tcp", fmt.Sprintf("localhost:%d", existingPort)); dialErr == nil {
				_ = conn.Close()
				e.log.Info("startModel: REUSING existing running container",
					zap.String("runtime", p.RuntimeName),
					zap.Int("port", existingPort),
				)
				cidOut, _ := exec.CommandContext(ctx, "docker", "inspect",
					"--format", "{{.Id}}", p.RuntimeName).Output()
				containerID := strings.TrimSpace(string(cidOut))
				return TaskResult{
					Success:      true,
					RuntimeID:    p.RuntimeID,
					ContainerID:  containerID,
					RuntimeState: "loading_model",
					Data: map[string]interface{}{
						"container_id": containerID,
						"bind_port":    existingPort,
						"reused":       true,
					},
				}
			}
			e.log.Info("startModel: container exists but port not reachable — will recreate",
				zap.String("runtime", p.RuntimeName),
				zap.Int("port", existingPort),
			)
		}

		// Case C — configured port is busy, scan forward
		if !isPortAvailable(p.BindPort) {
			e.log.Warn("startModel: configured port busy, scanning for free port",
				zap.String("runtime", p.RuntimeName),
				zap.Int("configured_port", p.BindPort),
			)
			if freePort, found := findAvailablePort(p.BindPort+1, 50); found {
				e.log.Info("startModel: found free port via scan",
					zap.String("runtime", p.RuntimeName),
					zap.Int("original_port", p.BindPort),
					zap.Int("actual_port", freePort),
				)
				p.BindPort = freePort
			} else {
				return TaskResult{
					Success:      false,
					RuntimeID:    p.RuntimeID,
					RuntimeState: "failed",
					Error: fmt.Sprintf(
						"port %d is busy and no free port found scanning %d–%d",
						p.BindPort, p.BindPort+1, p.BindPort+50,
					),
				}
			}
		}
	}

	// Remove any stale container with this name, then start fresh.
	// This covers: first deploy, re-deploy, crash recovery, idle restart.
	if out, rmErr := exec.CommandContext(ctx, "docker", "rm", "-f", p.RuntimeName).CombinedOutput(); rmErr != nil {
		if !strings.Contains(string(out), "No such container") {
			e.log.Warn("docker rm pre-flight warning",
				zap.String("name", p.RuntimeName),
				zap.String("output", string(out)),
			)
		}
	}

	args := e.buildDockerArgs(p)

	e.log.Info("startModel: STARTING container",
		zap.String("runtime", p.RuntimeName),
		zap.String("backend", p.Backend),
		zap.String("image", p.Image),
		zap.Ints("gpu_devices", p.GPUDevices),
		zap.Int("port", p.BindPort),
		zap.String("model_source", func() string {
			if p.GGUFPath != "" {
				return "local:" + p.GGUFPath
			}
			if p.HFRepo != "" {
				return "hf:" + p.HFRepo + "/" + p.HFFile
			}
			return "unknown"
		}()),
	)

	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return TaskResult{
			Success:      false,
			RuntimeID:    p.RuntimeID,
			RuntimeState: "failed",
			Error:        fmt.Sprintf("docker run failed: %s — output: %s", err.Error(), string(out)),
		}
	}

	containerID := strings.TrimSpace(string(out))

	// ── Stage: LOADING_MODEL ────────────────────────────────────────────────
	// Container is running; model is loading into RAM/VRAM.
	// The HTTP port is NOT yet accepting connections — connection refused is
	// expected here and must NOT be treated as failure.
	// We report "loading_model" to the control plane; it polls /health
	// independently and transitions the row to "ready" when health passes.
	e.log.Info("startModel: LOADING_MODEL — container started, model loading",
		zap.String("container_id", containerID),
		zap.Int("port", p.BindPort),
	)

	// For Ollama: pull the model image after the daemon starts.
	// The 8-second sleep is the minimum time for the Ollama daemon to become ready.
	if p.Backend == "ollama" && p.ServedAs != "" {
		time.Sleep(8 * time.Second)
		pullOut, pullErr := exec.CommandContext(ctx, "docker", "exec", containerID,
			"ollama", "pull", p.ServedAs).CombinedOutput()
		if pullErr != nil {
			return TaskResult{
				Success:      false,
				RuntimeID:    p.RuntimeID,
				ContainerID:  containerID,
				RuntimeState: "failed",
				Error:        fmt.Sprintf("ollama pull %q: %s — %s", p.ServedAs, pullErr.Error(), string(pullOut)),
			}
		}
	}

	// Return "loading_model" — the control plane's waitForReady() polls /health
	// and transitions to "ready" when all four readiness conditions are satisfied.
	return TaskResult{
		Success:      true,
		RuntimeID:    p.RuntimeID,
		ContainerID:  containerID,
		RuntimeState: "loading_model",
		Data: map[string]interface{}{
			"container_id": containerID,
			"bind_port":    p.BindPort,
			// Report the resolved gguf_path so the control plane can persist it
			// in model_runtime_configs — avoids re-download on next container start.
			"gguf_path": p.GGUFPath,
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ensureModelCached — DOWNLOADING stage for llamacpp
// ─────────────────────────────────────────────────────────────────────────────

// ensureModelCached verifies the model file is present in the shared cache
// volume and downloads it if missing.
//
// Cases:
//  1. GGUFPath set + file exists → reuse; skip download.
//  2. GGUFPath set + file missing + HFRepo set → download then start.
//  3. Only HFRepo+HFFile set, no GGUFPath → check if HFFile already cached in
//     the volume. If yes: set GGUFPath to the local path (avoids HF startup
//     download). If no: llama-server downloads at startup via --hf-repo/--hf-file.
//  4. No model source at all → fail fast with a clear error.
func (e *Executor) ensureModelCached(ctx context.Context, p *startModelPayload) TaskResult {
	// Case 4: no usable source.
	if p.GGUFPath == "" && p.HFRepo == "" && !strings.HasPrefix(p.ModelName, "/") {
		return TaskResult{
			Success: false,
			Error: fmt.Sprintf(
				"llamacpp requires a model source: set gguf_path (local GGUF path), "+
					"hf_repo+hf_file (HuggingFace download), or an absolute path in model_name. "+
					"Got bare name %q which is not a valid GGUF path.", p.ModelName),
		}
	}

	// Resolve the host-side directory for the shared models volume so we can
	// check whether the file already exists on disk.
	hostVolumeRoot := firstNonEmpty(p.ModelsVolume, "llamacpp_models")
	hostVolumeDir := hostVolumeRoot
	if !strings.HasPrefix(hostVolumeRoot, "/") {
		volOut, volErr := exec.CommandContext(ctx, "docker", "volume", "inspect",
			"--format", "{{.Mountpoint}}", hostVolumeRoot).Output()
		if volErr == nil {
			hostVolumeDir = strings.TrimSpace(string(volOut))
		}
		// If the volume doesn't exist yet, hostVolumeDir stays as the volume name.
		// We'll fall through and let docker run create it.
	}

	// Case 3: no explicit GGUFPath but HFRepo+HFFile provided.
	// Check if the HFFile is already cached in the volume. If it is, reuse it
	// as a local path — this avoids a slow in-container HF download and fixes
	// the health check timeout issue (server doesn't open the port during download).
	if p.GGUFPath == "" && p.HFRepo != "" && p.HFFile != "" {
		// Try the flat file name first (e.g. gemma-2-2b-it-Q4_K_M.gguf at root of volume)
		candidatePaths := []string{
			hostVolumeDir + "/" + p.HFFile,
			// Also try repo-named subdir: bartowski/gemma-2-2b-it-GGUF/file.gguf
			hostVolumeDir + "/" + p.HFRepo + "/" + p.HFFile,
		}
		for _, candidate := range candidatePaths {
			statOut, statErr := exec.CommandContext(ctx, "stat", "-c", "%s", candidate).Output()
			if statErr != nil {
				continue // file doesn't exist
			}
			sizeStr := strings.TrimSpace(string(statOut))
			if sizeStr == "0" || sizeStr == "" {
				// Zero-byte placeholder — remove it, don't treat as cached
				_, _ = exec.CommandContext(ctx, "rm", "-f", candidate).CombinedOutput()
				e.log.Warn("found zero-byte cached file — removing",
					zap.String("path", candidate),
				)
				continue
			}
			// File found and non-empty — switch to local mode.
			relToVolume := strings.TrimPrefix(candidate, hostVolumeDir+"/")
			p.GGUFPath = "/models/" + relToVolume
			e.log.Info("model file already cached — using local path instead of HF download",
				zap.String("host_path", candidate),
				zap.String("container_path", p.GGUFPath),
				zap.String("size", sizeStr),
			)
			// Clear HF fields so buildDockerArgs uses -m, not --hf-repo.
			p.HFRepo = ""
			p.HFFile = ""
			return TaskResult{Success: true}
		}
		// File not cached — fall through to let llama-server download at startup.
		e.log.Info("model file not in cache — llama-server will download via HF",
			zap.String("hf_repo", p.HFRepo),
			zap.String("hf_file", p.HFFile),
		)
		return TaskResult{Success: true}
	}

	if p.GGUFPath == "" {
		// HFRepo set but no HFFile (unusual) — let llama-server handle it.
		return TaskResult{Success: true}
	}

	// Cases 1 & 2: GGUFPath explicitly set.
	// Map container path → host path.
	relPath := strings.TrimPrefix(p.GGUFPath, "/models/")
	if relPath == p.GGUFPath {
		relPath = strings.TrimPrefix(p.GGUFPath, "/")
	}
	hostFilePath := hostVolumeDir + "/" + relPath

	// Case 1: file already cached — verify it exists AND is non-empty.
	// A zero-byte file means a previous download was interrupted; treat it as missing.
	statOut, statErr := exec.CommandContext(ctx, "stat", "-c", "%s", hostFilePath).Output()
	if statErr == nil {
		sizeStr := strings.TrimSpace(string(statOut))
		if sizeStr != "0" && sizeStr != "" {
			e.log.Info("model already in cache — skipping download",
				zap.String("path", hostFilePath),
				zap.String("size", sizeStr),
			)
			return TaskResult{Success: true}
		}
		// Zero-byte or unreadable — remove and re-download.
		e.log.Warn("cached file is zero-byte or unreadable — removing and re-downloading",
			zap.String("path", hostFilePath),
		)
		_, _ = exec.CommandContext(ctx, "rm", "-f", hostFilePath).CombinedOutput()
	}

	// Case 2: file missing — download.
	e.log.Info("startModel: DOWNLOADING model",
		zap.String("host_path", hostFilePath),
		zap.String("hf_repo", p.HFRepo),
		zap.String("hf_file", p.HFFile),
	)

	if p.HFRepo == "" {
		return TaskResult{
			Success: false,
			Error: fmt.Sprintf(
				"GGUF file not found at %s and no hf_repo configured for auto-download. "+
					"Set hf_repo+hf_file or pre-populate the models volume manually.", p.GGUFPath),
		}
	}

	result, actualHostPath := e.downloadFromHF(ctx, p, hostVolumeDir, relPath)
	if !result.Success {
		return result
	}

	// Remap container path to the actual downloaded filename.
	relActual := strings.TrimPrefix(actualHostPath, hostVolumeDir+"/")
	p.GGUFPath = "/models/" + relActual
	e.log.Info("resolved GGUF container path after download",
		zap.String("container_path", p.GGUFPath),
	)
	return result
}

// downloadFromHF downloads a GGUF file from HuggingFace into the host volume.
// Uses huggingface-cli if available, then aria2c (multi-connection), then wget.
// Returns (TaskResult, actualHostPath).
func (e *Executor) downloadFromHF(ctx context.Context, p *startModelPayload, hostVolumeDir, relPath string) (TaskResult, string) {
	targetDir := hostVolumeDir
	if idx := strings.LastIndex(relPath, "/"); idx >= 0 {
		targetDir = hostVolumeDir + "/" + relPath[:idx]
	}
	if out, err := exec.CommandContext(ctx, "mkdir", "-p", targetDir).CombinedOutput(); err != nil {
		return TaskResult{Success: false,
			Error: fmt.Sprintf("mkdir %s: %s — %s", targetDir, err, string(out))}, ""
	}

	// ── Option 1: huggingface-cli ─────────────────────────────────────────
	if path, err := exec.LookPath("huggingface-cli"); err == nil {
		e.log.Info("downloading via huggingface-cli",
			zap.String("hf_repo", p.HFRepo),
			zap.String("hf_file", p.HFFile),
			zap.String("target_dir", targetDir),
			zap.String("tool", path),
		)
		args := []string{"download", p.HFRepo}
		if p.HFFile != "" {
			args = append(args, p.HFFile)
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
		return TaskResult{Success: true}, targetDir + "/" + p.HFFile
	}

	// ── Option 2: aria2c (multi-connection, faster than wget) ────────────
	if p.HFFile != "" {
		if _, err := exec.LookPath("aria2c"); err == nil {
			url := fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s", p.HFRepo, p.HFFile)
			targetFile := targetDir + "/" + p.HFFile
			aria2Args := []string{
				"--continue=true",
				"--max-connection-per-server=8",
				"--split=8",
				"--min-split-size=50M",
				"--dir", targetDir,
				"--out", p.HFFile,
			}
			if p.HFToken != "" {
				aria2Args = append(aria2Args, "--header=Authorization: Bearer "+p.HFToken)
			}
			aria2Args = append(aria2Args, url)
			e.log.Info("downloading via aria2c",
				zap.String("url", url),
				zap.String("target", targetFile),
			)
			out, err := exec.CommandContext(ctx, "aria2c", aria2Args...).CombinedOutput()
			if err != nil {
				return TaskResult{Success: false,
					Error: fmt.Sprintf("aria2c download: %s — %s", err, string(out))}, ""
			}
			return TaskResult{Success: true}, targetFile
		}
	}

	// ── Option 3: wget fallback ───────────────────────────────────────────
	if p.HFFile == "" {
		return TaskResult{Success: false,
			Error: "huggingface-cli and aria2c not found and no hf_file set for wget fallback"}, ""
	}
	url := fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s", p.HFRepo, p.HFFile)
	targetFile := targetDir + "/" + p.HFFile
	wgetArgs := []string{"-c", "-O", targetFile, url}
	if p.HFToken != "" {
		wgetArgs = append([]string{"--header", "Authorization: Bearer " + p.HFToken}, wgetArgs...)
	}
	e.log.Info("downloading via wget",
		zap.String("url", url),
		zap.String("target", targetFile),
	)
	out, err := exec.CommandContext(ctx, "wget", wgetArgs...).CombinedOutput()
	if err != nil {
		return TaskResult{Success: false,
			Error: fmt.Sprintf("wget download: %s — %s", err, string(out))}, ""
	}
	return TaskResult{Success: true}, targetFile
}

// ─────────────────────────────────────────────────────────────────────────────
// buildDockerArgs — constructs `docker run` arguments from startModelPayload
// ─────────────────────────────────────────────────────────────────────────────

func (e *Executor) buildDockerArgs(p startModelPayload) []string {
	args := []string{"run", "-d", "--name", p.RuntimeName, "--restart", "unless-stopped"}

	switch p.Backend {
	case "ollama":
		host := p.BindHost
		if host == "" {
			host = "127.0.0.1"
		}
		args = append(args, "-p", fmt.Sprintf("%s:%d:11434", host, p.BindPort))
		args = append(args, "-v", "ollama_models:/root/.ollama")

	case "llamacpp":
		vol := firstNonEmpty(p.ModelsVolume, "llamacpp_models")
		args = append(args, "-v", vol+":/models")
		// llamacpp uses host networking for simplicity.
		args = append(args, "--network", "host")
		// Override baked-in HEALTHCHECK to use the actual port.
		healthURL := fmt.Sprintf("http://localhost:%d/health", p.BindPort)
		args = append(args,
			"--health-cmd", fmt.Sprintf("curl -sf %s || exit 1", healthURL),
			"--health-interval", "15s",
			"--health-timeout", "5s",
			"--health-retries", "3",
			"--health-start-period", "30s",
		)

	default:
		// vllm, tgi, cpu_native — host networking.
		args = append(args, "--network", "host")
	}

	// ── GPU assignment — controlled exclusively by ExecutionMode ─────────────
	// "cpu":       never add --gpus (CPU-only deployment)
	// "gpu":       always add --gpus using GPUDevices list or --gpus all
	// "auto" / "": legacy behaviour — derive from GPUDevices and NGPULayers
	//
	// The control plane resolves "auto" → "cpu"|"gpu" before dispatch, so the
	// agent should always receive "cpu" or "gpu" in practice. The "auto" branch
	// is kept here only as a safe fallback for in-flight legacy tasks.
	wantsGPU := false
	switch p.ExecutionMode {
	case "cpu":
		wantsGPU = false
	case "gpu":
		wantsGPU = true
	default: // "auto" or empty — legacy: infer from payload fields
		wantsGPU = len(p.GPUDevices) > 0 ||
			(p.Backend == "llamacpp" && p.NGPULayers != 0)
	}

	if wantsGPU {
		if len(p.GPUDevices) > 0 {
			devList := make([]string, len(p.GPUDevices))
			for i, idx := range p.GPUDevices {
				devList[i] = strconv.Itoa(idx)
			}
			args = append(args, "--gpus", "device="+strings.Join(devList, ","))
		} else {
			args = append(args, "--gpus", "all")
		}
	}
	// wantsGPU == false → no --gpus flag at all; container runs on CPU

	// CPU affinity.
	if p.CPUSetCPUs != "" {
		args = append(args, "--cpuset-cpus", p.CPUSetCPUs)
	}
	if p.NUMANode >= 0 {
		args = append(args, "--cpuset-mems", strconv.Itoa(p.NUMANode))
	}

	// Resource limits.
	if p.CPULimit != "" {
		args = append(args, "--cpus", p.CPULimit)
	}
	if p.MemoryLimit != "" {
		args = append(args, "--memory", p.MemoryLimit)
	}

	// Environment.
	for k, v := range p.Env {
		args = append(args, "-e", k+"="+v)
	}

	// Image.
	args = append(args, p.Image)

	// Backend-specific command args.
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
		// Model source priority: GGUFPath > HFRepo+HFFile > HFRepo alone > ModelName as path.
		switch {
		case p.GGUFPath != "":
			args = append(args, "--model", p.GGUFPath)
		case p.HFRepo != "" && p.HFFile != "":
			args = append(args, "--hf-repo", p.HFRepo, "--hf-file", p.HFFile)
		case p.HFRepo != "":
			args = append(args, "--hf-repo", p.HFRepo)
		case strings.HasPrefix(p.ModelName, "/"):
			args = append(args, "--model", p.ModelName)
		}
		args = append(args,
			"--host", "0.0.0.0",
			"--port", strconv.Itoa(p.BindPort),
			"--ctx-size", strconv.Itoa(p.CtxSize),
		)
		if p.CPULimit != "" && p.CPULimit != "0" {
			args = append(args, "--threads", p.CPULimit)
		}
		// n-gpu-layers: only add when actually running on GPU.
		// wantsGPU is determined above from ExecutionMode.
		if wantsGPU {
			if len(p.GPUDevices) > 0 && p.NGPULayers == 0 {
				// GPU devices assigned but no explicit layer count → offload all.
				args = append(args, "--n-gpu-layers", "-1")
			} else if p.NGPULayers != 0 {
				args = append(args, "--n-gpu-layers", strconv.Itoa(p.NGPULayers))
			} else {
				// GPU mode but NGPULayers == 0 and no device list — default all.
				args = append(args, "--n-gpu-layers", "-1")
			}
		}
		// CPU mode: omit --n-gpu-layers entirely so llama-server uses CPU only.
	}

	args = append(args, p.ExtraArgs...)
	return args
}

// ─────────────────────────────────────────────────────────────────────────────
// STOP_RUNTIME / UNLOAD_RUNTIME / DELETE_RUNTIME
// ─────────────────────────────────────────────────────────────────────────────

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

func (e *Executor) unloadRuntime(ctx context.Context, task RemoteTask) TaskResult {
	// Unload == stop container; model weights remain on volume.
	return e.stopRuntime(ctx, task)
}

func (e *Executor) deleteRuntime(ctx context.Context, task RemoteTask) TaskResult {
	var p struct {
		RuntimeID   string `json:"runtime_id"`
		ContainerID string `json:"container_id"`
	}
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

// ─────────────────────────────────────────────────────────────────────────────
// PULL_MODEL / DELETE_MODEL / COLLECT_INVENTORY / HEALTH_CHECK
// ─────────────────────────────────────────────────────────────────────────────

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
		localPath := firstNonEmpty(p.LocalPath, "/models")
		args := []string{"run", "--rm", "-v", localPath + ":/models"}
		if p.HFToken != "" {
			args = append(args, "-e", "HUGGING_FACE_HUB_TOKEN="+p.HFToken)
		}
		args = append(args, "ghcr.io/ggml-org/llama.cpp:full", "--all-in-one", "/models/", p.HFRepo)
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			return TaskResult{Success: false,
				Error: fmt.Sprintf("llamacpp convert: %s — %s", err, string(out))}
		}
		modelDir := p.HFRepo
		if strings.Contains(modelDir, "/") {
			parts := strings.SplitN(modelDir, "/", 2)
			modelDir = parts[1]
		}
		return TaskResult{Success: true, Data: map[string]interface{}{
			"model_path": localPath + "/" + modelDir + "/ggml-model-q4_0.gguf",
		}}
	default:
		return TaskResult{Success: true,
			Data: map[string]interface{}{"note": "model will be downloaded on container start"}}
	}
}

func (e *Executor) deleteModel(ctx context.Context, task RemoteTask) TaskResult {
	var p struct {
		HFRepo  string `json:"hf_repo"`
		Backend string `json:"backend"`
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

func (e *Executor) collectInventory(ctx context.Context) TaskResult {
	return TaskResult{Success: true, Data: map[string]interface{}{
		"note": "inventory pushed via heartbeat loop",
	}}
}

func (e *Executor) healthCheck(ctx context.Context, task RemoteTask) TaskResult {
	var p struct {
		RuntimeIDs []string `json:"runtime_ids"`
	}
	_ = json.Unmarshal(task.Payload, &p)
	results := make(map[string]string)
	for _, rid := range p.RuntimeIDs {
		out, err := exec.CommandContext(ctx, "docker", "inspect",
			"--format", "{{.State.Status}}", rid).Output()
		if err != nil {
			results[rid] = "not_found"
		} else {
			results[rid] = strings.TrimSpace(string(out))
		}
	}
	return TaskResult{Success: true, Data: map[string]interface{}{"runtime_health": results}}
}
