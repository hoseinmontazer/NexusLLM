// Package runtimemgr implements the lazy-load runtime management architecture.
//
// Core idea: model containers are started on first request, kept running while
// in use, and automatically stopped after a configurable idle timeout.
// All llama.cpp containers on a node share a single /models volume — GGUF
// files are downloaded once and never duplicated per container.
//
// Unified startup pipeline:
//
//	Every trigger (initial deploy, cold start, lazy load, re-deploy, crash
//	recovery, always-running restore) invokes the same StartModel() workflow,
//	which progresses through the following states:
//
//	  CREATED → VALIDATING → DOWNLOADING → STARTING →
//	  LOADING_MODEL → WAITING_READY → READY
//
// A model is only READY when ALL four conditions hold simultaneously:
//   - Container is running (docker inspect shows "running")
//   - Health endpoint responds with HTTP 200
//   - Runtime reports model loaded (no "loading model" in logs)
//   - Endpoint is routable (registry resolves the model name)
//
// Connection refused during startup is explicitly NOT a failure — it is the
// expected state while the model is loading into memory.
//
// Failure triggers:
//   - Container exits unexpectedly (non-zero exit code / docker inspect "exited")
//   - Startup timeout exceeded (ColdStartTimeout)
//   - Fatal runtime error detected in container logs
//   - Validation fails (missing image, port, model source)
package runtimemgr

import "time"

// ─────────────────────────────────────────────────────────────────────────────
// Startup pipeline states — stored in agent_runtimes.state
// ─────────────────────────────────────────────────────────────────────────────

// State is the model lifecycle state tracked in agent_runtimes.state.
// These are the canonical values — the DB column stores them as lowercase strings.
// All startup paths (deploy, cold start, re-deploy, recovery, lazy load) use
// the same state machine.
type State string

const (
	// ── Startup pipeline ─────────────────────────────────────────────────────

	// StateCreated — runtime row inserted; START_MODEL task enqueued but not yet
	// claimed by the node agent.
	StateCreated State = "created"

	// StateValidating — agent claimed the task; validating model config, runtime
	// config, container image existence, and volume mounts.
	StateValidating State = "validating"

	// StateDownloading — GGUF or model weights not found in shared cache;
	// downloading from HuggingFace or other source.
	StateDownloading State = "downloading"

	// StateStarting — validation passed; removing any stale container and running
	// docker run with the full configuration.
	StateStarting State = "starting"

	// StateLoadingModel — container is running; model weights are being loaded
	// into RAM/VRAM.  The HTTP port is not yet accepting connections.
	// Connection refused at this stage is EXPECTED and must not be treated as failure.
	StateLoadingModel State = "loading_model"

	// StateWaitingReady — model is loaded; waiting for the health endpoint to
	// return HTTP 200 and for the registry to confirm the endpoint is routable.
	StateWaitingReady State = "waiting_ready"

	// StateReady — all four readiness conditions are satisfied:
	//   1. Container is running.
	//   2. Health endpoint responds HTTP 200.
	//   3. Runtime reports model loaded.
	//   4. Endpoint is routable in the registry.
	StateReady State = "ready"

	// ── Operational states ────────────────────────────────────────────────────

	// StateIdle — READY but no traffic for IdleTimeout; will be unloaded soon.
	StateIdle State = "idle"

	// StateStopping — UNLOAD_RUNTIME or STOP_RUNTIME task dispatched; container
	// is draining / stopping.
	StateStopping State = "stopping"

	// StateStopped — container was cleanly stopped (idle eviction or explicit stop).
	// Model weights remain on the volume.  The next StartModel() call recreates
	// the container without re-downloading.
	StateStopped State = "stopped"

	// StateFailed — terminal failure: container exited unexpectedly, startup
	// timeout exceeded, or fatal runtime error detected in logs.
	StateFailed State = "failed"

	// StateLost — node went offline while container was running.  Transitions to
	// StateCreated when the node comes back online and StartModel() is retried.
	StateLost State = "lost"

	// ── Internal / transitional (not written to DB) ────────────────────────────

	// StateUnknown — no agent_runtimes row found; treated as StateCreated for
	// routing purposes.
	StateUnknown State = "unknown"

	// StateNotRegistered — alias for StateUnknown used in older code paths.
	// Deprecated: use StateUnknown.
	StateNotRegistered State = "not_registered"
)

// ─────────────────────────────────────────────────────────────────────────────
// Configuration types
// ─────────────────────────────────────────────────────────────────────────────

// ModelConfig is the complete runtime configuration for a model.
// Stored in model_runtime_configs (and derived from model_endpoints / agent_runtimes).
// Used by the unified StartModel pipeline — every startup trigger reads from
// this struct, so it must carry every field needed for any backend.
type ModelConfig struct {
	// ── Routing identity ──────────────────────────────────────────────────
	ModelName string // short routing name clients use: "gemma2-2b"
	ModelID   string // models.id UUID

	// ── Backend ───────────────────────────────────────────────────────────
	// Matches models.backend_type: llamacpp | vllm | ollama | tgi | cpu_native.
	Backend string

	// ── Execution mode ────────────────────────────────────────────────────
	// ExecutionMode is read from model_runtime_configs.execution_mode.
	//   "cpu"  — CPU-only; GPUDevices is ignored; n_gpu_layers forced to 0
	//   "gpu"  — require GPU; fail placement if no GPU available
	//   "auto" — resolve at dispatch: use GPU if node has one, else CPU
	// Defaults to "auto" if unset.
	ExecutionMode string

	// WorkloadPolicy controls lifecycle management.
	//   "lazy_load" — start on first request, evict when idle (default for LLMs)
	//   "always_on" — start on deploy, never idle-evict, restart on crash (services)
	WorkloadPolicy string

	// ── llamacpp model source (first non-empty wins) ──────────────────────
	GGUFPath string // container path: "/models/gemma-2-2b-it-Q4_K_M.gguf"
	HFRepo   string // "bartowski/gemma-2-2b-it-GGUF"
	HFFile   string // "gemma-2-2b-it-Q4_K_M.gguf"
	HFToken  string // for gated repos

	// ── Container ─────────────────────────────────────────────────────────
	Image        string // full Docker image reference
	BindPort     int    // unique port per model on this node
	CtxSize      int    // --ctx-size (default: 4096)
	NGPULayers   int    // -1 = all GPU, 0 = CPU-only
	CPUThreads   string // "--threads N" (empty = auto)
	MemoryLimit  string // docker --memory e.g. "8g"
	ModelsVolume string // named volume or absolute host path

	// ── vLLM / TGI / generic backend settings ────────────────────────────
	TensorParallel int
	GPUMemoryUtil  float64
	MaxModelLen    int
	Dtype          string
	Quantization   string
	ExtraArgs      []string

	// ── Placement ─────────────────────────────────────────────────────────
	NodeID     string
	BindHost   string // bind address for the container (node's IP)
	GPUDevices []int  // [] = CPU-only
	CPUSetCPUs string // e.g. "0-31"
	NUMANode   int    // -1 = no affinity

	// ── Idle behaviour ────────────────────────────────────────────────────
	IdleTimeout time.Duration // 0 = use cluster default from Config
}

// Config is the cluster-wide configuration for the runtime manager.
// Loaded from environment / config file at startup.
type Config struct {
	// DefaultIdleTimeout is how long a model container stays running with no
	// traffic before being stopped. Individual models can override this.
	DefaultIdleTimeout time.Duration // default: 15 minutes

	// ColdStartTimeout is the maximum time EnsureRunning() will wait for a
	// container to become healthy before returning an error.
	ColdStartTimeout time.Duration // default: 5 minutes

	// HealthPollInterval is how often EnsureRunning() polls for health.
	HealthPollInterval time.Duration // default: 2 seconds

	// EvictCheckInterval is how often the idle manager scans for idle endpoints.
	EvictCheckInterval time.Duration // default: 30 seconds

	// MaxRetries is how many times EnsureRunning() retries a failed start.
	MaxRetries int // default: 2

	// DefaultModelsVolume is the Docker volume or host path mounted as /models.
	DefaultModelsVolume string // default: "llamacpp_models"

	// DefaultImage is the default llama-server image.
	DefaultImage string // default: "ghcr.io/ggml-org/llama.cpp:server"
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		DefaultIdleTimeout:  15 * time.Minute,
		ColdStartTimeout:    20 * time.Minute, // large models (235B) can take 10-15 min to load
		HealthPollInterval:  3 * time.Second,
		EvictCheckInterval:  30 * time.Second,
		MaxRetries:          2,
		DefaultModelsVolume: "llamacpp_models",
		DefaultImage:        "ghcr.io/ggml-org/llama.cpp:server",
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Result types
// ─────────────────────────────────────────────────────────────────────────────

// RunningEndpoint is returned by EnsureRunning when the model is ready.
type RunningEndpoint struct {
	EndpointID  string
	URL         string // "http://<host>:<port>"
	ContainerID string
	WarmupMs    int64 // latency of the EnsureRunning call in ms (0 if already warm)
}

// ModelStatus is a point-in-time snapshot of a model's runtime state.
type ModelStatus struct {
	ModelName   string
	EndpointID  string
	ContainerID string
	State       State
	URL         string
	LastUsedAt  time.Time
	IdleFor     time.Duration
}

// ResourceRequest is checked by the ResourceGuard before starting a new container.
type ResourceRequest struct {
	ModelName   string
	RAMMBNeeded int64 // estimated model RAM (weights + KV cache)
	GPUDevices  []int // empty = CPU-only
	CPUCores    int
	// Project context — used by ResourceGuard to account for reservations.
	// Both optional; if empty, reservation deduction is skipped.
	ProjectPriorityWeight int    // e.g. 900 for production-critical
	ProjectID             string // UUID string of the requesting project
}
