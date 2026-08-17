package proxy

// Regression test for the X-Nexus-Project authorization bypass (forensic
// audit, project-authorization round): a caller must not be able to
// override its project scope to a project it has no real relationship to.
// resolveTeamProjectOverride must only ever resolve a project belonging to
// the CALLER'S OWN team — never org-wide, never a different team's project.

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
	"github.com/nexusllm/nexusllm/internal/auth"
)

func setupProjectOverrideTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available — skipping project override integration tests")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable — skipping project override integration tests")
	}

	suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:8]
	pgName := "nexus-test-projoverride-" + suffix
	pgPort := 16500 + int(time.Now().UnixNano()%2000)

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
		CREATE TABLE organizations (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), name VARCHAR(255) NOT NULL);
		CREATE TABLE teams (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), organization_id UUID REFERENCES organizations(id), name VARCHAR(255) NOT NULL);
		CREATE TABLE projects (
			id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID NOT NULL REFERENCES organizations(id),
			team_id         UUID NOT NULL REFERENCES teams(id),
			name            VARCHAR(255) NOT NULL,
			priority_weight INTEGER NOT NULL DEFAULT 500,
			status          VARCHAR(20) NOT NULL DEFAULT 'active'
		);
	`); err != nil {
		t.Fatalf("schema setup: %v", err)
	}
	return db
}

// TestResolveTeamProjectOverride_RejectsOtherTeamProject is the direct
// regression proof for the bypass: a project belonging to a DIFFERENT team
// in the SAME org must never resolve, even though the pre-fix query would
// have found it (org-scoped, not team-scoped).
func TestResolveTeamProjectOverride_RejectsOtherTeamProject(t *testing.T) {
	db := setupProjectOverrideTestDB(t)
	ctx := context.Background()

	var orgID, teamA, teamB, projectB string
	if err := db.Get(&orgID, `INSERT INTO organizations (name) VALUES ('org') RETURNING id::text`); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := db.Get(&teamA, `INSERT INTO teams (organization_id, name) VALUES ($1,'team-a') RETURNING id::text`, orgID); err != nil {
		t.Fatalf("seed team A: %v", err)
	}
	if err := db.Get(&teamB, `INSERT INTO teams (organization_id, name) VALUES ($1,'team-b') RETURNING id::text`, orgID); err != nil {
		t.Fatalf("seed team B: %v", err)
	}
	if err := db.Get(&projectB, `INSERT INTO projects (organization_id, team_id, name) VALUES ($1,$2,'project-b') RETURNING id::text`, orgID, teamB); err != nil {
		t.Fatalf("seed project B: %v", err)
	}

	// A caller whose key belongs to team A tries to name team B's project.
	_, ok := resolveTeamProjectOverride(ctx, db, teamA, orgID, "project-b")
	if ok {
		t.Fatal("SECURITY REGRESSION: a project belonging to a different team in the same org resolved — this is the exact authorization bypass this fix closes")
	}
	// Also try by ID instead of name, in case only the name path was fixed.
	_, ok = resolveTeamProjectOverride(ctx, db, teamA, orgID, projectB)
	if ok {
		t.Fatal("SECURITY REGRESSION: a project belonging to a different team resolved by ID")
	}
}

// TestResolveTeamProjectOverride_AllowsOwnTeamProject proves the legitimate
// use case still works: selecting among your OWN team's projects.
func TestResolveTeamProjectOverride_AllowsOwnTeamProject(t *testing.T) {
	db := setupProjectOverrideTestDB(t)
	ctx := context.Background()

	var orgID, teamA, projectA string
	if err := db.Get(&orgID, `INSERT INTO organizations (name) VALUES ('org') RETURNING id::text`); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := db.Get(&teamA, `INSERT INTO teams (organization_id, name) VALUES ($1,'team-a') RETURNING id::text`, orgID); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	if err := db.Get(&projectA, `INSERT INTO projects (organization_id, team_id, name) VALUES ($1,$2,'project-a') RETURNING id::text`, orgID, teamA); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	proj, ok := resolveTeamProjectOverride(ctx, db, teamA, orgID, "project-a")
	if !ok || proj.ID != projectA {
		t.Fatalf("expected the caller's own team's project to resolve, got ok=%v proj=%+v", ok, proj)
	}
	// By ID too.
	proj, ok = resolveTeamProjectOverride(ctx, db, teamA, orgID, projectA)
	if !ok || proj.ID != projectA {
		t.Fatalf("expected resolution by ID to work identically, got ok=%v proj=%+v", ok, proj)
	}
}

// TestResolveTeamProjectOverride_RejectsInactiveProject proves a
// non-'active' project (e.g. archived) never resolves, even for the
// caller's own team.
func TestResolveTeamProjectOverride_RejectsInactiveProject(t *testing.T) {
	db := setupProjectOverrideTestDB(t)
	ctx := context.Background()

	var orgID, teamA string
	if err := db.Get(&orgID, `INSERT INTO organizations (name) VALUES ('org') RETURNING id::text`); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := db.Get(&teamA, `INSERT INTO teams (organization_id, name) VALUES ($1,'team-a') RETURNING id::text`, orgID); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (organization_id, team_id, name, status) VALUES ($1,$2,'archived-project','archived')`, orgID, teamA); err != nil {
		t.Fatalf("seed archived project: %v", err)
	}

	_, ok := resolveTeamProjectOverride(ctx, db, teamA, orgID, "archived-project")
	if ok {
		t.Fatal("expected an inactive project to never resolve via the override header")
	}
}

// TestResolveTeamProjectOverride_RejectsOtherOrgProject proves org isolation
// still holds — not just team isolation.
func TestResolveTeamProjectOverride_RejectsOtherOrgProject(t *testing.T) {
	db := setupProjectOverrideTestDB(t)
	ctx := context.Background()

	var orgA, orgB, teamA, teamInOrgB, projectInOrgB string
	if err := db.Get(&orgA, `INSERT INTO organizations (name) VALUES ('org-a') RETURNING id::text`); err != nil {
		t.Fatalf("seed org A: %v", err)
	}
	if err := db.Get(&orgB, `INSERT INTO organizations (name) VALUES ('org-b') RETURNING id::text`); err != nil {
		t.Fatalf("seed org B: %v", err)
	}
	if err := db.Get(&teamA, `INSERT INTO teams (organization_id, name) VALUES ($1,'team-a') RETURNING id::text`, orgA); err != nil {
		t.Fatalf("seed team A: %v", err)
	}
	if err := db.Get(&teamInOrgB, `INSERT INTO teams (organization_id, name) VALUES ($1,'team-in-b') RETURNING id::text`, orgB); err != nil {
		t.Fatalf("seed team in org B: %v", err)
	}
	if err := db.Get(&projectInOrgB, `INSERT INTO projects (organization_id, team_id, name) VALUES ($1,$2,'project-in-b') RETURNING id::text`, orgB, teamInOrgB); err != nil {
		t.Fatalf("seed project in org B: %v", err)
	}

	_, ok := resolveTeamProjectOverride(ctx, db, teamA, orgA, "project-in-b")
	if ok {
		t.Fatal("expected a project in a different organization to never resolve")
	}
	_ = projectInOrgB
}

// TestApplyProjectHeaderOverride_ProjectScopedKeyIgnoresHeader is the
// end-to-end regression test for the single most load-bearing guarantee of
// the X-Nexus-Project fix: a key that is ALREADY project-scoped must never
// have that scope changed by the header, even when the header names another
// valid, active project belonging to the caller's own team.
//
// Prior test coverage (the tests above) only exercised
// resolveTeamProjectOverride directly — the DB lookup — which implicitly
// assumes a team-only caller. None of them constructed a request with
// claims.ProjectID already set and asserted the header is ignored. This test
// exercises the actual HTTP-request-facing method
// (Handler.applyProjectHeaderOverride) that ChatCompletions calls, proving
// the `claims.ProjectID != ""` guard itself — not just the query it guards —
// actually fires end-to-end (production security re-audit).
func TestApplyProjectHeaderOverride_ProjectScopedKeyIgnoresHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupProjectOverrideTestDB(t)

	var orgID, teamA, ownProject string
	if err := db.Get(&orgID, `INSERT INTO organizations (name) VALUES ('org') RETURNING id::text`); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := db.Get(&teamA, `INSERT INTO teams (organization_id, name) VALUES ($1,'team-a') RETURNING id::text`, orgID); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	if err := db.Get(&ownProject, `INSERT INTO projects (organization_id, team_id, name) VALUES ($1,$2,'own-project') RETURNING id::text`, orgID, teamA); err != nil {
		t.Fatalf("seed own project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (organization_id, team_id, name) VALUES ($1,$2,'other-project')`, orgID, teamA); err != nil {
		t.Fatalf("seed other (same-team, otherwise-valid) project: %v", err)
	}

	h := &Handler{db: db}
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("X-Nexus-Project", "other-project")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	claims := &auth.TeamClaims{
		OrgID:     orgID,
		TeamID:    teamA,
		ProjectID: ownProject, // already project-scoped
	}

	result := h.applyProjectHeaderOverride(c, claims)
	if result.ProjectID != ownProject {
		t.Fatalf("SECURITY REGRESSION: an already project-scoped key's ProjectID changed from %q to %q via X-Nexus-Project — "+
			"a project-scoped key must never be able to override its own scope, even to another valid project of the same team",
			ownProject, result.ProjectID)
	}
	if result != claims {
		t.Fatal("expected the exact same claims to be returned unmodified for an already project-scoped key")
	}
}

// TestApplyProjectHeaderOverride_TeamOnlyKeyCanSelectOwnTeamProject proves
// the positive case still works end-to-end through the same method: a
// team-only key (no project_id) may attribute a request to one of its own
// team's projects via the header.
func TestApplyProjectHeaderOverride_TeamOnlyKeyCanSelectOwnTeamProject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupProjectOverrideTestDB(t)

	var orgID, teamA, projectA string
	if err := db.Get(&orgID, `INSERT INTO organizations (name) VALUES ('org') RETURNING id::text`); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := db.Get(&teamA, `INSERT INTO teams (organization_id, name) VALUES ($1,'team-a') RETURNING id::text`, orgID); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	if err := db.Get(&projectA, `INSERT INTO projects (organization_id, team_id, name) VALUES ($1,$2,'project-a') RETURNING id::text`, orgID, teamA); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	h := &Handler{db: db}
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("X-Nexus-Project", "project-a")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	claims := &auth.TeamClaims{OrgID: orgID, TeamID: teamA} // team-only key, no project_id

	result := h.applyProjectHeaderOverride(c, claims)
	if result.ProjectID != projectA {
		t.Fatalf("expected team-only key to pick up its own team's project via the header, got ProjectID=%q", result.ProjectID)
	}
}
