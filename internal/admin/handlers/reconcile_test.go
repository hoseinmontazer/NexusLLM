package handlers

// Regression tests for ReconcilePermissions (forensic audit, production-
// readiness round): detects and repairs Postgres/Redis drift in both
// directions for the project-authorization feature.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nexusllm/nexusllm/internal/policy"
	"github.com/redis/go-redis/v9"
)

// TestReconcilePermissions_AddsMissingTeamGrant proves under-permissive
// drift (Postgres has a grant, Redis doesn't — e.g. a failed post-commit
// sync) gets repaired.
func TestReconcilePermissions_AddsMissingTeamGrant(t *testing.T) {
	db, rdb := setupProjectModelsTestDB(t)
	projectID, modelName := seedProjectWithModel(t, db, true, "active")
	var teamID string
	if err := db.Get(&teamID, `SELECT team_id::text FROM projects WHERE id=$1::uuid`, projectID); err != nil {
		t.Fatalf("query team_id: %v", err)
	}
	// Grant directly in Postgres only — simulating a committed DB write
	// whose Redis sync never happened.
	var modelID string
	if err := db.Get(&modelID, `SELECT id::text FROM models WHERE name=$1`, modelName); err != nil {
		t.Fatalf("query model_id: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO team_model_permissions (team_id, model_id) VALUES ($1::uuid,$2::uuid)`, teamID, modelID); err != nil {
		t.Fatalf("seed team grant: %v", err)
	}

	engine := policy.NewEngine(rdb)
	report := ReconcilePermissions(context.Background(), db, engine, nil)
	if report.TeamRepairs != 1 {
		t.Fatalf("expected exactly 1 team repair, got %+v", report)
	}

	member, err := rdb.SIsMember(context.Background(), "nexus:team:"+teamID+":models", modelName).Result()
	if err != nil || !member {
		t.Fatalf("expected the missing grant to be repaired into Redis, member=%v err=%v", member, err)
	}
}

// TestReconcilePermissions_RemovesStrayTeamGrant proves over-permissive
// drift (Redis has a grant Postgres no longer has — e.g. a failed revoke
// sync) gets repaired. This is the more security-relevant direction: it
// means the gateway is enforcing access nobody currently grants.
func TestReconcilePermissions_RemovesStrayTeamGrant(t *testing.T) {
	db, rdb := setupProjectModelsTestDB(t)
	projectID, modelName := seedProjectWithModel(t, db, true, "active")
	var teamID string
	if err := db.Get(&teamID, `SELECT team_id::text FROM projects WHERE id=$1::uuid`, projectID); err != nil {
		t.Fatalf("query team_id: %v", err)
	}

	engine := policy.NewEngine(rdb)
	// Redis has the grant, Postgres never did (or it was since revoked and
	// the revoke's Redis removal failed).
	if err := engine.SetModelAllowed(context.Background(), teamID, modelName); err != nil {
		t.Fatalf("seed stray Redis grant: %v", err)
	}

	report := ReconcilePermissions(context.Background(), db, engine, nil)
	// This team has ZERO team_model_permissions rows at all (no other model
	// was ever granted), so it won't appear in the "teams checked" set at
	// all under the current scoping (reconciliation iterates teams that
	// have at least one Postgres row). Seed a DIFFERENT model as a real
	// grant so the team is in scope, proving the stray one still gets
	// removed alongside it.
	_ = report

	otherModelName := "model-" + uuid.New().String()[:8]
	if _, err := db.Exec(`INSERT INTO models (name, enabled, lifecycle) VALUES ($1,TRUE,'active')`, otherModelName); err != nil {
		t.Fatalf("seed other model: %v", err)
	}
	var otherModelID string
	if err := db.Get(&otherModelID, `SELECT id::text FROM models WHERE name=$1`, otherModelName); err != nil {
		t.Fatalf("query other model id: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO team_model_permissions (team_id, model_id) VALUES ($1::uuid,$2::uuid)`, teamID, otherModelID); err != nil {
		t.Fatalf("seed real grant: %v", err)
	}
	// Keep this one already in sync so the ONLY diff reconciliation finds is
	// the stray grant being tested.
	if err := engine.SetModelAllowed(context.Background(), teamID, otherModelName); err != nil {
		t.Fatalf("pre-sync real grant: %v", err)
	}

	report = ReconcilePermissions(context.Background(), db, engine, nil)
	if report.TeamRepairs != 1 {
		t.Fatalf("expected exactly 1 repair (removing the stray grant), got %+v", report)
	}

	member, err := rdb.SIsMember(context.Background(), "nexus:team:"+teamID+":models", modelName).Result()
	if err != nil || member {
		t.Fatalf("expected the stray grant to be removed from Redis, member=%v err=%v", member, err)
	}
	stillThere, err := rdb.SIsMember(context.Background(), "nexus:team:"+teamID+":models", otherModelName).Result()
	if err != nil || !stillThere {
		t.Fatalf("expected the real grant to remain untouched, member=%v err=%v", stillThere, err)
	}
}

// TestReconcilePermissions_ProjectDrift proves the same bidirectional repair
// for project grants.
func TestReconcilePermissions_ProjectDrift(t *testing.T) {
	db, rdb := setupProjectModelsTestDB(t)
	projectID, modelName := seedProjectWithModel(t, db, true, "active")
	var modelID string
	if err := db.Get(&modelID, `SELECT id::text FROM models WHERE name=$1`, modelName); err != nil {
		t.Fatalf("query model_id: %v", err)
	}
	// Postgres has it, Redis doesn't.
	if _, err := db.Exec(`INSERT INTO project_model_permissions (project_id, model_id) VALUES ($1::uuid,$2::uuid)`, projectID, modelID); err != nil {
		t.Fatalf("seed project grant: %v", err)
	}

	engine := policy.NewEngine(rdb)
	report := ReconcilePermissions(context.Background(), db, engine, nil)
	if report.ProjectRepairs != 1 {
		t.Fatalf("expected exactly 1 project repair, got %+v", report)
	}
	member, err := rdb.SIsMember(context.Background(), "nexus:project:"+projectID+":models", modelName).Result()
	if err != nil || !member {
		t.Fatalf("expected the missing project grant to be repaired, member=%v err=%v", member, err)
	}
}

// TestReconcilePermissions_LeavesUnconfiguredProjectAlone proves a project
// with zero Postgres rows is never touched — reconciliation must not turn a
// legitimate "never configured" passthrough project into an accidental
// "configured, empty" deny-all project.
func TestReconcilePermissions_LeavesUnconfiguredProjectAlone(t *testing.T) {
	db, rdb := setupProjectModelsTestDB(t)
	projectID, _ := seedProjectWithModel(t, db, true, "active")
	// No grants made for this project at all — zero project_model_permissions rows.

	engine := policy.NewEngine(rdb)
	ReconcilePermissions(context.Background(), db, engine, nil)

	configured, err := rdb.Exists(context.Background(), "nexus:project:"+projectID+":models:configured").Result()
	if err != nil {
		t.Fatalf("check configured marker: %v", err)
	}
	if configured != 0 {
		t.Fatal("expected reconciliation to leave an unconfigured project's passthrough state untouched, but it created a 'configured' marker")
	}
}

// TestReconcilePermissions_RepairsPostCommitRedisGap ties the restore fix
// (P0.2) and the reconciliation sweep (P1) together end-to-end: a restore
// whose Redis sync failed leaves a real gap that ordinary retries of
// restorePermissionsFromSnapshot cannot fix (the snapshot is already
// consumed) — only reconciliation can.
func TestReconcilePermissions_RepairsPostCommitRedisGap(t *testing.T) {
	env := setupRestoreTestEnv(t)
	modelName := "reconcile-" + uuid.New().String()[:8]
	teamID := seedDeletedModelWithSnapshot(t, env.db, modelName)
	newModelID := insertRedeployedModel(t, env.db, modelName)

	deadRdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	deadEngine := policy.NewEngine(deadRdb)

	if _, err := restorePermissionsFromSnapshot(context.Background(), env.db, deadEngine, nil, modelName, newModelID); err == nil {
		t.Fatal("expected the restore to report a Redis sync error")
	}

	// Confirm the gap exists: DB has it, Redis doesn't.
	member, _ := env.rdb.SIsMember(context.Background(), "nexus:team:"+teamID+":models", modelName).Result()
	if member {
		t.Fatal("test setup invariant violated: Redis should not have the grant yet")
	}

	// Reconciliation (against the REAL Redis this time) must repair it.
	realEngine := policy.NewEngine(env.rdb)
	report := ReconcilePermissions(context.Background(), env.db, realEngine, nil)
	if report.TeamRepairs != 1 {
		t.Fatalf("expected reconciliation to repair exactly 1 team grant, got %+v", report)
	}
	member, err := env.rdb.SIsMember(context.Background(), "nexus:team:"+teamID+":models", modelName).Result()
	if err != nil || !member {
		t.Fatalf("expected reconciliation to close the post-commit Redis gap left by the failed restore, member=%v err=%v", member, err)
	}
}

// TestReconcilePermissions_RepairsTeamRevokeToZeroWithFailedRedisSync is the
// regression test for Critical Finding #2 (production security re-audit): a
// team revoked down to ZERO permission rows, where the revoke's Redis
// removal failed, must still be found and repaired by reconciliation — even
// though it now has no rows in team_model_permissions for the row-based scan
// to find it by. This is exactly the state model_permission_scopes
// (migration 059) exists to keep visible.
func TestReconcilePermissions_RepairsTeamRevokeToZeroWithFailedRedisSync(t *testing.T) {
	env := setupRestoreTestEnv(t)
	ctx := context.Background()

	var orgID, teamID string
	if err := env.db.Get(&orgID, `INSERT INTO organizations (name) VALUES ($1) RETURNING id::text`, "org-revoke-zero"); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := env.db.Get(&teamID, `INSERT INTO teams (org_id, name) VALUES ($1,$2) RETURNING id::text`, orgID, "team-revoke-zero"); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	modelName := "revoke-zero-" + uuid.New().String()[:8]
	var modelID string
	if err := env.db.Get(&modelID, `INSERT INTO models (name, display_name) VALUES ($1,$1) RETURNING id::text`, modelName); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if _, err := env.db.Exec(`INSERT INTO team_model_permissions (team_id, model_id) VALUES ($1::uuid,$2::uuid)`, teamID, modelID); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	realEngine := policy.NewEngine(env.rdb)
	if err := realEngine.SetModelAllowed(ctx, teamID, modelName); err != nil {
		t.Fatalf("pre-sync grant to redis: %v", err)
	}

	// Revoke through the real handler, wired to a DEAD Redis client so the
	// Postgres DELETE (and the model_permission_scopes marker write) commit
	// while the Redis removal fails — leaving a stray grant in the REAL
	// Redis, exactly like a transient outage during a live revoke would.
	deadRdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	deadEngine := policy.NewEngine(deadRdb)
	h := NewTeamHandler(env.db, deadRdb, deadEngine)
	c, w := projectModelsTestContext("DELETE", "/", nil)
	c.Params = gin.Params{{Key: "id", Value: teamID}, {Key: "model", Value: modelName}}
	h.RemoveModelPermission(c)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected the revoke to report the Redis sync failure (500), got %d: %s", w.Code, w.Body.String())
	}

	// Confirm the exact drifted state: zero Postgres rows, stray Redis entry.
	var count int
	if err := env.db.Get(&count, `SELECT COUNT(*) FROM team_model_permissions WHERE team_id=$1::uuid`, teamID); err != nil {
		t.Fatalf("query team_model_permissions: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected zero team_model_permissions rows after revoke, got %d", count)
	}
	member, err := env.rdb.SIsMember(ctx, "nexus:team:"+teamID+":models", modelName).Result()
	if err != nil || !member {
		t.Fatalf("test setup invariant violated: expected the stray Redis grant to still be present, member=%v err=%v", member, err)
	}

	// Reconciliation, against the REAL Redis, must find and remove it — even
	// though this team now has zero rows in team_model_permissions.
	report := ReconcilePermissions(ctx, env.db, realEngine, nil)
	if report.TeamRepairs != 1 {
		t.Fatalf("expected reconciliation to repair exactly 1 stray team grant, got %+v", report)
	}
	member, err = env.rdb.SIsMember(ctx, "nexus:team:"+teamID+":models", modelName).Result()
	if err != nil || member {
		t.Fatalf("expected the stray grant to be removed after reconciliation, member=%v err=%v", member, err)
	}
}

// TestReconcilePermissions_RepairsProjectRevokeToZeroWithFailedRedisSync is
// the project-side twin of the team test above: a project revoked down to
// zero project_model_permissions rows, with a failed Redis removal, must
// still be swept and repaired.
func TestReconcilePermissions_RepairsProjectRevokeToZeroWithFailedRedisSync(t *testing.T) {
	env := setupRestoreTestEnv(t)
	ctx := context.Background()

	var orgID, teamID, projectID string
	if err := env.db.Get(&orgID, `INSERT INTO organizations (name) VALUES ($1) RETURNING id::text`, "org-proj-revoke-zero"); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := env.db.Get(&teamID, `INSERT INTO teams (org_id, name) VALUES ($1,$2) RETURNING id::text`, orgID, "team-proj-revoke-zero"); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	if err := env.db.Get(&projectID, `INSERT INTO projects (team_id, name) VALUES ($1,$2) RETURNING id::text`, teamID, "proj-revoke-zero"); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	modelName := "proj-revoke-zero-" + uuid.New().String()[:8]
	var modelID string
	if err := env.db.Get(&modelID, `INSERT INTO models (name, display_name) VALUES ($1,$1) RETURNING id::text`, modelName); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if _, err := env.db.Exec(`INSERT INTO project_model_permissions (project_id, model_id) VALUES ($1::uuid,$2::uuid)`, projectID, modelID); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	realEngine := policy.NewEngine(env.rdb)
	if err := realEngine.SetProjectModelAllowed(ctx, projectID, modelName); err != nil {
		t.Fatalf("pre-sync grant to redis: %v", err)
	}

	deadRdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	deadEngine := policy.NewEngine(deadRdb)
	h := NewProjectHandler(env.db, deadRdb, deadEngine)
	c, w := projectModelsTestContext("DELETE", "/", nil)
	c.Params = gin.Params{{Key: "id", Value: projectID}, {Key: "model", Value: modelName}}
	h.RemoveProjectModelPermission(c)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected the revoke to report the Redis sync failure (500), got %d: %s", w.Code, w.Body.String())
	}

	var count int
	if err := env.db.Get(&count, `SELECT COUNT(*) FROM project_model_permissions WHERE project_id=$1::uuid`, projectID); err != nil {
		t.Fatalf("query project_model_permissions: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected zero project_model_permissions rows after revoke, got %d", count)
	}
	member, err := env.rdb.SIsMember(ctx, "nexus:project:"+projectID+":models", modelName).Result()
	if err != nil || !member {
		t.Fatalf("test setup invariant violated: expected the stray Redis grant to still be present, member=%v err=%v", member, err)
	}

	report := ReconcilePermissions(ctx, env.db, realEngine, nil)
	if report.ProjectRepairs != 1 {
		t.Fatalf("expected reconciliation to repair exactly 1 stray project grant, got %+v", report)
	}
	member, err = env.rdb.SIsMember(ctx, "nexus:project:"+projectID+":models", modelName).Result()
	if err != nil || member {
		t.Fatalf("expected the stray grant to be removed after reconciliation, member=%v err=%v", member, err)
	}
}
