package handlers

// Regression tests for the project-level model authorization admin API
// (POST/DELETE/GET /admin/v1/projects/:id/models) — the grant/revoke/list
// surface backing internal/policy/engine.go's Option-A enforcement.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/nexusllm/nexusllm/internal/policy"
	"github.com/redis/go-redis/v9"
)

var projectModelsGinModeOnce sync.Once

func setProjectModelsGinTestMode() {
	projectModelsGinModeOnce.Do(func() { gin.SetMode(gin.TestMode) })
}

func setupProjectModelsTestDB(t *testing.T) (*sqlx.DB, *redis.Client) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available — skipping project model permission integration tests")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable — skipping project model permission integration tests")
	}

	suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:8]
	pgName := "nexus-test-projmodels-pg-" + suffix
	redisName := "nexus-test-projmodels-redis-" + suffix
	pgPort := 16200 + int(time.Now().UnixNano()%2000)
	redisPort := 16400 + int(time.Now().UnixNano()%2000)

	pgRun := exec.Command("docker", "run", "-d", "--rm", "--name", pgName,
		"-e", "POSTGRES_PASSWORD=test", "-e", "POSTGRES_DB=test",
		"-p", fmt.Sprintf("%d:5432", pgPort), "postgres:15-alpine")
	if out, err := pgRun.CombinedOutput(); err != nil {
		t.Skipf("could not start disposable postgres container (%v): %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", pgName).Run() })

	redisRun := exec.Command("docker", "run", "-d", "--rm", "--name", redisName,
		"-p", fmt.Sprintf("%d:6379", redisPort), "redis:7-alpine")
	if out, err := redisRun.CombinedOutput(); err != nil {
		t.Skipf("could not start disposable redis container (%v): %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", redisName).Run() })

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

	rdb := redis.NewClient(&redis.Options{Addr: fmt.Sprintf("127.0.0.1:%d", redisPort)})
	rdeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(rdeadline) {
		if rdb.Ping(context.Background()).Err() == nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	if _, err := db.Exec(`
		CREATE EXTENSION IF NOT EXISTS pgcrypto;

		CREATE TABLE IF NOT EXISTS organizations (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(), name VARCHAR(255) NOT NULL
		);
		CREATE TABLE IF NOT EXISTS teams (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			org_id UUID NOT NULL REFERENCES organizations(id),
			name VARCHAR(255) NOT NULL
		);
		CREATE TABLE IF NOT EXISTS projects (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			team_id UUID NOT NULL REFERENCES teams(id),
			name VARCHAR(255) NOT NULL
		);
		CREATE TABLE IF NOT EXISTS models (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			name VARCHAR(255) NOT NULL UNIQUE,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			lifecycle VARCHAR(30) NOT NULL DEFAULT 'active',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE TABLE IF NOT EXISTS project_model_permissions (
			project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			model_id   UUID NOT NULL REFERENCES models(id)   ON DELETE RESTRICT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (project_id, model_id)
		);
		CREATE TABLE IF NOT EXISTS team_model_permissions (
			team_id  UUID NOT NULL REFERENCES teams(id)  ON DELETE CASCADE,
			model_id UUID NOT NULL REFERENCES models(id) ON DELETE RESTRICT,
			PRIMARY KEY (team_id, model_id)
		);
		CREATE TABLE IF NOT EXISTS model_permission_scopes (
			scope_type    TEXT NOT NULL CHECK (scope_type IN ('team', 'project')),
			scope_id      UUID NOT NULL,
			configured_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (scope_type, scope_id)
		);
		CREATE TABLE IF NOT EXISTS api_keys (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			team_id UUID REFERENCES teams(id),
			project_id UUID REFERENCES projects(id),
			key_hash VARCHAR(64) NOT NULL UNIQUE,
			active BOOLEAN NOT NULL DEFAULT TRUE
		);
	`); err != nil {
		t.Fatalf("schema setup: %v", err)
	}
	return db, rdb
}

func newProjectModelsHandler(db *sqlx.DB, rdb *redis.Client) *ProjectHandler {
	return NewProjectHandler(db, rdb, policy.NewEngine(rdb))
}

func seedProjectWithModel(t *testing.T, db *sqlx.DB, modelEnabled bool, modelLifecycle string) (projectID, modelName string) {
	t.Helper()
	var orgID, teamID string
	if err := db.Get(&orgID, `INSERT INTO organizations (name) VALUES ($1) RETURNING id::text`, "org-"+uuid.New().String()[:8]); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := db.Get(&teamID, `INSERT INTO teams (org_id, name) VALUES ($1,$2) RETURNING id::text`, orgID, "team-"+uuid.New().String()[:8]); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	if err := db.Get(&projectID, `INSERT INTO projects (team_id, name) VALUES ($1,$2) RETURNING id::text`, teamID, "proj-"+uuid.New().String()[:8]); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	modelName = "model-" + uuid.New().String()[:8]
	if _, err := db.Exec(`INSERT INTO models (name, enabled, lifecycle) VALUES ($1,$2,$3)`, modelName, modelEnabled, modelLifecycle); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	return projectID, modelName
}

func projectModelsTestContext(method, path string, body interface{}) (*gin.Context, *httptest.ResponseRecorder) {
	setProjectModelsGinTestMode()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	c.Request = httptest.NewRequest(method, path, bytes.NewReader(b))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, w
}

// TestAddAndListProjectModelPermission proves the basic grant → list flow.
func TestAddAndListProjectModelPermission(t *testing.T) {
	db, rdb := setupProjectModelsTestDB(t)
	h := newProjectModelsHandler(db, rdb)
	projectID, modelName := seedProjectWithModel(t, db, true, "active")

	c, w := projectModelsTestContext("POST", "/", map[string]string{"model_name": modelName})
	c.Params = gin.Params{{Key: "id", Value: projectID}}
	h.AddProjectModelPermission(c)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Redis must reflect the grant immediately.
	member, err := rdb.SIsMember(context.Background(), "nexus:project:"+projectID+":models", modelName).Result()
	if err != nil || !member {
		t.Fatalf("expected model to be a member of the project's Redis ACL set, member=%v err=%v", member, err)
	}

	lc, lw := projectModelsTestContext("GET", "/", nil)
	lc.Params = gin.Params{{Key: "id", Value: projectID}}
	h.ListProjectModels(lc)
	if lw.Code != 200 {
		t.Fatalf("expected 200 from list, got %d", lw.Code)
	}
	var resp struct {
		Models []ProjectModelGrant `json:"models"`
	}
	if err := json.Unmarshal(lw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal list response: %v", err)
	}
	if len(resp.Models) != 1 || resp.Models[0].Name != modelName {
		t.Fatalf("expected list to show exactly [%s], got %v", modelName, resp.Models)
	}
	if !resp.Models[0].Synced {
		t.Fatalf("expected the grant to be reported synced (Redis already confirmed a member above), got %+v", resp.Models[0])
	}
}

// TestRemoveProjectModelPermission proves revoke removes from both Postgres
// and Redis, and leaves the project in deny-all (not passthrough) mode.
func TestRemoveProjectModelPermission(t *testing.T) {
	db, rdb := setupProjectModelsTestDB(t)
	h := newProjectModelsHandler(db, rdb)
	projectID, modelName := seedProjectWithModel(t, db, true, "active")

	c, _ := projectModelsTestContext("POST", "/", map[string]string{"model_name": modelName})
	c.Params = gin.Params{{Key: "id", Value: projectID}}
	h.AddProjectModelPermission(c)

	dc, dw := projectModelsTestContext("DELETE", "/", nil)
	dc.Params = gin.Params{{Key: "id", Value: projectID}, {Key: "model", Value: modelName}}
	h.RemoveProjectModelPermission(dc)
	if dw.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", dw.Code, dw.Body.String())
	}

	member, _ := rdb.SIsMember(context.Background(), "nexus:project:"+projectID+":models", modelName).Result()
	if member {
		t.Fatal("expected model to be removed from the project's Redis ACL set")
	}
	configured, _ := rdb.Exists(context.Background(), "nexus:project:"+projectID+":models:configured").Result()
	if configured == 0 {
		t.Fatal("expected the project to remain marked 'configured' after revoking its only grant — must not silently revert to legacy team-passthrough")
	}
}

// TestAddProjectModelPermission_RejectsDeletedModel proves deleted models
// cannot become grantable (Phase 13 "model lifecycle" requirement).
func TestAddProjectModelPermission_RejectsDeletedModel(t *testing.T) {
	db, rdb := setupProjectModelsTestDB(t)
	h := newProjectModelsHandler(db, rdb)
	projectID, modelName := seedProjectWithModel(t, db, true, "deleted")

	c, w := projectModelsTestContext("POST", "/", map[string]string{"model_name": modelName})
	c.Params = gin.Params{{Key: "id", Value: projectID}}
	h.AddProjectModelPermission(c)

	if w.Code != 404 {
		t.Fatalf("expected 404 for a soft-deleted model, got %d: %s", w.Code, w.Body.String())
	}
	var count int
	if err := db.Get(&count, `SELECT COUNT(*) FROM project_model_permissions WHERE project_id=$1::uuid`, projectID); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if count != 0 {
		t.Fatal("expected no grant row to be created for a deleted model")
	}
}

// TestAddProjectModelPermission_RedisFailureDoesNotReturn200 proves
// requirement #8: if the Redis sync fails, the admin API must not claim
// success.
func TestAddProjectModelPermission_RedisFailureDoesNotReturn200(t *testing.T) {
	db, _ := setupProjectModelsTestDB(t)
	// A client pointed at nothing — SetProjectModelAllowed's SAdd/Set pipeline
	// will fail immediately.
	deadRdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	h := newProjectModelsHandler(db, deadRdb)
	projectID, modelName := seedProjectWithModel(t, db, true, "active")

	c, w := projectModelsTestContext("POST", "/", map[string]string{"model_name": modelName})
	c.Params = gin.Params{{Key: "id", Value: projectID}}
	h.AddProjectModelPermission(c)

	if w.Code == 200 {
		t.Fatalf("expected a non-200 status when Redis sync fails, got 200: %s", w.Body.String())
	}
}

// TestListProjectModels_ReportsUnsyncedGrant proves the sync-state
// indicator (production-readiness audit, P2 frontend requirement): a grant
// that exists in Postgres but never made it into Redis (e.g. the write path
// above, or a restore whose post-commit sync failed) must be reported as
// unsynced, not silently shown as a normal active grant.
func TestListProjectModels_ReportsUnsyncedGrant(t *testing.T) {
	db, rdb := setupProjectModelsTestDB(t)
	h := newProjectModelsHandler(db, rdb)
	projectID, modelName := seedProjectWithModel(t, db, true, "active")

	var modelID string
	if err := db.Get(&modelID, `SELECT id::text FROM models WHERE name=$1`, modelName); err != nil {
		t.Fatalf("query model id: %v", err)
	}
	// Insert the Postgres grant directly, bypassing SetProjectModelAllowed —
	// simulates exactly the post-commit-Redis-sync-failed state.
	if _, err := db.Exec(`INSERT INTO project_model_permissions (project_id, model_id) VALUES ($1::uuid,$2::uuid)`, projectID, modelID); err != nil {
		t.Fatalf("seed unsynced grant: %v", err)
	}

	c, w := projectModelsTestContext("GET", "/", nil)
	c.Params = gin.Params{{Key: "id", Value: projectID}}
	h.ListProjectModels(c)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Models []ProjectModelGrant `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Models) != 1 || resp.Models[0].Name != modelName {
		t.Fatalf("expected exactly [%s], got %v", modelName, resp.Models)
	}
	if resp.Models[0].Synced {
		t.Fatal("expected the grant to be reported as NOT synced — Postgres has it but Redis never did")
	}
}
