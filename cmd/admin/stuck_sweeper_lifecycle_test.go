package main

// Regression test for Test E (forensic audit, Case File 003, round 6): the
// stuck-runtime sweeper must not recreate a runtime for a model an admin has
// disabled or soft-deleted, even if a runtime happens to be stuck mid-startup
// at the exact moment the model was deleted.

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestSweepStuckRuntimes_IgnoresDeletedModel(t *testing.T) {
	db := setupStuckSweeperTestDB(t)

	var nodeID, modelID, endpointID, stuckRuntimeID string
	if err := db.Get(&nodeID, `INSERT INTO nodes (hostname, ip_address) VALUES ('node-deleted-model', '10.7.0.9') RETURNING id::text`); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	// enabled=TRUE but lifecycle='deleted' — the exact production state found
	// during the audit (EnableModel does not clear a stale 'deleted'
	// lifecycle), so this proves the guard checks lifecycle independently of
	// enabled, not just enabled alone.
	if err := db.Get(&modelID, `INSERT INTO models (name, enabled, lifecycle) VALUES ('deleted-repro', TRUE, 'deleted') RETURNING id::text`); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO model_runtime_configs (model_id) VALUES ($1::uuid)`, modelID); err != nil {
		t.Fatalf("seed runtime config: %v", err)
	}
	if err := db.Get(&endpointID, `
		INSERT INTO model_endpoints (model_id, node_id, host, port, runtime_image)
		VALUES ($1::uuid, $2::uuid, '10.7.0.9', 46020, 'ghcr.io/ggml-org/llama.cpp:server') RETURNING id::text`,
		modelID, nodeID); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
	if err := db.Get(&stuckRuntimeID, `
		INSERT INTO agent_runtimes (node_id, endpoint_id, model_id, state, bind_host, bind_port)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'starting', '10.7.0.9', 46020) RETURNING id::text`,
		nodeID, endpointID, modelID); err != nil {
		t.Fatalf("seed stuck runtime: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	log := zap.NewNop()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Logf("sweepStuckRuntimes panicked (expected: nil taskMgr, panics AFTER the DB commit): %v", r)
			}
		}()
		sweepStuckRuntimes(context.Background(), db, nil, log, 0)
	}()

	// The stuck row itself may still be marked failed (that part of the
	// sweeper is unconditional cleanup) — what must NOT happen is a fresh
	// replacement runtime for this deleted model.
	var totalRuntimes int
	if err := db.Get(&totalRuntimes, `SELECT COUNT(*) FROM agent_runtimes WHERE model_id = $1::uuid`, modelID); err != nil {
		t.Fatalf("count runtimes: %v", err)
	}
	if totalRuntimes != 1 {
		t.Fatalf("expected no replacement runtime created for a deleted model, found %d agent_runtimes rows (expected 1, the original stuck row)", totalRuntimes)
	}

	var taskCount int
	if err := db.Get(&taskCount, `SELECT COUNT(*) FROM agent_tasks`); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if taskCount != 0 {
		t.Fatalf("expected no START_MODEL task dispatched for a deleted model, got %d tasks", taskCount)
	}
}

// TestSweepStuckRuntimes_StillRecoversEnabledModel is the non-regression
// check: the sweeper must still recreate a runtime for a genuinely
// enabled/active model whose runtime got stuck.
func TestSweepStuckRuntimes_StillRecoversEnabledModel(t *testing.T) {
	db := setupStuckSweeperTestDB(t)

	var nodeID, modelID, endpointID string
	if err := db.Get(&nodeID, `INSERT INTO nodes (hostname, ip_address) VALUES ('node-active-model', '10.7.0.10') RETURNING id::text`); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	if err := db.Get(&modelID, `INSERT INTO models (name, enabled, lifecycle) VALUES ('active-repro', TRUE, 'active') RETURNING id::text`); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO model_runtime_configs (model_id) VALUES ($1::uuid)`, modelID); err != nil {
		t.Fatalf("seed runtime config: %v", err)
	}
	if err := db.Get(&endpointID, `
		INSERT INTO model_endpoints (model_id, node_id, host, port, runtime_image)
		VALUES ($1::uuid, $2::uuid, '10.7.0.10', 46021, 'ghcr.io/ggml-org/llama.cpp:server') RETURNING id::text`,
		modelID, nodeID); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO agent_runtimes (node_id, endpoint_id, model_id, state, bind_host, bind_port)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'starting', '10.7.0.10', 46021)`,
		nodeID, endpointID, modelID); err != nil {
		t.Fatalf("seed stuck runtime: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	log := zap.NewNop()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Logf("sweepStuckRuntimes panicked (expected: nil taskMgr, panics AFTER the DB commit): %v", r)
			}
		}()
		sweepStuckRuntimes(context.Background(), db, nil, log, 0)
	}()

	var totalRuntimes int
	if err := db.Get(&totalRuntimes, `SELECT COUNT(*) FROM agent_runtimes WHERE model_id = $1::uuid`, modelID); err != nil {
		t.Fatalf("count runtimes: %v", err)
	}
	if totalRuntimes != 2 {
		t.Fatalf("expected the original stuck row plus one replacement for an enabled/active model, got %d rows", totalRuntimes)
	}
}

// TestSweepStuckRuntimes_NeverResurrectsTerminalStates is the regression test
// for production forensic audit Test E: the generic stuck-runtime sweeper
// must never convert a row the HA reconciler has already classified as
// terminal ('lost', 'stopped', 'archived') back into an automatic-start
// candidate. sweepStuckRuntimes' own query only selects rows in transient
// startup states (loading_model/waiting_ready/starting/pending/validating/
// downloading) — this test proves that guarantee holds even when such a row
// sits well past the stuck threshold, for each terminal state independently.
func TestSweepStuckRuntimes_NeverResurrectsTerminalStates(t *testing.T) {
	db := setupStuckSweeperTestDB(t)

	var nodeID, modelID, endpointID string
	if err := db.Get(&nodeID, `INSERT INTO nodes (hostname, ip_address) VALUES ('node-terminal-states', '10.7.0.11') RETURNING id::text`); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	if err := db.Get(&modelID, `INSERT INTO models (name, enabled, lifecycle) VALUES ('terminal-repro', TRUE, 'active') RETURNING id::text`); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO model_runtime_configs (model_id) VALUES ($1::uuid)`, modelID); err != nil {
		t.Fatalf("seed runtime config: %v", err)
	}
	if err := db.Get(&endpointID, `
		INSERT INTO model_endpoints (model_id, node_id, host, port, runtime_image)
		VALUES ($1::uuid, $2::uuid, '10.7.0.11', 46022, 'ghcr.io/ggml-org/llama.cpp:server') RETURNING id::text`,
		modelID, nodeID); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}

	terminalStates := []string{"lost", "stopped", "archived"}
	var runtimeIDs []string
	for _, state := range terminalStates {
		var id string
		if err := db.Get(&id, `
			INSERT INTO agent_runtimes (node_id, endpoint_id, model_id, state, bind_host, bind_port, updated_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, '10.7.0.11', 46022, NOW() - INTERVAL '2 hours') RETURNING id::text`,
			nodeID, endpointID, modelID, state); err != nil {
			t.Fatalf("seed %s runtime: %v", state, err)
		}
		runtimeIDs = append(runtimeIDs, id)
	}

	log := zap.NewNop()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Logf("sweepStuckRuntimes panicked (expected: nil taskMgr, panics AFTER the DB commit): %v", r)
			}
		}()
		// threshold=0 makes everything eligible by age — if the sweeper's
		// state filter were ever loosened to include terminal states, this
		// would catch it immediately.
		sweepStuckRuntimes(context.Background(), db, nil, log, 0)
	}()

	for i, id := range runtimeIDs {
		var state string
		if err := db.Get(&state, `SELECT state FROM agent_runtimes WHERE id=$1::uuid`, id); err != nil {
			t.Fatalf("query state for %s row: %v", terminalStates[i], err)
		}
		if state != terminalStates[i] {
			t.Fatalf("SECURITY/CORRECTNESS REGRESSION: a %q runtime was resurrected to %q by the stuck-runtime sweeper", terminalStates[i], state)
		}
	}

	// No replacement runtimes should have been created for this model either.
	var totalRuntimes int
	if err := db.Get(&totalRuntimes, `SELECT COUNT(*) FROM agent_runtimes WHERE model_id = $1::uuid`, modelID); err != nil {
		t.Fatalf("count runtimes: %v", err)
	}
	if totalRuntimes != len(terminalStates) {
		t.Fatalf("expected exactly %d rows (no replacements spawned for terminal-state rows), found %d", len(terminalStates), totalRuntimes)
	}
}
