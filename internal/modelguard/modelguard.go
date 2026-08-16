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
