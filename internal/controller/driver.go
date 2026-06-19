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
	DriverDocker         DriverType = "docker"
	DriverDockerCompose  DriverType = "docker_compose"
	DriverKubernetes     DriverType = "kubernetes"
)

// RuntimeSpec describes everything needed to start a model runtime.
type RuntimeSpec struct {
	// Logical identity
	ModelName   string
	Version     string
	EndpointID  string

	// Container image
	Image       string

	// Networking
	BindHost    string
	BindPort    int

	// GPU assignment
	GPUDevices  []int  // device indices, e.g. [0, 1]

	// vLLM / backend flags
	TensorParallel   int
	GPUMemoryUtil    float64
	MaxModelLen      int
	Dtype            string
	Quantization     string
	ExtraArgs        []string

	// Environment variables
	Env map[string]string

	// Resource limits
	CPULimit    string  // e.g. "4"
	MemoryLimit string  // e.g. "16g"

	// Kubernetes-specific
	Namespace       string
	DeploymentName  string
	NodeSelector    map[string]string
}

// RuntimeStatus is a point-in-time snapshot of a running runtime.
type RuntimeStatus struct {
	EndpointID  string
	Running     bool
	ContainerID string     // Docker container ID or K8s pod name
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
