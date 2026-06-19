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
// It shells out to the `docker` CLI so no Docker SDK dependency is needed.
type dockerDriver struct{}

// NewDockerDriver constructs a Docker driver.
func NewDockerDriver() Driver { return &dockerDriver{} }

func (d *dockerDriver) Type() DriverType { return DriverDocker }

func (d *dockerDriver) Start(ctx context.Context, spec RuntimeSpec) (string, error) {
	args := []string{"run", "-d", "--name", containerName(spec)}

	// GPU devices
	if len(spec.GPUDevices) > 0 {
		devList := make([]string, len(spec.GPUDevices))
		for i, idx := range spec.GPUDevices {
			devList[i] = strconv.Itoa(idx)
		}
		args = append(args, "--gpus", fmt.Sprintf("\"device=%s\"", strings.Join(devList, ",")))
	}

	// Port binding
	args = append(args, "-p", fmt.Sprintf("%s:%d:8000", spec.BindHost, spec.BindPort))

	// Environment
	for k, v := range spec.Env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}

	// Resource limits
	if spec.CPULimit != "" {
		args = append(args, "--cpus", spec.CPULimit)
	}
	if spec.MemoryLimit != "" {
		args = append(args, "--memory", spec.MemoryLimit)
	}

	// Restart policy
	args = append(args, "--restart", "unless-stopped")

	// Image
	args = append(args, spec.Image)

	// vLLM command-line arguments
	args = append(args,
		"--model", spec.ModelName,
		"--port", "8000",
	)
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
	if spec.Version != "" {
		args = append(args, "--served-model-name", spec.ModelName)
	}
	args = append(args, spec.ExtraArgs...)

	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return "", fmt.Errorf("docker run: %w — %s", err, string(out))
	}
	return strings.TrimSpace(string(out)), nil // container ID
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
		if code, err := strconv.Atoi(parts[2]); err == nil {
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
	name := fmt.Sprintf("nexus-%s-%s", spec.ModelName, spec.EndpointID[:8])
	return strings.ReplaceAll(name, "/", "-")
}
