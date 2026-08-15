package handlers

// Integration tests for the Start/Restart/Upgrade/Rollback bind_host fix
// (forensic audit, Case File 003): loadRuntime is the single shared choke
// point all four handlers call through, so proving it resolves the canonical
// node address — rather than preserving a stale agent_runtimes.bind_host or
// model_endpoints.host — proves the fix for all four at once.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"go.uber.org/zap"
)

func setupControllerTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available — skipping controller bind_host integration tests")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable — skipping controller bind_host integration tests")
	}

	suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:8]
	pgName := "nexus-test-controller-" + suffix
	pgPort := 15900 + int(time.Now().UnixNano()%2000)

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
			ip_address INET
		);

		CREATE TABLE IF NOT EXISTS models (
			id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			name         VARCHAR(255) NOT NULL UNIQUE,
			backend_type VARCHAR(50) NOT NULL DEFAULT 'openai_compat'
		);

		CREATE TABLE IF NOT EXISTS model_endpoints (
			id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			model_id      UUID NOT NULL REFERENCES models(id),
			node_id       UUID REFERENCES nodes(id),
			host          VARCHAR(255) NOT NULL DEFAULT '',
			port          INTEGER NOT NULL DEFAULT 0,
			runtime_image VARCHAR(255) NOT NULL DEFAULT ''
		);

		CREATE TABLE IF NOT EXISTS model_runtime_configs (
			model_id        UUID PRIMARY KEY REFERENCES models(id),
			gpu_devices     JSONB,
			tensor_parallel INTEGER,
			gpu_memory_util FLOAT,
			max_model_len   INTEGER,
			dtype           TEXT,
			quantization    TEXT,
			execution_mode  TEXT,
			gguf_path       TEXT,
			hf_repo         TEXT,
			hf_file         TEXT,
			ctx_size        INTEGER,
			n_gpu_layers    INTEGER,
			memory_limit    TEXT,
			models_volume   TEXT,
			extra_args      JSONB,
			env             JSONB
		);

		CREATE TABLE IF NOT EXISTS agent_runtimes (
			id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			node_id      UUID REFERENCES nodes(id),
			endpoint_id  UUID REFERENCES model_endpoints(id),
			model_id     UUID REFERENCES models(id),
			container_id VARCHAR(255) NOT NULL DEFAULT '',
			runtime_name VARCHAR(255) NOT NULL DEFAULT '',
			backend      VARCHAR(50) NOT NULL DEFAULT '',
			bind_host    VARCHAR(255) NOT NULL DEFAULT '',
			bind_port    INTEGER NOT NULL DEFAULT 0,
			gpu_ids      JSONB NOT NULL DEFAULT '[]',
			state        VARCHAR(30) NOT NULL DEFAULT 'active',
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
	`); err != nil {
		t.Fatalf("schema setup: %v", err)
	}
	return db
}

func testGinContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", nil).WithContext(context.Background())
	return c
}

// TestLoadRuntime_ResolvesCanonicalHost_NotStaleBindHost proves requirement
// #2: an already-running runtime with a stale bind_host must NOT keep that
// stale value on the next Start/Restart/Upgrade/Rollback — loadRuntime is
// the one function all four share, so this single test covers requirement
// #3 as well.
func TestLoadRuntime_ResolvesCanonicalHost_NotStaleBindHost(t *testing.T) {
	db := setupControllerTestDB(t)
	canonicalAddr := "192.168.77.10"

	var nodeID, modelID, endpointID, runtimeID string
	if err := db.Get(&nodeID, `INSERT INTO nodes (hostname, ip_address) VALUES ('node-x', $1) RETURNING id::text`, canonicalAddr); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	if err := db.Get(&modelID, `INSERT INTO models (name) VALUES ('restart-repro') RETURNING id::text`); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO model_runtime_configs (model_id) VALUES ($1::uuid)`, modelID); err != nil {
		t.Fatalf("seed runtime config: %v", err)
	}
	if err := db.Get(&endpointID, `
		INSERT INTO model_endpoints (model_id, node_id, host, port) VALUES ($1::uuid, $2::uuid, 'localhost', 46015) RETURNING id::text`,
		modelID, nodeID); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
	// The existing runtime row ALSO carries the stale value — this is the
	// "preserve stale bind_host" failure mode: both the fallback columns the
	// old query preferred are wrong, not just one.
	if err := db.Get(&runtimeID, `
		INSERT INTO agent_runtimes (node_id, endpoint_id, model_id, bind_host, bind_port, state)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'localhost', 46015, 'active') RETURNING id::text`,
		nodeID, endpointID, modelID); err != nil {
		t.Fatalf("seed stale runtime: %v", err)
	}

	h := NewControllerHandler(db, nil, zap.NewNop())
	c := testGinContext()
	row, ok := h.loadRuntime(c, endpointID)
	if !ok {
		t.Fatalf("loadRuntime failed unexpectedly (status=%d)", c.Writer.Status())
	}
	if row.BindHost != canonicalAddr {
		t.Fatalf("expected BindHost=%s (canonical node address), got %q — stale bind_host was preserved", canonicalAddr, row.BindHost)
	}
	if row.NodeID != nodeID {
		t.Fatalf("expected NodeID=%s, got %s", nodeID, row.NodeID)
	}
}

// TestLoadRuntime_MultiNode_EachEndpointResolvesOwnNode proves requirement
// #6 for the controller.go path specifically: two endpoints on two different
// nodes must each resolve to their own node's address.
func TestLoadRuntime_MultiNode_EachEndpointResolvesOwnNode(t *testing.T) {
	db := setupControllerTestDB(t)

	seedOne := func(addr string) (endpointID string) {
		var nodeID, modelID string
		if err := db.Get(&nodeID, `INSERT INTO nodes (hostname, ip_address) VALUES ($1, $2) RETURNING id::text`,
			"node-"+uuid.New().String()[:8], addr); err != nil {
			t.Fatalf("seed node: %v", err)
		}
		if err := db.Get(&modelID, `INSERT INTO models (name) VALUES ($1) RETURNING id::text`, "m-"+uuid.New().String()[:8]); err != nil {
			t.Fatalf("seed model: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO model_runtime_configs (model_id) VALUES ($1::uuid)`, modelID); err != nil {
			t.Fatalf("seed runtime config: %v", err)
		}
		if err := db.Get(&endpointID, `
			INSERT INTO model_endpoints (model_id, node_id, host, port) VALUES ($1::uuid, $2::uuid, 'localhost', 8000) RETURNING id::text`,
			modelID, nodeID); err != nil {
			t.Fatalf("seed endpoint: %v", err)
		}
		return endpointID
	}

	epA := seedOne("10.1.0.5")
	epB := seedOne("10.1.0.6")

	h := NewControllerHandler(db, nil, zap.NewNop())
	rowA, ok := h.loadRuntime(testGinContext(), epA)
	if !ok || rowA.BindHost != "10.1.0.5" {
		t.Fatalf("endpoint A: expected BindHost=10.1.0.5, got %+v (ok=%v)", rowA, ok)
	}
	rowB, ok := h.loadRuntime(testGinContext(), epB)
	if !ok || rowB.BindHost != "10.1.0.6" {
		t.Fatalf("endpoint B: expected BindHost=10.1.0.6, got %+v (ok=%v)", rowB, ok)
	}
}
