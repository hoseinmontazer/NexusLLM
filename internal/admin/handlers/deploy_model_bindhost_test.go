package handlers

// Regression tests for the production incident where the Admin Panel
// unconditionally sent host:"localhost" alongside a real node_id, and
// DeployModel's previous "only resolve canonical host when the caller
// supplied none" guard trusted that literal as genuine intent — silently
// defeating internal/nodeaddr.CanonicalHost resolution (forensic audit,
// Case File 003, round 5).
//
// The fix inverts precedence: node_id != "" now makes the node's own
// canonical address authoritative, unconditionally, regardless of any
// caller-supplied host value.

import (
	"bytes"
	"encoding/json"
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
	"github.com/nexusllm/nexusllm/internal/runtime"
	"github.com/nexusllm/nexusllm/internal/taskmanager"
	"go.uber.org/zap"
)

func setupDeployModelTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available — skipping DeployModel bind_host integration tests")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable — skipping DeployModel bind_host integration tests")
	}

	suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:8]
	pgName := "nexus-test-deploy-" + suffix
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
			ip_address INET
		);

		CREATE TABLE IF NOT EXISTS models (
			id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			name              VARCHAR(255) NOT NULL UNIQUE,
			display_name      VARCHAR(255) NOT NULL DEFAULT '',
			provider          VARCHAR(100) NOT NULL DEFAULT '',
			backend_type      VARCHAR(50)  NOT NULL DEFAULT 'openai_compat',
			service_type      VARCHAR(30)  NOT NULL DEFAULT 'CHAT',
			max_context       INTEGER NOT NULL DEFAULT 8192,
			max_output        INTEGER NOT NULL DEFAULT 4096,
			enabled           BOOLEAN NOT NULL DEFAULT TRUE,
			tags              JSONB NOT NULL DEFAULT '[]',
			capabilities      JSONB NOT NULL DEFAULT '[]',
			supports_thinking BOOLEAN NOT NULL DEFAULT FALSE,
			thinking_enabled  BOOLEAN NOT NULL DEFAULT FALSE,
			min_thinking_tokens INTEGER NOT NULL DEFAULT 500
		);

		CREATE TABLE IF NOT EXISTS model_versions (
			id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			model_id   UUID NOT NULL REFERENCES models(id),
			version    VARCHAR(30) NOT NULL DEFAULT 'v1',
			is_default BOOLEAN NOT NULL DEFAULT TRUE
		);

		CREATE TABLE IF NOT EXISTS model_runtime_configs (
			id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			model_id        UUID NOT NULL REFERENCES models(id),
			gpu_memory_util FLOAT NOT NULL DEFAULT 0.9,
			tensor_parallel INTEGER NOT NULL DEFAULT 1,
			dtype           VARCHAR(30) NOT NULL DEFAULT 'auto',
			quantization    VARCHAR(30),
			gguf_path       TEXT,
			hf_repo         TEXT,
			hf_file         TEXT,
			hf_token        TEXT,
			ctx_size        INTEGER NOT NULL DEFAULT 4096,
			n_gpu_layers    INTEGER NOT NULL DEFAULT 0,
			models_volume   TEXT,
			execution_mode  VARCHAR(20) NOT NULL DEFAULT 'auto'
		);

		CREATE TABLE IF NOT EXISTS model_endpoints (
			id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			model_id           UUID NOT NULL REFERENCES models(id),
			node_id            UUID REFERENCES nodes(id),
			host               VARCHAR(255) NOT NULL DEFAULT '',
			port               INTEGER NOT NULL DEFAULT 0,
			base_path          VARCHAR(50) NOT NULL DEFAULT '/v1',
			weight             INTEGER NOT NULL DEFAULT 100,
			priority           INTEGER NOT NULL DEFAULT 1,
			health_status      VARCHAR(20) NOT NULL DEFAULT 'unknown',
			is_enabled         BOOLEAN NOT NULL DEFAULT TRUE,
			lifecycle_state    VARCHAR(30) NOT NULL DEFAULT 'registered',
			runtime_image      VARCHAR(255) NOT NULL DEFAULT '',
			upstream_api_key   TEXT,
			upstream_base_url  TEXT,
			upstream_proxy     TEXT,
			updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS agent_runtimes (
			id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			node_id      UUID REFERENCES nodes(id),
			endpoint_id  UUID REFERENCES model_endpoints(id),
			model_id     UUID REFERENCES models(id),
			runtime_name VARCHAR(255) NOT NULL DEFAULT '',
			backend      VARCHAR(50) NOT NULL DEFAULT '',
			state        VARCHAR(30) NOT NULL DEFAULT 'pending',
			gpu_ids      JSONB NOT NULL DEFAULT '[]',
			bind_host    VARCHAR(255) NOT NULL DEFAULT '',
			bind_port    INTEGER NOT NULL DEFAULT 0,
			cpu_affinity TEXT NOT NULL DEFAULT '',
			numa_node    INTEGER NOT NULL DEFAULT -1,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
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

func newTestRuntimeHandler(t *testing.T, db *sqlx.DB) *RuntimeHandler {
	t.Helper()
	reg, err := runtime.NewEmptyRegistry(db, nil, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("new empty registry: %v", err)
	}
	h := NewRuntimeHandler(db, nil, reg, nil)
	h.log = zap.NewNop()
	h.taskMgr = taskmanager.NewManager(db, zap.NewNop())
	return h
}

func deployRequest(t *testing.T, h *RuntimeHandler, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	c.Request = httptest.NewRequest("POST", "/admin/v1/models/deploy", bytes.NewReader(b))
	c.Request.Header.Set("Content-Type", "application/json")
	h.DeployModel(c)
	return w
}

func seedNode(t *testing.T, db *sqlx.DB, hostname, ip string) string {
	t.Helper()
	var id string
	if err := db.Get(&id, `INSERT INTO nodes (hostname, ip_address) VALUES ($1, $2) RETURNING id::text`, hostname, ip); err != nil {
		t.Fatalf("seed node %s: %v", hostname, err)
	}
	return id
}

func baseDeployBody(name, nodeID string) map[string]interface{} {
	body := map[string]interface{}{
		"name":         name,
		"display_name": name,
		"backend_type": "llamacpp",
		"image":        "ghcr.io/ggml-org/llama.cpp:server",
		"port":         0,
	}
	if nodeID != "" {
		body["node_id"] = nodeID
		body["placement_mode"] = "specific_node"
		body["specific_node_id"] = nodeID
	}
	return body
}

func fetchBindHost(t *testing.T, db *sqlx.DB, modelName string) (endpointHost, runtimeHost string) {
	t.Helper()
	if err := db.Get(&endpointHost, `
		SELECT me.host FROM model_endpoints me
		JOIN models m ON m.id = me.model_id
		WHERE m.name = $1`, modelName); err != nil {
		t.Fatalf("query model_endpoints.host for %s: %v", modelName, err)
	}
	if err := db.Get(&runtimeHost, `
		SELECT ar.bind_host FROM agent_runtimes ar
		JOIN models m ON m.id = ar.model_id
		WHERE m.name = $1`, modelName); err != nil {
		t.Fatalf("query agent_runtimes.bind_host for %s: %v", modelName, err)
	}
	return endpointHost, runtimeHost
}

// TestDeployModel_NodeIDOverridesLocalhost reproduces the exact production
// incident: the Panel sends host:"localhost" alongside a real node_id.
func TestDeployModel_NodeIDOverridesLocalhost(t *testing.T) {
	db := setupDeployModelTestDB(t)
	h := newTestRuntimeHandler(t, db)
	canonicalAddr := "192.168.0.108"
	nodeID := seedNode(t, db, "aigpu-server", canonicalAddr)

	body := baseDeployBody("nexus-qwen-repro", nodeID)
	body["host"] = "localhost"

	w := deployRequest(t, h, body)
	if w.Code != 201 && w.Code != 202 {
		t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
	}

	epHost, rtHost := fetchBindHost(t, db, "nexus-qwen-repro")
	if epHost != canonicalAddr {
		t.Fatalf("model_endpoints.host = %q, want %q (node's canonical address, not the caller's literal 'localhost')", epHost, canonicalAddr)
	}
	if rtHost != canonicalAddr {
		t.Fatalf("agent_runtimes.bind_host = %q, want %q (node's canonical address, not the caller's literal 'localhost')", rtHost, canonicalAddr)
	}
}

// TestDeployModel_NodeIDOverridesArbitraryHost proves the invariant holds
// regardless of WHAT the caller sends as host — omitted, loopback, or any
// other arbitrary address — whenever a real node is targeted.
func TestDeployModel_NodeIDOverridesArbitraryHost(t *testing.T) {
	canonicalAddr := "10.20.30.40"

	cases := []struct {
		name       string
		modelName  string
		hostInBody interface{} // nil = omit the field entirely
	}{
		{"host omitted", "nexus-omit-host", nil},
		{"host is loopback 127.0.0.1", "nexus-loopback-host", "127.0.0.1"},
		{"host is an arbitrary unrelated address", "nexus-arbitrary-host", "10.99.99.99"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := setupDeployModelTestDB(t)
			h := newTestRuntimeHandler(t, db)
			nodeID := seedNode(t, db, "node-"+tc.modelName, canonicalAddr)

			body := baseDeployBody(tc.modelName, nodeID)
			if tc.hostInBody != nil {
				body["host"] = tc.hostInBody
			}

			w := deployRequest(t, h, body)
			if w.Code != 201 && w.Code != 202 {
				t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
			}

			epHost, rtHost := fetchBindHost(t, db, tc.modelName)
			if epHost != canonicalAddr {
				t.Fatalf("model_endpoints.host = %q, want %q", epHost, canonicalAddr)
			}
			if rtHost != canonicalAddr {
				t.Fatalf("agent_runtimes.bind_host = %q, want %q", rtHost, canonicalAddr)
			}
		})
	}
}

// TestDeployModel_LocalDeployment_NoNode_PreservesHost is the explicit
// no-regression check for requirement #7: a pure local/no-node deployment
// (node_id absent, host absent) must still fall back to "localhost" exactly
// as before this fix.
func TestDeployModel_LocalDeployment_NoNode_PreservesHost(t *testing.T) {
	db := setupDeployModelTestDB(t)
	h := newTestRuntimeHandler(t, db)

	body := baseDeployBody("nexus-local-no-node", "")
	// Local path (no taskMgr dispatch, since node_id=="") — don't start the
	// container, just verify the persisted host value.
	body["start_now"] = false

	w := deployRequest(t, h, body)
	if w.Code != 201 && w.Code != 202 {
		t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
	}

	var epHost string
	if err := db.Get(&epHost, `
		SELECT me.host FROM model_endpoints me
		JOIN models m ON m.id = me.model_id
		WHERE m.name = $1`, "nexus-local-no-node"); err != nil {
		t.Fatalf("query model_endpoints.host: %v", err)
	}
	if epHost != "localhost" {
		t.Fatalf("expected local/no-node deploy to default host to 'localhost', got %q", epHost)
	}
}

// TestDeployModel_LocalDeployment_ExplicitHost_Honored proves the "otherwise
// honor an explicit caller host" half of requirement #2's non-node branch.
func TestDeployModel_LocalDeployment_ExplicitHost_Honored(t *testing.T) {
	db := setupDeployModelTestDB(t)
	h := newTestRuntimeHandler(t, db)

	body := baseDeployBody("nexus-local-explicit-host", "")
	body["host"] = "192.168.5.5"
	body["start_now"] = false

	w := deployRequest(t, h, body)
	if w.Code != 201 && w.Code != 202 {
		t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
	}

	var epHost string
	if err := db.Get(&epHost, `
		SELECT me.host FROM model_endpoints me
		JOIN models m ON m.id = me.model_id
		WHERE m.name = $1`, "nexus-local-explicit-host"); err != nil {
		t.Fatalf("query model_endpoints.host: %v", err)
	}
	if epHost != "192.168.5.5" {
		t.Fatalf("expected explicit caller host to be honored when no node is assigned, got %q", epHost)
	}
}

// TestDeployModel_MultiNode_NoCrossContamination proves two node-backed
// deploys to different nodes each resolve their own node's address and never
// leak into each other.
func TestDeployModel_MultiNode_NoCrossContamination(t *testing.T) {
	db := setupDeployModelTestDB(t)
	h := newTestRuntimeHandler(t, db)

	nodeA := seedNode(t, db, "node-a", "10.1.0.5")
	nodeB := seedNode(t, db, "node-b", "10.1.0.6")

	bodyA := baseDeployBody("nexus-multi-a", nodeA)
	bodyA["host"] = "localhost"
	if w := deployRequest(t, h, bodyA); w.Code != 201 && w.Code != 202 {
		t.Fatalf("deploy A: unexpected status %d: %s", w.Code, w.Body.String())
	}

	bodyB := baseDeployBody("nexus-multi-b", nodeB)
	bodyB["host"] = "localhost"
	if w := deployRequest(t, h, bodyB); w.Code != 201 && w.Code != 202 {
		t.Fatalf("deploy B: unexpected status %d: %s", w.Code, w.Body.String())
	}

	epA, rtA := fetchBindHost(t, db, "nexus-multi-a")
	if epA != "10.1.0.5" || rtA != "10.1.0.5" {
		t.Fatalf("model A: expected 10.1.0.5, got endpoint=%q runtime=%q", epA, rtA)
	}
	epB, rtB := fetchBindHost(t, db, "nexus-multi-b")
	if epB != "10.1.0.6" || rtB != "10.1.0.6" {
		t.Fatalf("model B: expected 10.1.0.6, got endpoint=%q runtime=%q", epB, rtB)
	}
}
