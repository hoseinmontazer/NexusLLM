package scripts

// Validates scripts/repair_bind_host.sql directly — executes the actual file
// (not a paraphrase of it) against seeded data covering every case the
// forensic audit (Case File 003) required: violating node-backed rows,
// already-correct rows, external/provider-backed rows with no node_id,
// and a genuinely-colocated loopback node. Also proves the repair is
// idempotent by running it twice.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

func setupRepairTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available — skipping repair script integration test")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable — skipping repair script integration test")
	}

	suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:8]
	pgName := "nexus-test-repair-" + suffix
	pgPort := 16100 + int(time.Now().UnixNano()%2000)

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
			id   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			name VARCHAR(255) NOT NULL UNIQUE
		);
		CREATE TABLE IF NOT EXISTS model_endpoints (
			id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			model_id UUID REFERENCES models(id),
			node_id  UUID REFERENCES nodes(id),
			host     VARCHAR(255) NOT NULL DEFAULT '',
			port     INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE IF NOT EXISTS agent_runtimes (
			id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			model_id  UUID REFERENCES models(id),
			node_id   UUID NOT NULL REFERENCES nodes(id),
			bind_host VARCHAR(255) NOT NULL DEFAULT '',
			state     VARCHAR(30) NOT NULL DEFAULT 'active'
		);
	`); err != nil {
		t.Fatalf("schema setup: %v", err)
	}
	return db
}

func TestRepairBindHostScript(t *testing.T) {
	db := setupRepairTestDB(t)

	sqlBytes, err := os.ReadFile("repair_bind_host.sql")
	if err != nil {
		t.Fatalf("read repair_bind_host.sql: %v", err)
	}

	// ── Seed every case the audit required ────────────────────────────────
	mk := func(name string) string {
		var id string
		if err := db.Get(&id, `INSERT INTO models (name) VALUES ($1) RETURNING id::text`, name); err != nil {
			t.Fatalf("seed model %s: %v", name, err)
		}
		return id
	}
	mkNode := func(hostname, ip string) string {
		var id string
		if err := db.Get(&id, `INSERT INTO nodes (hostname, ip_address) VALUES ($1, $2) RETURNING id::text`, hostname, ip); err != nil {
			t.Fatalf("seed node %s: %v", hostname, err)
		}
		return id
	}

	// Case 1: violating agent_runtimes row — must be repaired.
	nodeRemote := mkNode("node-remote", "192.168.1.50")
	modelViolating := mk("violating-model")
	var runtimeViolating string
	if err := db.Get(&runtimeViolating, `
		INSERT INTO agent_runtimes (model_id, node_id, bind_host, state) VALUES ($1::uuid, $2::uuid, 'localhost', 'active') RETURNING id::text`,
		modelViolating, nodeRemote); err != nil {
		t.Fatalf("seed violating runtime: %v", err)
	}

	// Case 2: already-correct agent_runtimes row — must NOT change.
	modelCorrect := mk("correct-model")
	var runtimeCorrect string
	if err := db.Get(&runtimeCorrect, `
		INSERT INTO agent_runtimes (model_id, node_id, bind_host, state) VALUES ($1::uuid, $2::uuid, '192.168.1.50', 'active') RETURNING id::text`,
		modelCorrect, nodeRemote); err != nil {
		t.Fatalf("seed correct runtime: %v", err)
	}

	// Case 3: genuinely colocated node whose OWN canonical address is a
	// loopback value — must NOT be "corrected" away from it.
	nodeColocated := mkNode("devbox", "127.0.0.1")
	modelColocated := mk("colocated-model")
	var runtimeColocated string
	if err := db.Get(&runtimeColocated, `
		INSERT INTO agent_runtimes (model_id, node_id, bind_host, state) VALUES ($1::uuid, $2::uuid, '127.0.0.1', 'active') RETURNING id::text`,
		modelColocated, nodeColocated); err != nil {
		t.Fatalf("seed colocated runtime: %v", err)
	}

	// Case 4: external/provider-backed model_endpoints row with NO node_id
	// (routes via upstream_base_url, not node resolution) — must NOT change.
	modelExternal := mk("external-model")
	var endpointExternal string
	if err := db.Get(&endpointExternal, `
		INSERT INTO model_endpoints (model_id, node_id, host, port) VALUES ($1::uuid, NULL, '0.0.0.0', 443) RETURNING id::text`,
		modelExternal); err != nil {
		t.Fatalf("seed external endpoint: %v", err)
	}

	// Case 5: violating model_endpoints row on the same remote node — must be repaired.
	var endpointViolating string
	if err := db.Get(&endpointViolating, `
		INSERT INTO model_endpoints (model_id, node_id, host, port) VALUES ($1::uuid, $2::uuid, 'localhost', 46015) RETURNING id::text`,
		modelViolating, nodeRemote); err != nil {
		t.Fatalf("seed violating endpoint: %v", err)
	}

	runRepair := func() {
		if _, err := db.Exec(string(sqlBytes)); err != nil {
			t.Fatalf("execute repair_bind_host.sql: %v", err)
		}
	}

	assertState := func(label string) {
		var h string
		if err := db.Get(&h, `SELECT bind_host FROM agent_runtimes WHERE id=$1::uuid`, runtimeViolating); err != nil {
			t.Fatalf("%s: query violating runtime: %v", label, err)
		}
		if h != "192.168.1.50" {
			t.Fatalf("%s: expected violating agent_runtimes row repaired to 192.168.1.50, got %q", label, h)
		}
		if err := db.Get(&h, `SELECT bind_host FROM agent_runtimes WHERE id=$1::uuid`, runtimeCorrect); err != nil {
			t.Fatalf("%s: query correct runtime: %v", label, err)
		}
		if h != "192.168.1.50" {
			t.Fatalf("%s: already-correct runtime must be untouched, got %q", label, h)
		}
		if err := db.Get(&h, `SELECT bind_host FROM agent_runtimes WHERE id=$1::uuid`, runtimeColocated); err != nil {
			t.Fatalf("%s: query colocated runtime: %v", label, err)
		}
		if h != "127.0.0.1" {
			t.Fatalf("%s: genuinely colocated loopback runtime must NOT be rewritten, got %q", label, h)
		}
		if err := db.Get(&h, `SELECT host FROM model_endpoints WHERE id=$1::uuid`, endpointExternal); err != nil {
			t.Fatalf("%s: query external endpoint: %v", label, err)
		}
		if h != "0.0.0.0" {
			t.Fatalf("%s: external/provider endpoint with no node_id must be untouched, got %q", label, h)
		}
		if err := db.Get(&h, `SELECT host FROM model_endpoints WHERE id=$1::uuid`, endpointViolating); err != nil {
			t.Fatalf("%s: query violating endpoint: %v", label, err)
		}
		if h != "192.168.1.50" {
			t.Fatalf("%s: expected violating model_endpoints row repaired to 192.168.1.50, got %q", label, h)
		}
	}

	runRepair()
	assertState("after first run")

	// Idempotency: running it again must produce the exact same state.
	runRepair()
	assertState("after second run (idempotency check)")
}
