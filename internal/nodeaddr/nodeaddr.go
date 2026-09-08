// Package nodeaddr provides the single canonical way to resolve a node's
// reachable network address for building a runtime endpoint's bind_host.
//
// Forensic audit (Case File 003): every runtime-creation/reconstruction path
// that queried this address independently (internal/ha/reconciler.go, and
// internal/admin/handlers/runtime.go's DeployModel) got it right; every path
// that instead copied or preferred an EXISTING agent_runtimes.bind_host or
// model_endpoints.host value (internal/runtimemgr/activator.go,
// cmd/admin/main.go's stuck-runtime sweeper, internal/admin/handlers/
// controller.go's shared Start/Restart/Upgrade/Rollback path) could silently
// perpetuate a stale or wrong host indefinitely. This package exists so there
// is exactly one implementation of "what address should a runtime on this
// node bind to," not N independent (and inconsistently correct) copies of it.
package nodeaddr

import (
	"context"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// ErrNoAdvertisedAddress is returned when a node exists but has neither an
// ip_address nor a hostname registered — there is nothing for a caller to
// bind a runtime endpoint to.
var ErrNoAdvertisedAddress = errors.New("node has no advertised address (ip_address and hostname both unset)")

// CanonicalHost resolves the network-reachable address of nodeID from the
// nodes table — the single source of truth for "what address is this node
// reachable at." Prefers the registered IP address, falls back to hostname.
//
// It deliberately does NOT fall back to the literal string "localhost" when
// nodeID is empty/unknown or the node has no address registered — doing so
// previously made every caller silently persist an endpoint guaranteed to be
// unreachable from wherever health checks and request proxying actually run,
// instead of failing the deploy with a clear error (forensic audit: this
// silent fallback, combined with internal/admin/handlers/runtime.go's
// DeployModel resolving bind_host from input.NodeID BEFORE the auto-placement
// scheduler decision had set it, is what produced a month of "host=localhost,
// health=down" endpoint rows for auto-placed models while the real container
// ran, unreachable, on the actual node).
//
// This intentionally does NOT treat a node's own genuinely-registered
// "localhost"/"127.0.0.1" as inherently invalid: if that IS what the node is
// registered as (a real, deliberately colocated single-node deployment), that
// is its canonical reachable address, and callers must not "correct" it to
// something else. The invariant this package establishes is "matches what
// the node itself is registered as, and only that," not "is not a loopback
// string."
func CanonicalHost(ctx context.Context, db *sqlx.DB, nodeID string) (string, error) {
	if nodeID == "" {
		return "", fmt.Errorf("nodeaddr.CanonicalHost: empty node ID")
	}
	var ipAddr, hostname *string
	err := db.QueryRowContext(ctx,
		`SELECT host(ip_address), hostname FROM nodes WHERE id = $1`, nodeID,
	).Scan(&ipAddr, &hostname)
	if err != nil {
		return "", fmt.Errorf("nodeaddr.CanonicalHost: node %s: %w", nodeID, err)
	}
	if ipAddr != nil && *ipAddr != "" {
		return *ipAddr, nil
	}
	if hostname != nil && *hostname != "" {
		return *hostname, nil
	}
	return "", fmt.Errorf("nodeaddr.CanonicalHost: node %s: %w", nodeID, ErrNoAdvertisedAddress)
}
