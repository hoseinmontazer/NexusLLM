// Package runtimemgr implements the lazy-load runtime management architecture.
//
// Core idea: model containers are started on first request, kept running while
// in use, and automatically stopped after a configurable idle timeout.
// All llama.cpp containers on a node share a single /models volume — GGUF
// files are downloaded once and never duplicated per container.
//
// Flow:
//   Request arrives → Activator.EnsureRunning() → container starts if needed
//   Container idle for IdleTimeout → IdleManager stops it, removes from registry
//   Next request for same model → EnsureRunning() starts it again
package runtimemgr

import "time"

// ─────────────────────────────────────────────────────────────────────────────
// States
// ─────────────────────────────────────────────────────────────────────────────

// State is the lazy-load lifecycle state of a model on a specific node.
// This is separate from the endpoint lifecycle_state in the DB — it is the
// runtime manager's internal view, derived from agent_runtimes rows.
type State string

const (
	// StateUnknown — no information available yet.
	StateUnknown State = "unknown"
	// StateNotRegistered — model has never been deployed on this node.
	StateNotRegistered State = "not_registered"
	// StateDownloading — PULL_MODEL task is in flight; GGUF not yet on volume.
	StateDownloading State = "downloading"
	// StateStopped — container exists (docker ps -a shows it) but is not running.
	// Model file is on the volume. WARM_RUNTIME restarts it without re-downloading.
	StateStopped State = "stopped"
	// StateStarting — DEPLOY_RUNTIME or WARM_RUNTIME task dispatched; container
	// process starting, model not yet loaded into RAM/VRAM.
	StateStarting State = "starting"
	// StateLoading — container is running; model is being loaded into RAM/VRAM.
	// llama-server HTTP is not yet accepting requests.
	StateLoading State = "loading"
	// StateReady — llama-server health check passes; serving requests.
	StateReady State = "ready"
	// StateIdle — ready but no traffic for IdleTimeout; will be stopped soon.
	StateIdle State = "idle"
	// StateStopping — UNLOAD_RUNTIME task dispatched; container stopping.
	StateStopping State = "stopping"
	// StateFailed — container crashed or health check timed out.
	StateFailed State = "failed"
)

// ─────────────────────────────────────────────────────────────────────────────
// Configuration types
// ─────────────────────────────────────────────────────────────────────────────

// ModelConfig is the persisted configuration for a lazy-loadable llama.cpp model.
// Stored in model_runtime_configs (extended columns added by migration 010).
type ModelConfig struct {
	// Routing identity
	ModelName string // short name clients use: "gemma2-2b"
	ModelID   string // models.id UUID

	// llama.cpp model source (first non-empty wins at start time)
	GGUFPath string // path inside container: "/models/gemma-2-2b-it-Q4_K_M.gguf"
	HFRepo   string // "bartowski/gemma-2-2b-it-GGUF"
	HFFile   string // "gemma-2-2b-it-Q4_K_M.gguf"
	HFToken  string // for gated repos

	// Container
	Image        string // "ghcr.io/ggml-org/llama.cpp:server"
	BindPort     int    // unique port per model on this node
	CtxSize      int    // --ctx-size (default: 4096)
	NGPULayers   int    // -1 = all GPU, 0 = CPU-only
	CPUThreads   string // "--threads N" (empty = auto)
	MemoryLimit  string // docker --memory e.g. "8g"
	ModelsVolume string // named volume or absolute host path (default: "llamacpp_models")

	// Placement
	NodeID     string
	GPUDevices []int  // [] = CPU-only
	CPUSetCPUs string // e.g. "0-31"
	NUMANode   int    // -1 = no affinity

	// Idle behaviour
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
		ColdStartTimeout:    5 * time.Minute,
		HealthPollInterval:  2 * time.Second,
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
}
