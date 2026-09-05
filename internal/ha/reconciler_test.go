package ha

// Regression tests for the bounded-recovery-chain fix (production forensic
// audit, Case File 004). An EARLIER round (Case File 003) added a per-row
// recovery_attempt counter, an exponential cooldown, and a max-attempts cap —
// but production data kept accumulating unbounded rows anyway (one logical
// replica reused as a replacement root ~639 times over ~35 hours; a single
// model saw 360 new agent_runtimes rows/hour for 8 consecutive hours;
// recovery_attempt stayed ≈1 on ~99.5% of rows) because:
//  1. Every replacement row was inserted with a HARDCODED recovery_attempt=1
//     and the pre-existing recovered_from column was never populated, so the
//     "attempt count" lived on the OLD row (mutated in place) rather than
//     being a property of the logical replacement CHAIN — nothing could ever
//     read "how many times has this logical replica already been replaced".
//  2. The cooldown was derived from updated_at, which every Case 1/2
//     transition (including just clearing replaced_by) touched — silently
//     resetting the cooldown clock on every reconciler tick.
//  3. plan()'s sibling under-replication top-up path had NO attempt cap at
//     all — only a time-based cooldown — and claim_replica_slot()
//     deliberately excludes 'unhealthy'/'failed' rows from its capacity
//     count (so an in-progress replacement never blocks itself), which means
//     total replacement count was never bounded by capacity either.
//
// These tests prove the fix: recovery_attempt is carried forward via
// recovered_from (so the mathematical invariant attempt <= 5 holds for the
// whole chain, not per-row), next_retry_at is a persisted, non-resettable
// cooldown, and BOTH creation paths (rolling replacement and under-replication
// top-up) share the same bound.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/nexusllm/nexusllm/internal/taskmanager"
	"go.uber.org/zap"
)

func setupReconcilerTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available — skipping HA reconciler integration tests")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable — skipping HA reconciler integration tests")
	}

	suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:8]
	pgName := "nexus-test-ha-" + suffix
	pgPort := 15700 + int(time.Now().UnixNano()%2000)

	run := exec.Command("docker", "run", "-d", "--rm", "--name", pgName,
		"-e", "POSTGRES_PASSWORD=test", "-e", "POSTGRES_DB=test",
		"-p", fmt.Sprintf("%d:5432", pgPort), "postgres:15-alpine")
	if out, err := run.CombinedOutput(); err != nil {
		t.Skipf("could not start disposable postgres container (%v): %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", pgName).Run() })

	dsn := fmt.Sprintf("postgres://postgres:test@127.0.0.1:%d/test?sslmode=disable", pgPort)
	var db *sqlx.DB
	var err error
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		db, err = sqlx.Connect("postgres", dsn)
		if err == nil && db.Ping() == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil || db == nil {
		t.Fatalf("postgres never became ready: %v", err)
	}

	if _, err := db.Exec(`
		CREATE EXTENSION IF NOT EXISTS pgcrypto;

		CREATE TABLE IF NOT EXISTS nodes (
			id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			hostname   VARCHAR(255) NOT NULL UNIQUE,
			ip_address INET,
			status     VARCHAR(20) NOT NULL DEFAULT 'online',
			cordoned   BOOLEAN NOT NULL DEFAULT FALSE
		);

		CREATE TABLE IF NOT EXISTS gpu_nodes (
			id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			node_id UUID NOT NULL REFERENCES nodes(id)
		);
		CREATE TABLE IF NOT EXISTS gpu_devices (
			id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			node_id  UUID NOT NULL REFERENCES gpu_nodes(id),
			vram_mb  BIGINT NOT NULL DEFAULT 0
		);
		CREATE TABLE IF NOT EXISTS gpu_telemetry (
			device_id      UUID NOT NULL,
			memory_used_mb BIGINT NOT NULL DEFAULT 0,
			recorded_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE TABLE IF NOT EXISTS node_capabilities (
			node_id   UUID PRIMARY KEY REFERENCES nodes(id),
			gpu_count INTEGER NOT NULL DEFAULT 0
		);

		CREATE TABLE IF NOT EXISTS models (
			id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			name              VARCHAR(255) NOT NULL UNIQUE,
			backend_type      VARCHAR(50) NOT NULL DEFAULT 'openai_compat',
			enabled           BOOLEAN NOT NULL DEFAULT TRUE,
			lifecycle         VARCHAR(30) NOT NULL DEFAULT 'active',
			supports_thinking BOOLEAN NOT NULL DEFAULT FALSE,
			thinking_enabled  BOOLEAN NOT NULL DEFAULT FALSE,
			deployment_mode   VARCHAR(10) NOT NULL DEFAULT 'managed'
		);

		CREATE TABLE IF NOT EXISTS model_endpoints (
			id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			model_id        UUID NOT NULL REFERENCES models(id),
			node_id         UUID REFERENCES nodes(id),
			host            VARCHAR(255) NOT NULL DEFAULT '',
			port            INTEGER NOT NULL DEFAULT 0,
			runtime_image   VARCHAR(255) NOT NULL DEFAULT '',
			priority        INTEGER NOT NULL DEFAULT 1,
			lifecycle_state VARCHAR(30) NOT NULL DEFAULT 'active'
		);

		CREATE TABLE IF NOT EXISTS model_runtime_configs (
			model_id        UUID PRIMARY KEY REFERENCES models(id),
			gguf_path       TEXT, hf_repo TEXT, hf_file TEXT, hf_token TEXT,
			models_volume   TEXT, ctx_size INTEGER, n_gpu_layers INTEGER,
			execution_mode  TEXT, workload_policy TEXT,
			tensor_parallel INTEGER, gpu_memory_util FLOAT,
			max_model_len   INTEGER, dtype TEXT, quantization TEXT,
			gpu_devices     JSONB, cpu_threads INTEGER,
			memory_limit    TEXT, extra_args JSONB, env JSONB
		);

		CREATE TABLE IF NOT EXISTS model_replica_specs (
			model_id                     UUID PRIMARY KEY REFERENCES models(id),
			desired_replicas             INTEGER NOT NULL DEFAULT 1,
			min_available                INTEGER NOT NULL DEFAULT 1,
			placement_policy             VARCHAR(30) NOT NULL DEFAULT 'spread',
			auto_recover                 BOOLEAN NOT NULL DEFAULT TRUE,
			recovery_delay_s             INTEGER NOT NULL DEFAULT 30,
			max_surge                    INTEGER NOT NULL DEFAULT 1,
			health_retry_interval_s      INTEGER NOT NULL DEFAULT 30,
			replacement_start_timeout_s  INTEGER NOT NULL DEFAULT 900,
			drain_timeout_s              INTEGER NOT NULL DEFAULT 30,
			termination_grace_s          INTEGER NOT NULL DEFAULT 15
		);

		CREATE TABLE IF NOT EXISTS agent_runtimes (
			id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			node_id          UUID REFERENCES nodes(id),
			endpoint_id      UUID REFERENCES model_endpoints(id),
			model_id         UUID REFERENCES models(id),
			runtime_name     VARCHAR(255) NOT NULL DEFAULT '',
			backend          VARCHAR(50) NOT NULL DEFAULT '',
			container_id     VARCHAR(255) NOT NULL DEFAULT '',
			state            VARCHAR(30) NOT NULL DEFAULT 'pending',
			error_msg        TEXT,
			gpu_ids          JSONB NOT NULL DEFAULT '[]',
			bind_host        VARCHAR(255) NOT NULL DEFAULT '',
			bind_port        INTEGER NOT NULL DEFAULT 0,
			cpu_affinity     TEXT NOT NULL DEFAULT '',
			numa_node        INTEGER NOT NULL DEFAULT -1,
			requested_mode   VARCHAR(20) NOT NULL DEFAULT 'auto',
			effective_mode   VARCHAR(20) NOT NULL DEFAULT 'auto',
			workload_policy  VARCHAR(30) NOT NULL DEFAULT 'lazy_load',
			replica_index    INTEGER,
			replaced_by      UUID,
			recovered_from   UUID,
			recovery_attempt INTEGER NOT NULL DEFAULT 0,
			next_retry_at    TIMESTAMPTZ,
			created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS runtime_recovery_log (
			id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			model_id       UUID,
			model_name     VARCHAR(255),
			lost_runtime_id UUID,
			lost_node_id   UUID,
			new_runtime_id UUID,
			new_node_id    UUID,
			replica_index  INTEGER,
			trigger        VARCHAR(30) NOT NULL DEFAULT 'reconcile',
			status         VARCHAR(30) NOT NULL DEFAULT 'pending',
			reason         TEXT,
			created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			completed_at   TIMESTAMPTZ
		);

		CREATE TABLE IF NOT EXISTS agent_tasks (
			id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			node_id         UUID NOT NULL,
			task_type       VARCHAR(50) NOT NULL,
			payload         JSONB NOT NULL DEFAULT '{}',
			status          VARCHAR(20) NOT NULL DEFAULT 'pending',
			priority        INTEGER NOT NULL DEFAULT 50,
			created_by      VARCHAR(100) NOT NULL DEFAULT '',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			claimed_at      TIMESTAMPTZ,
			started_at      TIMESTAMPTZ,
			completed_at    TIMESTAMPTZ,
			timeout_at      TIMESTAMPTZ,
			result          JSONB,
			error_msg       TEXT,
			runtime_id      UUID,
			idempotency_key TEXT UNIQUE
		);

		CREATE TABLE IF NOT EXISTS node_port_leases (
			id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			node_id      UUID NOT NULL REFERENCES nodes(id),
			port         INTEGER NOT NULL,
			runtime_id   UUID,
			model_id     UUID,
			allocated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			released_at  TIMESTAMPTZ
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_port_leases_active_unique
			ON node_port_leases(node_id, port) WHERE released_at IS NULL;

		CREATE OR REPLACE FUNCTION allocate_node_port(p_node_id UUID, p_model_id UUID DEFAULT NULL)
		RETURNS INTEGER AS $$
		DECLARE
			v_port     INTEGER;
			v_lock_key BIGINT;
		BEGIN
			v_lock_key := abs(hashtext(p_node_id::text));
			PERFORM pg_advisory_lock(v_lock_key);
			SELECT s.port INTO v_port
			FROM generate_series(8100, 8999) AS s(port)
			WHERE NOT EXISTS (
				SELECT 1 FROM node_port_leases l
				WHERE l.node_id = p_node_id AND l.port = s.port AND l.released_at IS NULL
			)
			ORDER BY s.port LIMIT 1;
			IF v_port IS NOT NULL THEN
				INSERT INTO node_port_leases (node_id, port, model_id) VALUES (p_node_id, v_port, p_model_id);
			END IF;
			PERFORM pg_advisory_unlock(v_lock_key);
			RETURN COALESCE(v_port, 0);
		END;
		$$ LANGUAGE plpgsql;

		CREATE OR REPLACE FUNCTION release_node_port(p_node_id UUID, p_port INTEGER)
		RETURNS VOID AS $$
		BEGIN
			UPDATE node_port_leases SET released_at = NOW()
			WHERE node_id = p_node_id AND port = p_port AND released_at IS NULL;
		END;
		$$ LANGUAGE plpgsql;

		CREATE OR REPLACE FUNCTION desired_replicas(p_model_id UUID)
		RETURNS INTEGER LANGUAGE sql STABLE AS $$
			SELECT COALESCE((SELECT desired_replicas FROM model_replica_specs WHERE model_id = p_model_id), 1);
		$$;

		CREATE OR REPLACE FUNCTION claim_replica_slot(
			p_model_id UUID, p_desired INTEGER, p_max_surge INTEGER DEFAULT 1
		) RETURNS BOOLEAN LANGUAGE plpgsql AS $$
		DECLARE
			v_non_terminal INTEGER;
			v_limit        INTEGER;
		BEGIN
			PERFORM pg_advisory_xact_lock(hashtext(p_model_id::text));
			SELECT COUNT(*) INTO v_non_terminal FROM agent_runtimes
			WHERE model_id = p_model_id
			  AND state NOT IN ('stopped','deleted','archived','unloaded','lost','draining','failed','unhealthy');
			v_limit := p_desired + COALESCE(p_max_surge, 1);
			RETURN v_non_terminal < v_limit;
		END;
		$$;
	`); err != nil {
		t.Fatalf("schema setup: %v", err)
	}
	return db
}

type reconcilerFixture struct {
	db      *sqlx.DB
	r       *Reconciler
	nodeID  string
	modelID string
}

func seedReconcilerFixture(t *testing.T, db *sqlx.DB, workloadPolicy string) reconcilerFixture {
	t.Helper()
	var nodeID string
	if err := db.Get(&nodeID, `INSERT INTO nodes (hostname, ip_address) VALUES ($1, '10.5.0.10') RETURNING id::text`,
		"node-"+uuid.New().String()[:8]); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	var modelID string
	if err := db.Get(&modelID, `INSERT INTO models (name, backend_type) VALUES ($1, 'llamacpp') RETURNING id::text`,
		"model-"+uuid.New().String()[:8]); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO model_endpoints (model_id, node_id, runtime_image, lifecycle_state) VALUES ($1,$2,'ghcr.io/ggml-org/llama.cpp:server','active')`,
		modelID, nodeID); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO model_runtime_configs (model_id, workload_policy) VALUES ($1,$2)`, modelID, workloadPolicy); err != nil {
		t.Fatalf("seed runtime config: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO model_replica_specs (model_id, desired_replicas, max_surge) VALUES ($1,1,1)`, modelID); err != nil {
		t.Fatalf("seed replica spec: %v", err)
	}
	taskMgr := taskmanager.NewManager(db, zap.NewNop())
	r := NewReconciler(db, taskMgr, nil, zap.NewNop())
	return reconcilerFixture{db: db, r: r, nodeID: nodeID, modelID: modelID}
}

func (f reconcilerFixture) seedUnhealthyRuntime(t *testing.T, updatedAt time.Time, recoveryAttempt int) string {
	t.Helper()
	var id string
	if err := f.db.Get(&id, `
		INSERT INTO agent_runtimes (node_id, model_id, runtime_name, backend, state, bind_port, replica_index, recovery_attempt, updated_at)
		VALUES ($1,$2,$3,'llamacpp','unhealthy',8100,0,$4,$5) RETURNING id::text`,
		f.nodeID, f.modelID, "nexus-"+uuid.New().String()[:8], recoveryAttempt, updatedAt); err != nil {
		t.Fatalf("seed unhealthy runtime: %v", err)
	}
	return id
}

func (f reconcilerFixture) backdateUpdatedAt(t *testing.T, runtimeID string, age time.Duration) {
	t.Helper()
	if _, err := f.db.Exec(`UPDATE agent_runtimes SET updated_at = NOW() - ($2 || ' seconds')::interval WHERE id=$1`,
		runtimeID, int(age.Seconds())); err != nil {
		t.Fatalf("backdate updated_at: %v", err)
	}
}

// setNextRetryAt forces a row's persisted cooldown into the past (or future),
// simulating the passage of time without needing to actually wait — this is
// the field that must NOT be silently reset by routine bookkeeping (the
// production bug this round fixes).
func (f reconcilerFixture) setNextRetryAt(t *testing.T, runtimeID string, when time.Time) {
	t.Helper()
	if _, err := f.db.Exec(`UPDATE agent_runtimes SET next_retry_at = $2 WHERE id=$1`, runtimeID, when); err != nil {
		t.Fatalf("set next_retry_at: %v", err)
	}
}

func (f reconcilerFixture) recoveredFrom(t *testing.T, runtimeID string) *string {
	t.Helper()
	var v *string
	if err := f.db.Get(&v, `SELECT recovered_from::text FROM agent_runtimes WHERE id=$1`, runtimeID); err != nil {
		t.Fatalf("query recovered_from: %v", err)
	}
	return v
}

func (f reconcilerFixture) rowState(t *testing.T, runtimeID string) (state string, replacedBy *string, attempt int) {
	t.Helper()
	if err := f.db.Get(&state, `SELECT state FROM agent_runtimes WHERE id=$1`, runtimeID); err != nil {
		t.Fatalf("query state: %v", err)
	}
	_ = f.db.Get(&replacedBy, `SELECT replaced_by::text FROM agent_runtimes WHERE id=$1`, runtimeID)
	if err := f.db.Get(&attempt, `SELECT recovery_attempt FROM agent_runtimes WHERE id=$1`, runtimeID); err != nil {
		t.Fatalf("query recovery_attempt: %v", err)
	}
	return
}

func (f reconcilerFixture) countRowsForModel(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.Get(&n, `SELECT COUNT(*) FROM agent_runtimes WHERE model_id=$1`, f.modelID); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// TestHandleUnhealthyReplica_RespectsCooldown_DoesNotSpawnImmediately proves
// a freshly-unhealthy row (within its cooldown window) is NOT immediately
// respawned on the very next tick — this is the exact behavior that was
// previously missing (unlike the sibling plan() under-replication path).
func TestHandleUnhealthyReplica_RespectsCooldown_DoesNotSpawnImmediately(t *testing.T) {
	db := setupReconcilerTestDB(t)
	f := seedReconcilerFixture(t, db, "always_on")
	runtimeID := f.seedUnhealthyRuntime(t, time.Now(), 0) // just became unhealthy — updated_at is "now"

	f.r.rollingReplacementSweep(context.Background())

	if n := f.countRowsForModel(t); n != 1 {
		t.Fatalf("expected no replacement spawned within cooldown, got %d total rows for model", n)
	}
	state, replacedBy, attempt := f.rowState(t, runtimeID)
	if state != "unhealthy" || replacedBy != nil || attempt != 0 {
		t.Fatalf("expected original row untouched (unhealthy, no replaced_by, attempt=0), got state=%q replaced_by=%v attempt=%d", state, replacedBy, attempt)
	}
}

// TestHandleUnhealthyReplica_BoundedRetries_ThenTerminal is the core proof
// that the previously-infinite retry loop is now bounded: it drives the
// original unhealthy row through repeated failed-replacement cycles (each
// one backdating past its cooldown, exactly as a real clock would after
// waiting) and asserts that after maxUnhealthyRecoveryAttempts, the row is
// marked terminal ('lost') and no further replacement is spawned on
// subsequent ticks — matching production evidence of one model_id
// accumulating thousands of rows with no such limit.
func TestHandleUnhealthyReplica_BoundedRetries_ThenTerminal(t *testing.T) {
	db := setupReconcilerTestDB(t)
	f := seedReconcilerFixture(t, db, "always_on")
	runtimeID := f.seedUnhealthyRuntime(t, time.Now(), 0)

	ctx := context.Background()

	for attempt := 0; attempt < maxUnhealthyRecoveryAttempts; attempt++ {
		if attempt == 0 {
			// First attempt has no persisted next_retry_at yet — falls back
			// to the row's own updated_at.
			f.backdateUpdatedAt(t, runtimeID, unhealthyRecoveryCooldown(0)+time.Second)
		} else {
			// Subsequent attempts: force the PERSISTED cooldown into the
			// past directly, rather than backdating updated_at — proving
			// the cooldown source is next_retry_at, not a value that gets
			// reset by routine bookkeeping (the production bug).
			f.setNextRetryAt(t, runtimeID, time.Now().Add(-time.Second))
		}

		f.r.rollingReplacementSweep(ctx)

		state, replacedBy, hubAttempt := f.rowState(t, runtimeID)
		if state != "unhealthy" {
			t.Fatalf("attempt %d: expected original (hub) row still 'unhealthy' while replacement pending, got %q", attempt, state)
		}
		if replacedBy == nil {
			t.Fatalf("attempt %d: expected replaced_by to be set after spawning a replacement", attempt)
		}
		if hubAttempt != attempt+1 {
			t.Fatalf("attempt %d: expected the hub row's own recovery_attempt to track chain depth (%d), got %d", attempt, attempt+1, hubAttempt)
		}
		// The NEW row (not the hub) must carry the same attempt number and
		// point recovered_from back at the hub — this is the actual
		// per-attempt bookkeeping the production bug lost entirely.
		_, _, newAttempt := f.rowState(t, *replacedBy)
		if newAttempt != attempt+1 {
			t.Fatalf("attempt %d: expected new replacement row's own recovery_attempt=%d, got %d", attempt, attempt+1, newAttempt)
		}
		if rf := f.recoveredFrom(t, *replacedBy); rf == nil || *rf != runtimeID {
			t.Fatalf("attempt %d: expected new row's recovered_from to point at the hub %s, got %v", attempt, runtimeID, rf)
		}

		// Simulate the replacement failing quickly (e.g. bad config) so the
		// next loop iteration exercises Case 1's "failed" branch.
		if _, err := db.Exec(`UPDATE agent_runtimes SET state='failed' WHERE id=$1`, *replacedBy); err != nil {
			t.Fatalf("attempt %d: mark replacement failed: %v", attempt, err)
		}
		// One more sweep clears the pointer (Case 1's failed branch) and
		// persists the next cooldown.
		f.r.rollingReplacementSweep(ctx)
		state, replacedBy, _ = f.rowState(t, runtimeID)
		if replacedBy != nil {
			t.Fatalf("attempt %d: expected replaced_by cleared after replacement failed, still %v", attempt, *replacedBy)
		}
	}

	// One more sweep, past the attempt cap: must NOT spawn again, and must
	// transition the hub row to a terminal state.
	f.setNextRetryAt(t, runtimeID, time.Now().Add(-time.Second))
	rowsBefore := f.countRowsForModel(t)
	f.r.rollingReplacementSweep(ctx)
	rowsAfter := f.countRowsForModel(t)
	if rowsAfter != rowsBefore {
		t.Fatalf("expected no new row spawned after max attempts exhausted, went from %d to %d rows", rowsBefore, rowsAfter)
	}
	finalState, _, finalAttempt := f.rowState(t, runtimeID)
	if finalState != "lost" {
		t.Fatalf("expected hub row marked terminal ('lost') after exhausting retries, got %q", finalState)
	}
	if finalAttempt != maxUnhealthyRecoveryAttempts {
		t.Fatalf("expected recovery_attempt to stop at %d, got %d", maxUnhealthyRecoveryAttempts, finalAttempt)
	}

	// Prove subsequent ticks create nothing further — the row is no longer
	// 'unhealthy' so stepUnhealthyReplicas' query no longer selects it at all.
	for i := 0; i < 3; i++ {
		f.r.rollingReplacementSweep(ctx)
	}
	if n := f.countRowsForModel(t); n != rowsAfter {
		t.Fatalf("expected zero additional rows on subsequent ticks after abandonment, went from %d to %d", rowsAfter, n)
	}

	var logCount int
	if err := db.Get(&logCount, `SELECT COUNT(*) FROM runtime_recovery_log WHERE model_id=$1 AND status='abandoned'`, f.modelID); err != nil {
		t.Fatalf("query recovery log: %v", err)
	}
	if logCount == 0 {
		t.Fatal("expected an 'abandoned' entry in runtime_recovery_log for observability — none found")
	}
}

// TestExecuteReturningID_ChainAttemptAccumulatesAndCaps directly proves the
// mathematical invariant required by the production audit: for one logical
// replica (a chain of rows linked via recovered_from), attempt numbers
// strictly increase 1..maxUnhealthyRecoveryAttempts and the chain is refused
// (marked 'lost') beyond that — regardless of which caller (rolling
// replacement or under-replication top-up) drives it. This bypasses
// handleUnhealthyReplica entirely and calls executeReturningID directly so
// the chain mechanics are tested in isolation from cooldown/state-machine
// concerns already covered above.
func TestExecuteReturningID_ChainAttemptAccumulatesAndCaps(t *testing.T) {
	db := setupReconcilerTestDB(t)
	f := seedReconcilerFixture(t, db, "always_on")
	ctx := context.Background()

	baseAction := func(recoveredFrom string) ReconcileAction {
		return ReconcileAction{
			ModelID:       f.modelID,
			ModelName:     "chain-test",
			Action:        "start_replica",
			TargetNode:    f.nodeID,
			ReplicaIdx:    0,
			RecoveredFrom: recoveredFrom,
			Reason:        "test",
		}
	}
	status := ReplicaStatus{ModelID: f.modelID, ModelName: "chain-test", DesiredReplicas: 1, MaxSurge: 1}

	var chain []string
	prev := ""
	for i := 1; i <= maxUnhealthyRecoveryAttempts; i++ {
		id, attempt, err := f.r.executeReturningID(ctx, status, baseAction(prev))
		if err != nil {
			t.Fatalf("attempt %d: unexpected error: %v", i, err)
		}
		if attempt != i {
			t.Fatalf("attempt %d: expected computed attempt=%d, got %d", i, i, attempt)
		}
		// Mark this row terminal so the NEXT iteration's ClaimSlot check
		// (and the next chain link) has a clean, unambiguous predecessor.
		if _, err := db.Exec(`UPDATE agent_runtimes SET state='failed' WHERE id=$1`, id); err != nil {
			t.Fatalf("attempt %d: mark failed: %v", i, err)
		}
		chain = append(chain, id)
		prev = id
	}

	// The 6th attempt must be refused — chain exhausted.
	_, _, err := f.r.executeReturningID(ctx, status, baseAction(prev))
	if !errors.Is(err, ErrRecoveryChainExhausted) {
		t.Fatalf("expected ErrRecoveryChainExhausted on the attempt beyond the cap, got %v", err)
	}
	var finalState string
	if err := db.Get(&finalState, `SELECT state FROM agent_runtimes WHERE id=$1`, prev); err != nil {
		t.Fatalf("query final predecessor state: %v", err)
	}
	if finalState != "lost" {
		t.Fatalf("expected the exhausted chain's last row to be marked 'lost', got %q", finalState)
	}

	// No row anywhere in the chain should show an attempt number outside
	// [1, maxUnhealthyRecoveryAttempts] — the core invariant.
	for i, id := range chain {
		var attempt int
		if err := db.Get(&attempt, `SELECT recovery_attempt FROM agent_runtimes WHERE id=$1`, id); err != nil {
			t.Fatalf("query chain[%d] attempt: %v", i, err)
		}
		if attempt < 1 || attempt > maxUnhealthyRecoveryAttempts {
			t.Fatalf("chain[%d] (id=%s): attempt=%d violates 1<=attempt<=%d", i, id, attempt, maxUnhealthyRecoveryAttempts)
		}
	}
}

// TestExecuteReturningID_ConcurrentCreates_CapacityAndReplicaIndexSafe is the
// direct regression test for the production evidence of 12 agent_runtimes
// rows created within ~400ms, all sharing replica_index=0 on the same node,
// for a model configured with desired_replicas=1, max_surge=1 (so at most 2
// non-terminal rows should ever coexist). It fires several concurrent
// creation attempts at the same fresh model and asserts both invariants
// (ClaimSlot's capacity bound and collision-free replica_index) hold despite
// the race — this is exactly the fix being verified: the replica_index
// computation now happens inside the same advisory-lock scope ClaimSlot
// acquires, instead of before the transaction began.
//
// Each goroutine targets a DIFFERENT node (allocatePort's underlying
// allocate_node_port() DB function serializes ALL port allocations for one
// node behind a single session-level pg_advisory_lock — a pre-existing
// mechanism, unrelated to this round's fix — so contending on the SAME node
// would test port-lease queuing rather than the model-level capacity/
// replica_index invariant this test targets). ClaimSlot's advisory lock is
// keyed by MODEL, not node, so this still fully exercises real concurrent
// contention on the actual invariant under test.
func TestExecuteReturningID_ConcurrentCreates_CapacityAndReplicaIndexSafe(t *testing.T) {
	db := setupReconcilerTestDB(t)
	f := seedReconcilerFixture(t, db, "always_on")
	ctx := context.Background()
	status := ReplicaStatus{ModelID: f.modelID, ModelName: "concurrent-test", DesiredReplicas: 1, MaxSurge: 1}

	const concurrency = 6
	nodeIDs := make([]string, concurrency)
	nodeIDs[0] = f.nodeID
	for i := 1; i < concurrency; i++ {
		if err := db.Get(&nodeIDs[i], `INSERT INTO nodes (hostname, ip_address) VALUES ($1, '10.5.0.20') RETURNING id::text`,
			fmt.Sprintf("concurrent-node-%d-%s", i, uuid.New().String()[:6])); err != nil {
			t.Fatalf("seed extra node %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func(nodeID string) {
			defer wg.Done()
			action := ReconcileAction{
				ModelID:    f.modelID,
				ModelName:  "concurrent-test",
				Action:     "start_replica",
				TargetNode: nodeID,
				ReplicaIdx: 0, // deliberately identical pre-lock estimate for every goroutine
				Reason:     "concurrency test",
			}
			_, _, _ = f.r.executeReturningID(ctx, status, action)
		}(nodeIDs[i])
	}
	wg.Wait()

	var nonTerminal int
	if err := db.Get(&nonTerminal, `
		SELECT COUNT(*) FROM agent_runtimes
		WHERE model_id=$1
		  AND state NOT IN ('stopped','deleted','archived','unloaded','lost','draining','failed','unhealthy')`,
		f.modelID); err != nil {
		t.Fatalf("count non-terminal: %v", err)
	}
	if nonTerminal > status.DesiredReplicas+status.MaxSurge {
		t.Fatalf("capacity violated: %d non-terminal rows for desired=%d+surge=%d", nonTerminal, status.DesiredReplicas, status.MaxSurge)
	}

	var replicaIndexes []int
	if err := db.Select(&replicaIndexes, `
		SELECT replica_index FROM agent_runtimes
		WHERE model_id=$1
		  AND state NOT IN ('stopped','deleted','archived','unloaded','lost','draining','failed','unhealthy')`,
		f.modelID); err != nil {
		t.Fatalf("query replica_index values: %v", err)
	}
	seen := make(map[int]bool, len(replicaIndexes))
	for _, idx := range replicaIndexes {
		if seen[idx] {
			t.Fatalf("replica_index collision among non-terminal rows: index %d used more than once (indexes=%v)", idx, replicaIndexes)
		}
		seen[idx] = true
	}
}

// TestHandleUnhealthyReplica_HealthyReplacement_StillDrainsOldRuntime proves
// the fix does not disable normal HA recovery: when a replacement genuinely
// becomes ready, the old unhealthy runtime must still transition to
// draining exactly as before.
func TestHandleUnhealthyReplica_HealthyReplacement_StillDrainsOldRuntime(t *testing.T) {
	db := setupReconcilerTestDB(t)
	f := seedReconcilerFixture(t, db, "always_on")
	runtimeID := f.seedUnhealthyRuntime(t, time.Now(), 0)
	f.backdateUpdatedAt(t, runtimeID, unhealthyRecoveryCooldown(0)+time.Second)

	ctx := context.Background()
	f.r.rollingReplacementSweep(ctx)

	_, replacedBy, _ := f.rowState(t, runtimeID)
	if replacedBy == nil {
		t.Fatal("expected a replacement to have been spawned")
	}

	// Simulate the replacement becoming healthy.
	if _, err := db.Exec(`UPDATE agent_runtimes SET state='ready' WHERE id=$1`, *replacedBy); err != nil {
		t.Fatalf("mark replacement ready: %v", err)
	}
	f.r.rollingReplacementSweep(ctx)

	finalState, _, _ := f.rowState(t, runtimeID)
	if finalState != "draining" {
		t.Fatalf("expected old runtime to transition to 'draining' once replacement is ready, got %q", finalState)
	}
}

// TestHandleUnhealthyReplica_FailedReplacement_ContainerActuallyStopped
// proves the container-leak fix: previously, when a rolling-replacement
// attempt itself ended in 'failed' (or timed out), only its agent_runtimes
// row was updated — the container it had already started (if any) was never
// asked to stop, since the only stop-dispatch path (stopDrainedRuntime) is
// reachable exclusively via the 'draining' state, which a failed replacement
// never passes through. Confirmed in production: dozens of same-model
// containers accumulating over hours/days with no live route to them.
func TestHandleUnhealthyReplica_FailedReplacement_ContainerActuallyStopped(t *testing.T) {
	db := setupReconcilerTestDB(t)
	f := seedReconcilerFixture(t, db, "always_on")
	runtimeID := f.seedUnhealthyRuntime(t, time.Now(), 0)
	f.backdateUpdatedAt(t, runtimeID, unhealthyRecoveryCooldown(0)+time.Second)

	ctx := context.Background()
	f.r.rollingReplacementSweep(ctx)

	_, replacedBy, _ := f.rowState(t, runtimeID)
	if replacedBy == nil {
		t.Fatal("expected a replacement to have been spawned")
	}

	// Simulate the replacement having actually started a real container
	// (container_id populated by the node-agent's TaskResult) and then
	// failing before ever becoming ready.
	const fakeContainerID = "deadbeef0000"
	if _, err := db.Exec(`UPDATE agent_runtimes SET state='failed', container_id=$2 WHERE id=$1`,
		*replacedBy, fakeContainerID); err != nil {
		t.Fatalf("mark replacement failed with container_id: %v", err)
	}

	f.r.rollingReplacementSweep(ctx)

	// The hub row must have cleared its pointer (existing behavior).
	_, clearedReplacedBy, _ := f.rowState(t, runtimeID)
	if clearedReplacedBy != nil {
		t.Fatalf("expected replaced_by cleared after replacement failed, still %v", *clearedReplacedBy)
	}

	// The fix: an UNLOAD_RUNTIME task must have been enqueued for the FAILED
	// REPLACEMENT's runtime_id — this is the only mechanism that actually
	// stops the physical container. Without the fix, agent_tasks stays empty.
	var taskCount int
	if err := db.Get(&taskCount, `
		SELECT COUNT(*) FROM agent_tasks
		WHERE task_type = 'UNLOAD_RUNTIME' AND runtime_id::text = $1`, *replacedBy); err != nil {
		t.Fatalf("query agent_tasks: %v", err)
	}
	if taskCount == 0 {
		t.Fatalf("expected an UNLOAD_RUNTIME task enqueued for the failed replacement's container (runtime_id=%s) — none found; the container leaks", *replacedBy)
	}
}

// TestSweepFailedContainers_GracePeriodRows_ContainerActuallyStopped proves
// the same fix applies to the general failed→stopped grace-period sweep, not
// just the rolling-replacement path — any 'failed' row with a real
// container_id that wasn't confirmed dead must get a stop dispatched before
// its bookkeeping moves to 'stopped'.
func TestSweepFailedContainers_GracePeriodRows_ContainerActuallyStopped(t *testing.T) {
	db := setupReconcilerTestDB(t)
	f := seedReconcilerFixture(t, db, "always_on")

	const fakeContainerID = "cafef00dbaad"
	var runtimeID string
	// error_msg must be non-NULL and not match '%[container-dead]%' — Path 2's
	// filter is `error_msg NOT LIKE ...`, which is NULL (excludes the row) for
	// a NULL error_msg, same as the original Path 1/Path 2 split intends.
	if err := f.db.Get(&runtimeID, `
		INSERT INTO agent_runtimes (node_id, model_id, runtime_name, backend, state, container_id, error_msg, bind_port, replica_index, updated_at)
		VALUES ($1,$2,$3,'llamacpp','failed',$4,'startup probe failed',8100,0,NOW() - INTERVAL '10 minutes') RETURNING id::text`,
		f.nodeID, f.modelID, "nexus-"+uuid.New().String()[:8], fakeContainerID); err != nil {
		t.Fatalf("seed stale failed runtime: %v", err)
	}

	f.r.sweepFailedContainers(context.Background())

	state, _, _ := f.rowState(t, runtimeID)
	if state != "stopped" {
		t.Fatalf("expected row moved to 'stopped' after grace period, got %q", state)
	}

	var taskCount int
	if err := db.Get(&taskCount, `
		SELECT COUNT(*) FROM agent_tasks
		WHERE task_type = 'UNLOAD_RUNTIME' AND runtime_id::text = $1`, runtimeID); err != nil {
		t.Fatalf("query agent_tasks: %v", err)
	}
	if taskCount == 0 {
		t.Fatalf("expected an UNLOAD_RUNTIME task enqueued before the grace-period sweep marked this row 'stopped' — none found; the container leaks")
	}
}

// TestNextReplicaIndex_PicksSmallestUnusedSlot proves the replica_index
// collision fix directly: production data showed two simultaneously-active
// replicas both labeled "-r1-" (forensic audit, Case File 003, round 6).
// nextReplicaIndex must never hand out an index already held by a
// non-terminal row for the same model.
func TestNextReplicaIndex_PicksSmallestUnusedSlot(t *testing.T) {
	db := setupReconcilerTestDB(t)
	f := seedReconcilerFixture(t, db, "always_on")
	ctx := context.Background()

	insertNonTerminal := func(idx int) {
		if _, err := db.Exec(`
			INSERT INTO agent_runtimes (node_id, model_id, runtime_name, backend, state, bind_port, replica_index)
			VALUES ($1,$2,$3,'llamacpp','ready',8100,$4)`,
			f.nodeID, f.modelID, "nexus-existing-"+uuid.New().String()[:6], idx); err != nil {
			t.Fatalf("seed existing replica idx=%d: %v", idx, err)
		}
	}

	if got := f.r.nextReplicaIndex(ctx, f.modelID); got != 0 {
		t.Fatalf("expected 0 for a model with no existing replicas, got %d", got)
	}

	insertNonTerminal(0)
	if got := f.r.nextReplicaIndex(ctx, f.modelID); got != 1 {
		t.Fatalf("expected 1 once slot 0 is taken, got %d", got)
	}

	insertNonTerminal(1)
	insertNonTerminal(3) // gap at 2
	if got := f.r.nextReplicaIndex(ctx, f.modelID); got != 2 {
		t.Fatalf("expected the smallest unused slot (2, filling the gap), got %d", got)
	}

	// A terminal-state row's index must be treated as free again.
	var stoppedID string
	if err := db.Get(&stoppedID, `
		INSERT INTO agent_runtimes (node_id, model_id, runtime_name, backend, state, bind_port, replica_index)
		VALUES ($1,$2,$3,'llamacpp','stopped',8100,2) RETURNING id::text`,
		f.nodeID, f.modelID, "nexus-stopped-"+uuid.New().String()[:6]); err != nil {
		t.Fatalf("seed stopped replica: %v", err)
	}
	if got := f.r.nextReplicaIndex(ctx, f.modelID); got != 2 {
		t.Fatalf("expected a stopped row's index (2) to be reusable, got %d", got)
	}
}

// TestStepUnhealthyReplicas_IgnoresLazyLoadModels is a direct regression test
// for a production incident (gpt-oss-120b): the reconciler's rolling-
// replacement path does unconstrained free-placement across nodes, which is
// wrong for a lazy_load model whose files may only exist on one specific
// node — it picked a node without the model's GGUF file and failed
// instantly. The sibling under-replication path (loadReplicaStatuses)
// already excludes lazy_load models for the same reason; this proves
// stepUnhealthyReplicas now does too, leaving lazy_load recovery entirely to
// the cold-start activator (which correctly respects node pinning).
func TestStepUnhealthyReplicas_IgnoresLazyLoadModels(t *testing.T) {
	db := setupReconcilerTestDB(t)
	f := seedReconcilerFixture(t, db, "lazy_load")
	runtimeID := f.seedUnhealthyRuntime(t, time.Now(), 0)
	f.backdateUpdatedAt(t, runtimeID, unhealthyRecoveryCooldown(0)+time.Second)

	f.r.rollingReplacementSweep(context.Background())

	state, replacedBy, attempt := f.rowState(t, runtimeID)
	if state != "unhealthy" || replacedBy != nil || attempt != 0 {
		t.Fatalf("expected a lazy_load model's unhealthy row to be left completely untouched by the reconciler, got state=%q replaced_by=%v attempt=%d", state, replacedBy, attempt)
	}
	if n := f.countRowsForModel(t); n != 1 {
		t.Fatalf("expected no replacement spawned for a lazy_load model, got %d total rows", n)
	}
}
