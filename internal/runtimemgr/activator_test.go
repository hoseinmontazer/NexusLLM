package runtimemgr

import (
	"sync"
	"testing"

	"github.com/nexusllm/nexusllm/internal/runtime"
)

// TestInflightDeduplication_ConcurrentColdStart verifies that when 100 concurrent
// requests call EnsureRunning / getOrCreate for the same model, exactly ONE
// caller receives owner=true to start the container, and all other 99 callers
// receive owner=false and block until release.
func TestInflightDeduplication_ConcurrentColdStart(t *testing.T) {
	inf := newInflightMap()
	modelName := "whisper-large-v3"

	const concurrency = 100
	var wg sync.WaitGroup
	wg.Add(concurrency)

	var mu sync.Mutex
	ownerCount := 0
	waiterCount := 0

	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			ch, owner := inf.getOrCreate(modelName)
			mu.Lock()
			if owner {
				ownerCount++
			} else {
				waiterCount++
			}
			mu.Unlock()

			if !owner {
				// Waiter blocks until owner releases
				<-ch
			}
		}()
	}

	// Give goroutines time to hit getOrCreate
	for {
		mu.Lock()
		count := ownerCount + waiterCount
		mu.Unlock()
		if count == concurrency {
			break
		}
	}

	mu.Lock()
	if ownerCount != 1 {
		t.Errorf("expected exactly 1 owner, got %d", ownerCount)
	}
	if waiterCount != concurrency-1 {
		t.Errorf("expected %d waiters, got %d", concurrency-1, waiterCount)
	}
	mu.Unlock()

	// Owner releases inflight lock
	ch, owner := inf.getOrCreate(modelName)
	if owner {
		t.Fatalf("expected existing channel, got owner=true")
	}
	inf.release(modelName, ch)

	// Wait for all waiters to unblock
	wg.Wait()
}

// TestPortPrecedenceAndCustomEnvPreservation verifies that when an operator stores
// custom env vars (like WHISPER__MODEL=small, CUSTOM_VAR=foo) and a stale port hint
// (UVICORN_PORT=8000), when a dynamic port (e.g. 32781) is allocated, only the
// runtime listening-port variables (PORT, HTTP_PORT, UVICORN_PORT) are updated
// to match the allocated bind_port, while unrelated env vars remain untouched.
func TestPortPrecedenceAndCustomEnvPreservation(t *testing.T) {
	cfgEnv := map[string]string{
		"WHISPER__MODEL": "small",
		"CUSTOM_VAR":     "foo",
		"UVICORN_PORT":   "8000", // stale hint in lazy config
	}
	bindPort := 32781

	payloadEnv := make(map[string]string, len(cfgEnv))
	for k, v := range cfgEnv {
		payloadEnv[k] = v
	}

	// Call the actual production ContainerPortEnvVars implementation for cpu_native
	backend := runtime.NewCPUNativeBackend(nil)
	if bindPort > 0 {
		for k, v := range backend.ContainerPortEnvVars(bindPort) {
			payloadEnv[k] = v
		}
	}

	// Assert listening-port variables are overridden to the dynamic bind_port
	if payloadEnv["UVICORN_PORT"] != "32781" {
		t.Errorf("expected UVICORN_PORT to be 32781, got %s", payloadEnv["UVICORN_PORT"])
	}
	if payloadEnv["PORT"] != "32781" {
		t.Errorf("expected PORT to be 32781, got %s", payloadEnv["PORT"])
	}
	if payloadEnv["HTTP_PORT"] != "32781" {
		t.Errorf("expected HTTP_PORT to be 32781, got %s", payloadEnv["HTTP_PORT"])
	}

	// Assert custom operator env vars are preserved intact
	if payloadEnv["WHISPER__MODEL"] != "small" {
		t.Errorf("expected WHISPER__MODEL to remain 'small', got %s", payloadEnv["WHISPER__MODEL"])
	}
	if payloadEnv["CUSTOM_VAR"] != "foo" {
		t.Errorf("expected CUSTOM_VAR to remain 'foo', got %s", payloadEnv["CUSTOM_VAR"])
	}
}
