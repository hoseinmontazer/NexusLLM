package nodeaddr

// Integration tests against a disposable postgres:15-alpine container — the
// same pattern used by internal/admin/handlers/restore_permissions_test.go.
// CanonicalHost's whole reason to exist is "there is exactly one correct
// implementation of resolving a node's reachable address"; these tests prove
// its behavior directly so every caller can trust it without re-deriving the
// same SQL.

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
)

func setupNodeaddrTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available — skipping nodeaddr integration tests")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable — skipping nodeaddr integration tests")
	}

	suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:8]
	pgName := "nexus-test-nodeaddr-" + suffix
	pgPort := 15500 + int(time.Now().UnixNano()%3000)

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
	`); err != nil {
		t.Fatalf("schema setup: %v", err)
	}
	return db
}

func TestCanonicalHost_PrefersIPAddressOverHostname(t *testing.T) {
	db := setupNodeaddrTestDB(t)
	var nodeID string
	if err := db.Get(&nodeID, `INSERT INTO nodes (hostname, ip_address) VALUES ('node-b.internal', '192.168.10.22') RETURNING id::text`); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	got := CanonicalHost(context.Background(), db, nodeID)
	if got != "192.168.10.22" {
		t.Fatalf("expected 192.168.10.22, got %q", got)
	}
}

func TestCanonicalHost_FallsBackToHostnameWhenNoIP(t *testing.T) {
	db := setupNodeaddrTestDB(t)
	var nodeID string
	if err := db.Get(&nodeID, `INSERT INTO nodes (hostname, ip_address) VALUES ('node-c.internal', NULL) RETURNING id::text`); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	got := CanonicalHost(context.Background(), db, nodeID)
	if got != "node-c.internal" {
		t.Fatalf("expected node-c.internal, got %q", got)
	}
}

func TestCanonicalHost_UnregisteredNodeFallsBackToLocalhost(t *testing.T) {
	db := setupNodeaddrTestDB(t)
	got := CanonicalHost(context.Background(), db, uuid.New().String())
	if got != "localhost" {
		t.Fatalf("expected localhost fallback for a dangling node_id, got %q", got)
	}
}

// TestCanonicalHost_GenuinelyColocatedLoopbackIsNotRewritten proves the
// topology-aware invariant explicitly: a node that legitimately registered
// itself with a loopback address (a real single-node/dev deployment) must
// resolve to that SAME loopback value, not be "corrected" to something else.
// CanonicalHost has no blacklist of suspicious strings — it only ever
// reflects what the node's own row says.
func TestCanonicalHost_GenuinelyColocatedLoopbackIsNotRewritten(t *testing.T) {
	db := setupNodeaddrTestDB(t)
	var nodeID string
	if err := db.Get(&nodeID, `INSERT INTO nodes (hostname, ip_address) VALUES ('devbox', '127.0.0.1') RETURNING id::text`); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	got := CanonicalHost(context.Background(), db, nodeID)
	if got != "127.0.0.1" {
		t.Fatalf("expected the node's own registered loopback address 127.0.0.1 to be returned unchanged, got %q", got)
	}
}

func TestCanonicalHost_MultipleNodesResolveIndependently(t *testing.T) {
	db := setupNodeaddrTestDB(t)
	var nodeA, nodeB string
	if err := db.Get(&nodeA, `INSERT INTO nodes (hostname, ip_address) VALUES ('node-a', '10.0.0.10') RETURNING id::text`); err != nil {
		t.Fatalf("seed node A: %v", err)
	}
	if err := db.Get(&nodeB, `INSERT INTO nodes (hostname, ip_address) VALUES ('node-b', '10.0.0.20') RETURNING id::text`); err != nil {
		t.Fatalf("seed node B: %v", err)
	}
	if got := CanonicalHost(context.Background(), db, nodeA); got != "10.0.0.10" {
		t.Fatalf("node A: expected 10.0.0.10, got %q", got)
	}
	if got := CanonicalHost(context.Background(), db, nodeB); got != "10.0.0.20" {
		t.Fatalf("node B: expected 10.0.0.20, got %q", got)
	}
}
