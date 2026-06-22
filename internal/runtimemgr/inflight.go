package runtimemgr

import "sync"

// inflightMap deduplicates concurrent EnsureRunning calls for the same model.
// The first caller becomes the "owner" and runs the start sequence.
// All other callers block on the channel until the owner is done.
type inflightMap struct {
	mu    sync.Mutex
	calls map[string]chan struct{}
}

func newInflightMap() inflightMap {
	return inflightMap{calls: make(map[string]chan struct{})}
}

// getOrCreate returns the channel for modelName and whether this caller is the
// owner (true = owner, should run the sequence; false = waiter, should block).
func (m *inflightMap) getOrCreate(modelName string) (chan struct{}, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ch, ok := m.calls[modelName]; ok {
		return ch, false // waiter
	}
	ch := make(chan struct{})
	m.calls[modelName] = ch
	return ch, true // owner
}

// release closes the channel (unblocking all waiters) and removes it from the map.
func (m *inflightMap) release(modelName string, ch chan struct{}) {
	m.mu.Lock()
	delete(m.calls, modelName)
	m.mu.Unlock()
	close(ch)
}
