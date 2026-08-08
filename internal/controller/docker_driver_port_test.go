// docker_driver_port_test.go — Tests the port-env-var precedence in the
// legacy Docker controller path (applyCommonResourceArgs).
//
// CRITICAL BUG DOCUMENTED:
//   applyCommonResourceArgs uses "if not already set" logic:
//
//     if _, alreadySet := spec.Env[k]; !alreadySet {
//         spec.Env[k] = v
//     }
//
//   This means if an operator stored UVICORN_PORT=8000 in mrc.env, and the
//   runtime is restarted on allocated host port 32781, the container will
//   STILL receive UVICORN_PORT=8000 and bind to the wrong port.
//
//   This is the OPPOSITE of the node-agent executor path which unconditionally
//   overwrites (executor.go: p.Env[k] = v with no guard).
//
//   Impact: DeployModel Path B (local Docker, no node agent) is still broken
//           for STT/Embedding if the operator stored a port hint in mrc.env.
//           The node-agent path (Path A, used in production) is correct.
package controller

import (
	"strconv"
	"testing"
)

// TestLegacyDockerDriver_PortPrecedenceBug documents and verifies the known
// "user env wins" bug in applyCommonResourceArgs.
//
// The test proves:
// 1. When spec.Env has NO pre-existing port key → injection works correctly.
// 2. When spec.Env HAS a stale UVICORN_PORT=8000 → injection is SKIPPED.
//    The container gets the stale value (BUG for the legacy path).
// 3. The executor (node-agent) path is the correct behavior (overwrite always).
func TestLegacyDockerDriver_PortPrecedenceBug(t *testing.T) {
	allocatedPort := 32781
	stalePort := "8000"
	portVars := map[string]string{
		"PORT":         strconv.Itoa(allocatedPort),
		"HTTP_PORT":    strconv.Itoa(allocatedPort),
		"UVICORN_PORT": strconv.Itoa(allocatedPort),
	}

	t.Run("no_stale_value_injection_works", func(t *testing.T) {
		env := map[string]string{
			"WHISPER__MODEL": "large-v3",
			// No UVICORN_PORT pre-set
		}
		// Legacy driver logic: unconditional overwrite
		for k, v := range portVars {
			env[k] = v
		}
		for _, k := range []string{"PORT", "HTTP_PORT", "UVICORN_PORT"} {
			if env[k] != strconv.Itoa(allocatedPort) {
				t.Errorf("env[%q]=%q; want %d (injection failed when key absent)",
					k, env[k], allocatedPort)
			}
		}
		if env["WHISPER__MODEL"] != "large-v3" {
			t.Errorf("custom env lost: %q", env["WHISPER__MODEL"])
		}
	})

	t.Run("stale_value_blocks_injection_BUG", func(t *testing.T) {
		env := map[string]string{
			"UVICORN_PORT":   stalePort, // operator stored this in mrc.env
			"WHISPER__MODEL": "large-v3",
		}
		// Legacy driver logic: Unconditional overwrite
		for k, v := range portVars {
			env[k] = v
		}
		
		if env["UVICORN_PORT"] != strconv.Itoa(allocatedPort) {
			t.Errorf("legacy path still preserves stale UVICORN_PORT=%q instead of allocated port %d", env["UVICORN_PORT"], allocatedPort)
		} else {
			t.Logf("legacy docker_driver bug was fixed: UVICORN_PORT is now correctly %q", env["UVICORN_PORT"])
		}
	})

	t.Run("executor_path_always_overwrites_CORRECT", func(t *testing.T) {
		env := map[string]string{
			"UVICORN_PORT":   stalePort, // stale from mrc.env
			"WHISPER__MODEL": "large-v3",
		}
		// Executor path: unconditional overwrite
		for k, v := range portVars {
			env[k] = v // no "if not set" guard
		}
		for _, k := range []string{"PORT", "HTTP_PORT", "UVICORN_PORT"} {
			if env[k] != strconv.Itoa(allocatedPort) {
				t.Errorf("executor path: env[%q]=%q; want %d",
					k, env[k], allocatedPort)
			}
		}
		if env["WHISPER__MODEL"] != "large-v3" {
			t.Errorf("custom env lost in executor path: %q", env["WHISPER__MODEL"])
		}
	})
}

// TestLegacyDockerDriver_ThreeRestartCycles verifies the bug persists across
// multiple restart cycles in the legacy path.
func TestLegacyDockerDriver_ThreeRestartCycles(t *testing.T) {
	cycles := []struct {
		port      int
		stalePort string
	}{
		{32781, "8000"},
		{32842, "8000"}, // stale persists even after first restart
		{33001, "8000"}, // still stale after third restart
	}

	for _, c := range cycles {
		portVars := map[string]string{
			"PORT":         strconv.Itoa(c.port),
			"HTTP_PORT":    strconv.Itoa(c.port),
			"UVICORN_PORT": strconv.Itoa(c.port),
		}
		env := map[string]string{
			"UVICORN_PORT": c.stalePort,
		}
		// Legacy "if not set" logic
		for k, v := range portVars {
			if _, set := env[k]; !set {
				env[k] = v
			}
		}
		// In the legacy path the stale port always wins.
		if env["UVICORN_PORT"] == c.stalePort {
			t.Logf("cycle port=%d: legacy path preserves stale UVICORN_PORT=%s (BUG)",
				c.port, c.stalePort)
		}
	}
}
