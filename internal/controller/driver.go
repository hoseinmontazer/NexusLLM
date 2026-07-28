// Package controller manages the lifecycle of model runtimes.
// It abstracts over Docker, Docker Compose, and Kubernetes so the rest of
// the platform never needs to know which driver is in use.
package controller

import (
	"context"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Core types
// ─────────────────────────────────────────────────────────────────────────────

// DriverType identifies the runtime driver.
type DriverType string

const (
	DriverDocker        DriverType = "docker"
	DriverDockerCompose DriverType = "docker_compose"
	DriverKubernetes    DriverType = "kubernetes"
)

// RuntimeSpec describes everything needed to start a model runtime.
type RuntimeSpec struct {
	// Logical identity
	ModelName       string // HuggingFace model ID or local path (e.g. "google/gemma-3-27b-it")
	ServedModelName string // Short name clients use (e.g. "gemma-3-27b")
	Version         string
	EndpointID      string
	BackendType     string // vllm | tgi | openai_compat | cpu_native | llamacpp

	// Container image
	Image string

	// Networking
	BindHost string
	BindPort int

	// GPU assignment (GPU_RUNTIME only)
	GPUDevices []int // device indices, e.g. [0, 1]

	// vLLM / backend flags
	TensorParallel int
	GPUMemoryUtil  float64
	MaxModelLen    int
	Dtype          string
	Quantization   string
	ExtraArgs      []string

	// Environment variables
	Env map[string]string

	// Resource limits
	CPULimit    string // e.g. "4" (number of CPUs)
	MemoryLimit string // e.g. "16g"

	// CPU_RUNTIME placement (set by placement engine)
	CPUSetCPUs  string
	NUMANode    int
	RuntimeType string // "GPU_RUNTIME" | "CPU_RUNTIME"

	// ─── Generic model source fields ─────────────────────────────────────────
	// These replace the old LlamaCpp-prefixed fields. The docker driver reads
	// them in buildLlamaCppArgs. Other backends ignore them.
	// Keeping the field names generic means RuntimeSpec stays backend-agnostic.
	//
	// GGUFPath: container-side path to a local GGUF file, e.g. "/models/gemma.gguf"
	GGUFPath string
	// HFRepo: HuggingFace repo ID, e.g. "bartowski/gemma-2-2b-it-GGUF"
	HFRepo string
	// HFFile: specific file in the HF repo, e.g. "gemma-2-2b-it-Q4_K_M.gguf"
	HFFile string
	// CtxSize: context window size (default: 4096). Used by llamacpp server.
	CtxSize int
	// NGPULayers: GPU offload layers. 0=CPU-only, -1=all GPU. Used by llamacpp.
	NGPULayers int
	// ModelsVolume: Docker volume or host path mounted as /models. Used by llamacpp.
	ModelsVolume string
}

// RuntimeStatus is a point-in-time snapshot of a running runtime.
type RuntimeStatus struct {
	EndpointID  string
	Running     bool
	ContainerID string // Docker container ID or K8s pod name
	StartedAt   *time.Time
	ExitCode    *int
	Error       string
}

// Driver is the interface every deployment driver must implement.
type Driver interface {
	// Type identifies the driver.
	Type() DriverType

	// Start creates and starts the runtime described by spec.
	// Returns the container/pod identifier.
	Start(ctx context.Context, spec RuntimeSpec) (id string, err error)

	// Stop gracefully stops the runtime identified by id.
	// timeout is the drain window before force-kill.
	Stop(ctx context.Context, id string, timeout time.Duration) error

	// Restart stops then starts the runtime.
	Restart(ctx context.Context, id string, spec RuntimeSpec, timeout time.Duration) (newID string, err error)

	// Status returns the current status of a runtime.
	Status(ctx context.Context, id string) (RuntimeStatus, error)

	// Logs returns recent stdout/stderr from the runtime.
	Logs(ctx context.Context, id string, tail int) (string, error)

	// Remove destroys the runtime (container/pod/deployment) completely.
	Remove(ctx context.Context, id string) error
}
