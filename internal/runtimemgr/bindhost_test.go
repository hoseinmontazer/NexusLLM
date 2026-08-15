package runtimemgr

// Integration tests for the Phase-4 endpoint-host fix (forensic audit,
// Case File 003): loadConfigQuery must resolve cfg.BindHost to the target
// node's canonical reachable address rather than a stale model_endpoints.host,
// and enqueueStartModel's agent_runtimes INSERT must use that resolved value
// directly instead of re-reading model_endpoints.host itself.
//
// Runs against a disposable postgres:15-alpine container, same pattern as
// internal/nodeaddr/nodeaddr_test.go and
// internal/admin/handlers/restore_permissions_test.go.

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

func setupBindHostTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available — skipping bind_host integration tests")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable — skipping bind_host integration tests")
	}

	suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:8]
	pgName := "nexus-test-bindhost-" + suffix
	pgPort := 15800 + int(time.Now().UnixNano()%2000)

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
			lifecycle_state VARCHAR(30) NOT NULL DEFAULT 'registered'
		);

		CREATE TABLE IF NOT EXISTS model_runtime_configs (
			model_id          UUID PRIMARY KEY REFERENCES models(id),
			gguf_path         TEXT,
			hf_repo           TEXT,
			hf_file           TEXT,
			hf_token          TEXT,
			ctx_size          INTEGER,
			n_gpu_layers      INTEGER,
			cpu_threads       TEXT,
			memory_limit      TEXT,
			models_volume     TEXT,
			tensor_parallel   INTEGER,
			gpu_memory_util   FLOAT,
			dtype             TEXT,
			quantization      TEXT,
			gpu_devices       JSONB,
			execution_mode    TEXT,
			workload_policy   TEXT,
			extra_args        JSONB,
			env               JSONB,
			idle_timeout_secs INTEGER
		);

		CREATE TABLE IF NOT EXISTS agent_runtimes (
			id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			node_id        UUID NOT NULL REFERENCES nodes(id),
			endpoint_id    UUID REFERENCES model_endpoints(id),
			model_id       UUID REFERENCES models(id),
			runtime_name   VARCHAR(255) NOT NULL DEFAULT '',
			backend        VARCHAR(50) NOT NULL DEFAULT '',
			container_id   VARCHAR(255) NOT NULL DEFAULT '',
			state          VARCHAR(30) NOT NULL DEFAULT 'pending',
			gpu_ids        JSONB NOT NULL DEFAULT '[]',
			bind_host      VARCHAR(255) NOT NULL DEFAULT '',
			bind_port      INTEGER NOT NULL DEFAULT 0,
			requested_port INTEGER NOT NULL DEFAULT 0,
			actual_port    INTEGER NOT NULL DEFAULT 0,
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
			SELECT COALESCE(
				(SELECT desired_replicas FROM model_replica_specs WHERE model_id = p_model_id),
				1
			);
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

// seedStaleEndpointScenario reproduces the exact historical failure: a model
// whose model_endpoints.host was wrongly set to "localhost" for a model that
// actually belongs on a remote node whose real registered address is
// 192.168.50.99.
func seedStaleEndpointScenario(t *testing.T, db *sqlx.DB) (modelName, nodeID, canonicalAddr string) {
	t.Helper()
	canonicalAddr = "192.168.50.99"
	modelName = "qwen-repro-" + uuid.New().String()[:8]

	if err := db.Get(&nodeID, `INSERT INTO nodes (hostname, ip_address) VALUES ($1, $2) RETURNING id::text`,
		"node-b-"+modelName, canonicalAddr); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	var modelID string
	if err := db.Get(&modelID, `INSERT INTO models (name) VALUES ($1) RETURNING id::text`, modelName); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO model_endpoints (model_id, node_id, host, port, lifecycle_state)
		VALUES ($1::uuid, $2::uuid, 'localhost', 46015, 'active')`,
		modelID, nodeID,
	); err != nil {
		t.Fatalf("seed stale endpoint: %v", err)
	}
	return modelName, nodeID, canonicalAddr
}

// TestLoadConfigQuery_ResolvesCanonicalHostDespiteStaleEndpointHost is the
// exact historical-failure reproduction requested by the audit:
//
//	model_endpoints.host = "localhost"
//	node canonical address = 192.168.50.99
//	→ cfg.BindHost must resolve to 192.168.50.99, not "localhost".
func TestLoadConfigQuery_ResolvesCanonicalHostDespiteStaleEndpointHost(t *testing.T) {
	db := setupBindHostTestDB(t)
	modelName, nodeID, canonicalAddr := seedStaleEndpointScenario(t, db)

	a := &RuntimeActivator{db: db, log: zap.NewNop()}
	cfg, err := a.loadConfigQuery(context.Background(), modelName, false)
	if err != nil {
		t.Fatalf("loadConfigQuery: %v", err)
	}
	if cfg.NodeID != nodeID {
		t.Fatalf("expected NodeID %s, got %s", nodeID, cfg.NodeID)
	}
	if cfg.BindHost != canonicalAddr {
		t.Fatalf("expected cfg.BindHost=%s (the node's canonical address), got %q (stale model_endpoints.host would have been \"localhost\")", canonicalAddr, cfg.BindHost)
	}
}

// TestEnqueueStartModel_UsesCfgBindHost_NotStaleModelEndpointsHost proves the
// INSERT itself — not just cfg's computation — persists the canonical
// address. taskMgr is intentionally nil: the DB transaction commits before
// enqueueStartModel reaches the task-dispatch call, so we recover the
// resulting nil-pointer panic and assert on the already-committed row.
func TestEnqueueStartModel_UsesCfgBindHost_NotStaleModelEndpointsHost(t *testing.T) {
	db := setupBindHostTestDB(t)
	modelName, nodeID, canonicalAddr := seedStaleEndpointScenario(t, db)

	a := &RuntimeActivator{db: db, log: zap.NewNop()}
	cfg, err := a.loadConfigQuery(context.Background(), modelName, false)
	if err != nil {
		t.Fatalf("loadConfigQuery: %v", err)
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Logf("enqueueStartModel panicked (expected: nil taskMgr, panics AFTER the DB commit): %v", r)
			}
		}()
		if err := a.enqueueStartModel(context.Background(), cfg); err != nil {
			t.Logf("enqueueStartModel returned error: %v", err)
		}
	}()

	var bindHost string
	if err := db.Get(&bindHost, `
		SELECT bind_host FROM agent_runtimes WHERE node_id = $1::uuid ORDER BY created_at DESC LIMIT 1`,
		nodeID,
	); err != nil {
		t.Fatalf("expected agent_runtimes row to have been committed before the panic: %v", err)
	}
	if bindHost != canonicalAddr {
		t.Fatalf("expected agent_runtimes.bind_host=%s, got %q — the INSERT re-read stale model_endpoints.host instead of using cfg.BindHost", canonicalAddr, bindHost)
	}
}

// TestEnqueueStartModel_MultiNodeEachResolvesOwnAddress seeds two models on
// two different nodes and confirms each runtime gets its OWN node's address,
// not a value bleeding across from the other.
func TestEnqueueStartModel_MultiNodeEachResolvesOwnAddress(t *testing.T) {
	db := setupBindHostTestDB(t)

	seedOne := func(nodeAddr string) (modelName, nodeID string) {
		modelName = "multi-" + uuid.New().String()[:8]
		if err := db.Get(&nodeID, `INSERT INTO nodes (hostname, ip_address) VALUES ($1, $2) RETURNING id::text`,
			"node-"+modelName, nodeAddr); err != nil {
			t.Fatalf("seed node: %v", err)
		}
		var modelID string
		if err := db.Get(&modelID, `INSERT INTO models (name) VALUES ($1) RETURNING id::text`, modelName); err != nil {
			t.Fatalf("seed model: %v", err)
		}
		if _, err := db.Exec(`
			INSERT INTO model_endpoints (model_id, node_id, host, port, lifecycle_state)
			VALUES ($1::uuid, $2::uuid, 'localhost', 40000, 'active')`, modelID, nodeID); err != nil {
			t.Fatalf("seed endpoint: %v", err)
		}
		return modelName, nodeID
	}

	modelA, nodeA := seedOne("10.0.0.11")
	modelB, nodeB := seedOne("10.0.0.22")

	a := &RuntimeActivator{db: db, log: zap.NewNop()}
	for _, tc := range []struct{ model, node, want string }{
		{modelA, nodeA, "10.0.0.11"},
		{modelB, nodeB, "10.0.0.22"},
	} {
		cfg, err := a.loadConfigQuery(context.Background(), tc.model, false)
		if err != nil {
			t.Fatalf("loadConfigQuery(%s): %v", tc.model, err)
		}
		func() {
			defer func() { _ = recover() }()
			_ = a.enqueueStartModel(context.Background(), cfg)
		}()
		var bindHost string
		if err := db.Get(&bindHost, `SELECT bind_host FROM agent_runtimes WHERE node_id=$1::uuid ORDER BY created_at DESC LIMIT 1`, tc.node); err != nil {
			t.Fatalf("query bind_host for %s: %v", tc.model, err)
		}
		if bindHost != tc.want {
			t.Fatalf("model %s: expected bind_host=%s, got %q", tc.model, tc.want, bindHost)
		}
	}
}
