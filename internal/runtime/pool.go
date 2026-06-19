package runtime

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// RoutingStrategy controls how an endpoint is selected from a pool.
type RoutingStrategy string

const (
	StrategyRoundRobin    RoutingStrategy = "round_robin"
	StrategyWeighted      RoutingStrategy = "weighted"
	StrategyActivePassive RoutingStrategy = "active_passive" // primary + hot standby
	StrategyLeastConn     RoutingStrategy = "least_conn"
)

// Endpoint holds runtime state for a single backend instance in a pool.
type Endpoint struct {
	ID          string
	ModelID     string
	URL         string          // http://host:port/v1 (base path included)
	Weight      int             // used by weighted strategy (0 = excluded)
	Priority    int             // 1 = primary, 2+ = standby (active/passive)
	Status      HealthStatus
	ActiveConns int64           // atomic; incremented on dispatch, decremented on finish
	LastSuccess time.Time
	mu          sync.RWMutex
}

// IsAvailable returns true when the endpoint can accept requests.
func (e *Endpoint) IsAvailable() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.Status == StatusHealthy || e.Status == StatusDegraded
}

// SetStatus updates the health status thread-safely.
func (e *Endpoint) SetStatus(s HealthStatus) {
	e.mu.Lock()
	e.Status = s
	if s == StatusHealthy {
		e.LastSuccess = time.Now()
	}
	e.mu.Unlock()
}

// Pool is a collection of Endpoints for a single model.
// It is safe for concurrent use.
type Pool struct {
	ModelID  string
	Strategy RoutingStrategy
	mu       sync.RWMutex
	endpoints []*Endpoint
	rrIdx     atomic.Int64 // round-robin cursor
}

// NewPool constructs an empty Pool.
func NewPool(modelID string, strategy RoutingStrategy) *Pool {
	return &Pool{ModelID: modelID, Strategy: strategy}
}

// Add appends an endpoint to the pool. Safe to call after construction.
func (p *Pool) Add(ep *Endpoint) {
	p.mu.Lock()
	p.endpoints = append(p.endpoints, ep)
	p.mu.Unlock()
}

// Remove removes an endpoint by ID.
func (p *Pool) Remove(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.endpoints[:0]
	for _, ep := range p.endpoints {
		if ep.ID != id {
			out = append(out, ep)
		}
	}
	p.endpoints = out
}

// Pick returns the next healthy endpoint according to the pool strategy.
// Returns an error when no healthy endpoints are available.
func (p *Pool) Pick() (*Endpoint, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	switch p.Strategy {
	case StrategyRoundRobin:
		return p.roundRobin()
	case StrategyWeighted:
		return p.weighted()
	case StrategyActivePassive:
		return p.activePassive()
	case StrategyLeastConn:
		return p.leastConn()
	default:
		return p.roundRobin()
	}
}

// Endpoints returns a snapshot of all endpoints.
func (p *Pool) Endpoints() []*Endpoint {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Endpoint, len(p.endpoints))
	copy(out, p.endpoints)
	return out
}

// HealthyCount returns the number of endpoints that can accept traffic.
func (p *Pool) HealthyCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, ep := range p.endpoints {
		if ep.IsAvailable() {
			n++
		}
	}
	return n
}

// ─── routing algorithms ───────────────────────────────────────────────────────

func (p *Pool) roundRobin() (*Endpoint, error) {
	avail := p.available()
	if len(avail) == 0 {
		return nil, fmt.Errorf("model %s: no healthy endpoints", p.ModelID)
	}
	idx := p.rrIdx.Add(1) - 1
	return avail[idx%int64(len(avail))], nil
}

func (p *Pool) weighted() (*Endpoint, error) {
	avail := p.available()
	if len(avail) == 0 {
		return nil, fmt.Errorf("model %s: no healthy endpoints", p.ModelID)
	}
	total := 0
	for _, ep := range avail {
		total += ep.Weight
	}
	if total == 0 {
		return p.roundRobin()
	}
	r := rand.Intn(total) //nolint:gosec // non-crypto use
	cumulative := 0
	for _, ep := range avail {
		cumulative += ep.Weight
		if r < cumulative {
			return ep, nil
		}
	}
	return avail[len(avail)-1], nil
}

func (p *Pool) activePassive() (*Endpoint, error) {
	// Return the lowest-priority-number healthy endpoint (1 = primary).
	var best *Endpoint
	for _, ep := range p.endpoints {
		if !ep.IsAvailable() {
			continue
		}
		if best == nil || ep.Priority < best.Priority {
			best = ep
		}
	}
	if best == nil {
		return nil, fmt.Errorf("model %s: all endpoints down", p.ModelID)
	}
	return best, nil
}

func (p *Pool) leastConn() (*Endpoint, error) {
	avail := p.available()
	if len(avail) == 0 {
		return nil, fmt.Errorf("model %s: no healthy endpoints", p.ModelID)
	}
	best := avail[0]
	for _, ep := range avail[1:] {
		if atomic.LoadInt64(&ep.ActiveConns) < atomic.LoadInt64(&best.ActiveConns) {
			best = ep
		}
	}
	return best, nil
}

func (p *Pool) available() []*Endpoint {
	out := make([]*Endpoint, 0, len(p.endpoints))
	for _, ep := range p.endpoints {
		if ep.IsAvailable() {
			out = append(out, ep)
		}
	}
	return out
}
