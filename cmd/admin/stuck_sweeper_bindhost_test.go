package main

// Integration test for the stuck-runtime sweeper bind_host fix (forensic
// audit, Case File 003): sweepStuckRuntimes must resolve the canonical
// reachable address of the target node instead of copying
// model_endpoints.host, which can be stale.

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
	"go.uber.org/zap"
)

func setupStuckSweeperTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available — skipping stuck-sweeper bind_host integration test")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable — skipping stuck-sweeper bind_host integration test")
	}

	suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:8]
	pgName := "nexus-test-sweeper-" + suffix
	pgPort := 16000 + int(time.Now().UnixNano()%2000)

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
			status     VARCHAR(20) NOT NULL DEFAULT 'online'
		);

		CREATE TABLE IF NOT EXISTS models (
			id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			name         VARCHAR(255) NOT NULL UNIQUE,
			backend_type VARCHAR(50) NOT NULL DEFAULT 'openai_compat',
			enabled      BOOLEAN NOT NULL DEFAULT TRUE
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
			max_model_len   INTEGER, dtype TEXT, quantization TEXT
		);

		CREATE TABLE IF NOT EXISTS agent_runtimes (
			id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			node_id        UUID REFERENCES nodes(id),
			endpoint_id    UUID REFERENCES model_endpoints(id),
			model_id       UUID REFERENCES models(id),
			runtime_name   VARCHAR(255) NOT NULL DEFAULT '',
			backend        VARCHAR(50) NOT NULL DEFAULT '',
			container_id   VARCHAR(255) NOT NULL DEFAULT '',
			state          VARCHAR(30) NOT NULL DEFAULT 'pending',
			error_msg      TEXT,
			gpu_ids        JSONB NOT NULL DEFAULT '[]',
			bind_host      VARCHAR(255) NOT NULL DEFAULT '',
			bind_port      INTEGER NOT NULL DEFAULT 0,
			cpu_affinity   TEXT NOT NULL DEFAULT '',
			numa_node      INTEGER NOT NULL DEFAULT -1,
			requested_mode VARCHAR(20) NOT NULL DEFAULT 'auto',
			effective_mode VARCHAR(20) NOT NULL DEFAULT 'auto',
			workload_policy VARCHAR(30) NOT NULL DEFAULT 'lazy_load',
			created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS model_replica_specs (
			model_id         UUID PRIMARY KEY REFERENCES models(id),
			desired_replicas INTEGER NOT NULL DEFAULT 1
		);

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

// TestSweepStuckRuntimes_ResolvesCanonicalHost_NotStaleEndpointHost proves
// requirement #4: a stuck runtime whose model_endpoints.host is stale
// ("localhost") gets replaced by a new runtime bound to its node's canonical
// reachable address, not a copy of that stale value.
func TestSweepStuckRuntimes_ResolvesCanonicalHost_NotStaleEndpointHost(t *testing.T) {
	db := setupStuckSweeperTestDB(t)
	canonicalAddr := "192.168.90.5"

	var nodeID, modelID, endpointID, stuckRuntimeID string
	if err := db.Get(&nodeID, `INSERT INTO nodes (hostname, ip_address) VALUES ('node-y', $1) RETURNING id::text`, canonicalAddr); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	if err := db.Get(&modelID, `INSERT INTO models (name) VALUES ('stuck-repro') RETURNING id::text`); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO model_runtime_configs (model_id) VALUES ($1::uuid)`, modelID); err != nil {
		t.Fatalf("seed runtime config: %v", err)
	}
	if err := db.Get(&endpointID, `
		INSERT INTO model_endpoints (model_id, node_id, host, port, runtime_image)
		VALUES ($1::uuid, $2::uuid, 'localhost', 46015, 'ghcr.io/ggml-org/llama.cpp:server') RETURNING id::text`,
		modelID, nodeID); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
	// A runtime stuck in "starting" — the sweeper's target state.
	if err := db.Get(&stuckRuntimeID, `
		INSERT INTO agent_runtimes (node_id, endpoint_id, model_id, state, bind_host, bind_port)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'starting', 'localhost', 46015) RETURNING id::text`,
		nodeID, endpointID, modelID); err != nil {
		t.Fatalf("seed stuck runtime: %v", err)
	}

	// Ensure the row is unambiguously "stuck" per the sweep's own age check.
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

	var newBindHost string
	if err := db.Get(&newBindHost, `
		SELECT bind_host FROM agent_runtimes
		WHERE node_id = $1::uuid AND id != $2::uuid
		ORDER BY created_at DESC LIMIT 1`,
		nodeID, stuckRuntimeID,
	); err != nil {
		t.Fatalf("expected a replacement agent_runtimes row to have been committed: %v", err)
	}
	if newBindHost != canonicalAddr {
		t.Fatalf("expected replacement runtime's bind_host=%s, got %q — the sweeper copied stale model_endpoints.host instead of resolving the node's canonical address", canonicalAddr, newBindHost)
	}

	// The original stuck row must have been marked failed, not left dangling.
	var oldState, oldErrMsg string
	if err := db.Get(&oldState, `SELECT state FROM agent_runtimes WHERE id = $1::uuid`, stuckRuntimeID); err != nil {
		t.Fatalf("query old runtime state: %v", err)
	}
	_ = db.Get(&oldErrMsg, `SELECT COALESCE(error_msg,'<null>') FROM agent_runtimes WHERE id = $1::uuid`, stuckRuntimeID)
	t.Logf("diagnostic: stuckRuntimeID=%s oldState=%q oldErrMsg=%q", stuckRuntimeID, oldState, oldErrMsg)
	if oldState != "failed" {
		t.Fatalf("expected the original stuck runtime to be marked 'failed', got %q", oldState)
	}
}
