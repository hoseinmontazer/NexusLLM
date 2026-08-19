// Package modelguard provides the single shared predicate for whether a
// model may have a new agent_runtimes row created for it, by any automatic
// or manual path — the HA reconciler, the cold-start activator, the admin
// Start/Restart/Upgrade/Rollback handlers, and the stuck-runtime sweeper.
//
// Forensic audit (Case File 003, round 6) found these paths disagreed on
// what "still relevant" means: some checked only models.enabled, some
// checked nothing at all, and production data showed models.enabled=TRUE
// coexisting with models.lifecycle='deleted' (EnableModel does not clear a
// stale 'deleted' lifecycle when re-enabling a model), so an enabled-only
// check is not sufficient on its own. Once an admin has disabled OR
// soft-deleted a model, no automatic or manual path may create a new
// runtime for it until the model is explicitly re-enabled (with lifecycle
// restored to non-deleted) or redeployed under a fresh model_id.
package modelguard

// Eligible reports whether a model in this enabled/lifecycle state may have
// a new runtime created for it. The two conditions are checked
// independently: enabled=false blocks creation regardless of lifecycle, and
// lifecycle="deleted" blocks creation regardless of enabled.
func Eligible(enabled bool, lifecycle string) bool {
	return enabled && lifecycle != "deleted"
}

// SQLCondition is the literal SQL fragment equivalent to Eligible, for
// queries that filter directly in a WHERE clause rather than fetching
// enabled/lifecycle into Go first. Every automatic/manual runtime-creation
// query MUST use this exact condition (with its own models-table alias
// substituted for "m") so this predicate cannot drift out of sync with Eligible.
const SQLCondition = "m.enabled = TRUE AND COALESCE(m.lifecycle,'active') != 'deleted'"

// ─────────────────────────────────────────────────────────────────────────────
// Deployment ownership — models.deployment_mode (migration 061)
// ─────────────────────────────────────────────────────────────────────────────

// Deployment modes stored in models.deployment_mode.
const (
	// ModeManaged — NexusLLM owns the container lifecycle: it may create,
	// recreate, stop, evict and preempt the runtime.
	ModeManaged = "managed"

	// ModeManual — the operator deployed the model themselves (docker compose,
	// systemd, another orchestrator, a host with no node agent). NexusLLM
	// routes to it, enforces policy and probes health, but never touches the
	// container. A manual model whose container is down is simply reported
	// unhealthy — nothing tries to bring it up.
	ModeManual = "manual"
)

// ManagedByNexus reports whether NexusLLM owns the container lifecycle for a
// model in this deployment mode. An empty or unrecognised value means
// "managed" so a missing migration 061 or a NULL column never silently
// disables lifecycle management for models that rely on it.
func ManagedByNexus(deploymentMode string) bool {
	return deploymentMode != ModeManual
}

// SQLManagedCondition is the literal SQL fragment equivalent to
// ManagedByNexus, for queries that select candidate models for an automatic
// lifecycle action (runtime creation, idle eviction, HA replacement,
// preemption, stuck-runtime recovery) rather than fetching the mode into Go
// first. Use it with the models-table alias "m", alongside SQLCondition.
const SQLManagedCondition = "COALESCE(m.deployment_mode,'managed') != 'manual'"
