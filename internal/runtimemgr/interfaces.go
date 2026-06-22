package runtimemgr

import "context"

// ─────────────────────────────────────────────────────────────────────────────
// Primary interface used by the gateway proxy
// ─────────────────────────────────────────────────────────────────────────────

// Activator is the interface the proxy handler calls to ensure a model is
// running before forwarding a request. It is the single entry point for
// all lazy-load logic.
type Activator interface {
	// EnsureRunning guarantees the named model is healthy and accepting requests
	// before returning. It blocks until the model is ready or ctx is cancelled.
	//
	// Callers should use a context with a reasonable deadline (e.g. 5 minutes
	// for a first cold-start that requires model download, 60 seconds for a
	// warm restart of an already-downloaded model).
	//
	// Returns ErrModelNotFound if the model is not registered.
	// Returns ErrColdStartTimeout if the model fails to become healthy in time.
	// Returns ErrInsufficientResources if the node lacks RAM/GPU to load it.
	EnsureRunning(ctx context.Context, modelName string) (*RunningEndpoint, error)

	// RecordActivity notifies the manager that a request was served for this
	// model. Resets the idle countdown. Called by the proxy after forwarding.
	RecordActivity(ctx context.Context, endpointID string)

	// Status returns the current runtime state of a model.
	Status(ctx context.Context, modelName string) (*ModelStatus, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// Errors
// ─────────────────────────────────────────────────────────────────────────────

// sentinel error values
var (
	ErrModelNotFound        = errStr("model not registered in runtime manager")
	ErrColdStartTimeout     = errStr("model did not become healthy within timeout")
	ErrInsufficientResources = errStr("node has insufficient resources to load model")
	ErrDownloadFailed       = errStr("model download/conversion failed")
)

type errStr string

func (e errStr) Error() string { return string(e) }
