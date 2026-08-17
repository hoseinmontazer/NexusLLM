package ha

// Regression tests for the unbounded unhealthy-replacement retry loop and
// the replica_index label-collision fix (forensic audit, Case File 003,
// round 6). Production data showed one model_id accumulate 2,318
// agent_runtimes rows because handleUnhealthyReplica retried on every 30s
// tick with no cooldown and no attempt limit — these tests prove that is
// now bounded, without breaking normal (eventually-successful) replacement.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
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
			thinking_enabled  BOOLEAN NOT NULL DEFAULT FALSE
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
			recovery_attempt INTEGER NOT NULL DEFAULT 0,
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
		// Simulate having waited out this attempt's cooldown.
		f.backdateUpdatedAt(t, runtimeID, unhealthyRecoveryCooldown(attempt)+time.Second)

		f.r.rollingReplacementSweep(ctx)

		state, replacedBy, gotAttempt := f.rowState(t, runtimeID)
		if state != "unhealthy" {
			t.Fatalf("attempt %d: expected original row still 'unhealthy' while replacement pending, got %q", attempt, state)
		}
		if replacedBy == nil {
			t.Fatalf("attempt %d: expected replaced_by to be set after spawning a replacement", attempt)
		}
		if gotAttempt != attempt+1 {
			t.Fatalf("attempt %d: expected recovery_attempt=%d, got %d", attempt, attempt+1, gotAttempt)
		}

		// Simulate the replacement failing quickly (e.g. bad config) so the
		// next loop iteration exercises Case 1's "failed" branch.
		if _, err := db.Exec(`UPDATE agent_runtimes SET state='failed' WHERE id=$1`, *replacedBy); err != nil {
			t.Fatalf("attempt %d: mark replacement failed: %v", attempt, err)
		}
		// One more sweep clears the pointer (Case 1's failed branch) —
		// without this, the next loop iteration's cooldown backdate would
		// apply to a row that still has replaced_by set from this attempt.
		f.r.rollingReplacementSweep(ctx)
		state, replacedBy, _ = f.rowState(t, runtimeID)
		if replacedBy != nil {
			t.Fatalf("attempt %d: expected replaced_by cleared after replacement failed, still %v", attempt, *replacedBy)
		}
	}

	// One more sweep, past the attempt cap: must NOT spawn again, and must
	// transition the original row to a terminal state.
	f.backdateUpdatedAt(t, runtimeID, unhealthyRecoveryCooldown(maxUnhealthyRecoveryAttempts)+time.Second)
	rowsBefore := f.countRowsForModel(t)
	f.r.rollingReplacementSweep(ctx)
	rowsAfter := f.countRowsForModel(t)
	if rowsAfter != rowsBefore {
		t.Fatalf("expected no new row spawned after max attempts exhausted, went from %d to %d rows", rowsBefore, rowsAfter)
	}
	finalState, _, finalAttempt := f.rowState(t, runtimeID)
	if finalState != "lost" {
		t.Fatalf("expected original row marked terminal ('lost') after exhausting retries, got %q", finalState)
	}
	if finalAttempt != maxUnhealthyRecoveryAttempts {
		t.Fatalf("expected recovery_attempt to stop incrementing at %d, got %d", maxUnhealthyRecoveryAttempts, finalAttempt)
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
