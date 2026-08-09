package runtimemgr

import "sync"

// StartTracker deduplicates background startup goroutines for cold-start requests.
//
// # Problem it solves
//
// The proxy handler used to call EnsureRunning with an 8-second context (the
// "probe"). When that context expired, EnsureRunning's defer released the
// inflightMap entry, making the model look "unstarted" to the next request.
// The background goroutine the handler spawned had to win a fresh ownership
// race on every retry — and if it lost, the retry became an 8-second waiter
// that always timed out before the model was ready.
//
// # What StartTracker does
//
// It tracks whether a background startup goroutine is already running for a
// given model name. The goroutine's lifetime is tied to the actual startup
// operation (up to ColdStartTimeout, ≈20 min), not to the HTTP request that
// triggered it. Subsequent HTTP retries check the tracker and skip spawning
// a new goroutine if one is already live.
//
// # What StartTracker does NOT do
//
//   - It does not replace inflightMap. EnsureRunning still uses inflightMap to
//     deduplicate concurrent calls within a single goroutine context.
//   - It does not interact with doStartModel, enqueueStartModel, or the
//     replicaguard. Those remain the authoritative duplicate-prevention layer.
//   - It does not hold any DB state. It is a pure in-process coordination
//     primitive scoped to a single gateway process lifetime.
//
// # Goroutine explosion prevention
//
// TryStart guarantees at most one background startup goroutine per model at a
// time. 100 concurrent cold-start requests produce exactly one goroutine.
// The replicaguard (PostgreSQL advisory lock + replica-slot check) provides
// the cross-process guarantee; StartTracker provides the intra-process guard.
type StartTracker struct {
	mu      sync.Mutex
	running map[string]struct{}
}

// NewStartTracker constructs a StartTracker.
func NewStartTracker() *StartTracker {
	return &StartTracker{running: make(map[string]struct{})}
}

// TryStart reports whether the caller should launch the startup goroutine for
// modelName.
//
//   - Returns true  when no goroutine is currently registered for modelName.
//     The caller MUST call fn() and MUST call Done(modelName) when fn returns.
//   - Returns false when a goroutine is already registered. The caller must
//     NOT spawn another goroutine.
//
// Callers must always follow this pattern:
//
//	if tracker.TryStart(modelName) {
//	    go func() {
//	        defer tracker.Done(modelName)
//	        fn()
//	    }()
//	}
func (t *StartTracker) TryStart(modelName string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.running[modelName]; ok {
		return false
	}
	t.running[modelName] = struct{}{}
	return true
}

// Done removes the registration for modelName, allowing future TryStart calls
// to return true again. Must be called exactly once after the startup goroutine
// completes, regardless of success or failure.
func (t *StartTracker) Done(modelName string) {
	t.mu.Lock()
	delete(t.running, modelName)
	t.mu.Unlock()
}

// IsStarting reports whether a background startup goroutine is currently
// registered for modelName. Used by the proxy handler to skip the probe
// entirely when a startup is already in progress.
func (t *StartTracker) IsStarting(modelName string) bool {
	t.mu.Lock()
	_, ok := t.running[modelName]
	t.mu.Unlock()
	return ok
}
