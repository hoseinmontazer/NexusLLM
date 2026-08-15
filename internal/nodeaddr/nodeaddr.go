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

	"github.com/jmoiron/sqlx"
)

// CanonicalHost resolves the network-reachable address of nodeID from the
// nodes table — the single source of truth for "what address is this node
// reachable at." Prefers the registered IP address, falls back to hostname,
// and only falls back to the literal string "localhost" when neither is set
// (e.g. an unregistered/dangling node reference) or the query fails.
//
// This intentionally does NOT treat "localhost"/"127.0.0.1" as inherently
// invalid: if a node's own registered address genuinely is a loopback value
// (a real, deliberately colocated single-node deployment), that IS its
// canonical reachable address, and callers must not "correct" it to
// something else. The invariant this package establishes is "matches what
// the node itself is registered as," not "is not a loopback string."
func CanonicalHost(ctx context.Context, db *sqlx.DB, nodeID string) string {
	var host string
	_ = db.QueryRowContext(ctx,
		`SELECT COALESCE(host(ip_address), hostname, 'localhost') FROM nodes WHERE id = $1`, nodeID,
	).Scan(&host)
	if host == "" {
		return "localhost"
	}
	return host
}
