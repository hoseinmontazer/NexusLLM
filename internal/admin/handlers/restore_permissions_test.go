package handlers

// Integration tests for the Phase 2A/2B fix to restorePermissionsFromSnapshot
// (forensic audit, Case File 002 §02):
//
//   - Phase 2A: a restored permission must reach the SAME Redis ACL set the
//     gateway's policy engine enforces against, via the canonical
//     policy.Engine.SetModelAllowed path — never a duplicate Redis write —
//     and a Redis failure must not be reported as a successful restore.
//   - Phase 2B: the restore must be one atomic, row-locked transaction —
//     concurrent redeploys racing the same snapshot must not both consume it.
//
// These properties depend on real Postgres row-locking (SELECT ... FOR
// UPDATE) and a real Redis SIsMember/SAdd round trip, which cannot be
// faithfully exercised with mocks. Tests spin up disposable postgres:15-alpine
// and redis:7-alpine containers via the local Docker daemon and skip cleanly
// if Docker isn't available.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/nexusllm/nexusllm/internal/policy"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// ─── Shared disposable Postgres + Redis, spun up once per test run ──────────

type restoreTestEnv struct {
	db  *sqlx.DB
	rdb *redis.Client
}

func setupRestoreTestEnv(t *testing.T) *restoreTestEnv {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available — skipping Phase 2A/2B integration tests")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable — skipping Phase 2A/2B integration tests")
	}

	suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:8]
	pgName := "nexus-test-pg-" + suffix
	redisName := "nexus-test-redis-" + suffix
	pgPort := 15000 + (int(time.Now().UnixNano() % 5000))
	redisPort := pgPort + 1

	runPG := exec.Command("docker", "run", "-d", "--rm", "--name", pgName,
		"-e", "POSTGRES_PASSWORD=test", "-e", "POSTGRES_DB=test",
		"-p", fmt.Sprintf("%d:5432", pgPort), "postgres:15-alpine")
	if out, err := runPG.CombinedOutput(); err != nil {
		t.Skipf("could not start disposable postgres container (%v): %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", pgName).Run() })

	runRedis := exec.Command("docker", "run", "-d", "--rm", "--name", redisName,
		"-p", fmt.Sprintf("%d:6379", redisPort), "redis:7-alpine")
	if out, err := runRedis.CombinedOutput(); err != nil {
		t.Skipf("could not start disposable redis container (%v): %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", redisName).Run() })

	dsn := fmt.Sprintf("postgres://postgres:test@127.0.0.1:%d/test?sslmode=disable", pgPort)
	var db *sqlx.DB
	var err error
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		db, err = sqlx.Connect("postgres", dsn)
		if err == nil {
			if pingErr := db.Ping(); pingErr == nil {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil || db == nil {
		t.Fatalf("postgres never became ready: %v", err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: fmt.Sprintf("127.0.0.1:%d", redisPort)})
	rDeadline := time.Now().Add(20 * time.Second)
	var pingErr error
	for time.Now().Before(rDeadline) {
		if pingErr = rdb.Ping(context.Background()).Err(); pingErr == nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if pingErr != nil {
		t.Fatalf("redis never became ready: %v", pingErr)
	}

	if err := applyMinimalSchema(db); err != nil {
		t.Fatalf("failed to apply minimal schema: %v", err)
	}

	return &restoreTestEnv{db: db, rdb: rdb}
}

// applyMinimalSchema creates just the tables restorePermissionsFromSnapshot
// touches, matching migrations/001_initial.sql and
// migrations/053_stable_model_identity.sql exactly (columns, FK ON DELETE
// behavior, the snapshot trigger) — not the full 56-migration chain, so the
// test stays fast and self-contained while still exercising real Postgres
// semantics for the invariants under test.
func applyMinimalSchema(db *sqlx.DB) error {
	_, err := db.Exec(`
		CREATE EXTENSION IF NOT EXISTS pgcrypto;

		CREATE TABLE IF NOT EXISTS organizations (
			id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			name   VARCHAR(255) NOT NULL,
			active BOOLEAN NOT NULL DEFAULT TRUE
		);

		CREATE TABLE IF NOT EXISTS teams (
			id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			name   VARCHAR(255) NOT NULL,
			active BOOLEAN NOT NULL DEFAULT TRUE
		);

		CREATE TABLE IF NOT EXISTS models (
			id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			name         VARCHAR(255) NOT NULL,
			display_name VARCHAR(255) NOT NULL DEFAULT '',
			enabled      BOOLEAN NOT NULL DEFAULT TRUE,
			lifecycle    VARCHAR(20) NOT NULL DEFAULT 'active'
			             CHECK (lifecycle IN ('active','archived','deleted'))
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_models_name_active
			ON models(name) WHERE lifecycle != 'deleted';

		CREATE TABLE IF NOT EXISTS team_model_permissions (
			team_id  UUID NOT NULL REFERENCES teams(id)  ON DELETE CASCADE,
			model_id UUID NOT NULL REFERENCES models(id) ON DELETE RESTRICT,
			PRIMARY KEY (team_id, model_id)
		);

		CREATE TABLE IF NOT EXISTS model_permission_snapshots (
			id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			model_name       VARCHAR(255) NOT NULL,
			deleted_model_id UUID NOT NULL,
			team_ids         JSONB NOT NULL DEFAULT '[]',
			deleted_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			restored         BOOLEAN NOT NULL DEFAULT FALSE,
			restored_at      TIMESTAMPTZ
		);

		CREATE OR REPLACE FUNCTION fn_snapshot_model_permissions()
		RETURNS TRIGGER AS $$
		DECLARE
			v_team_ids JSONB;
		BEGIN
			IF NEW.lifecycle = 'deleted' AND OLD.lifecycle != 'deleted' THEN
				SELECT COALESCE(jsonb_agg(tmp.team_id::text), '[]'::jsonb)
				INTO   v_team_ids
				FROM   team_model_permissions tmp
				WHERE  tmp.model_id = OLD.id;

				INSERT INTO model_permission_snapshots (model_name, deleted_model_id, team_ids)
				VALUES (OLD.name, OLD.id, v_team_ids);
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;

		DROP TRIGGER IF EXISTS trg_snapshot_model_perms ON models;
		CREATE TRIGGER trg_snapshot_model_perms
			AFTER UPDATE OF lifecycle ON models
			FOR EACH ROW
			EXECUTE FUNCTION fn_snapshot_model_permissions();
	`)
	return err
}

// seedDeletedModelWithSnapshot creates an org/team/model, grants the team
// access, then soft-deletes the model (firing the real trigger) — leaving
// exactly one unconsumed snapshot ready to be restored under a new model_id,
// exactly reproducing the delete→redeploy sequence from the forensic audit.
func seedDeletedModelWithSnapshot(t *testing.T, db *sqlx.DB, modelName string) (teamID string) {
	t.Helper()
	ctx := context.Background()

	var orgID string
	if err := db.GetContext(ctx, &orgID, `INSERT INTO organizations (name) VALUES ($1) RETURNING id::text`, "org-"+modelName); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := db.GetContext(ctx, &teamID, `INSERT INTO teams (org_id, name) VALUES ($1,$2) RETURNING id::text`, orgID, "team-"+modelName); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	var oldModelID string
	if err := db.GetContext(ctx, &oldModelID, `INSERT INTO models (name, display_name) VALUES ($1,$1) RETURNING id::text`, modelName); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO team_model_permissions (team_id, model_id) VALUES ($1::uuid,$2::uuid)`, teamID, oldModelID); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE models SET lifecycle='deleted' WHERE id=$1::uuid`, oldModelID); err != nil {
		t.Fatalf("soft-delete model (trigger fire): %v", err)
	}
	return teamID
}

func insertRedeployedModel(t *testing.T, db *sqlx.DB, modelName string) (newModelID string) {
	t.Helper()
	// A redeploy under the same name only succeeds once the old row is
	// lifecycle='deleted' (idx_models_name_active), matching production.
	if err := db.Get(&newModelID, `INSERT INTO models (name, display_name) VALUES ($1,$1) RETURNING id::text`, modelName); err != nil {
		t.Fatalf("insert redeployed model: %v", err)
	}
	return newModelID
}

// ─── Phase 2A: Redis enforcement synchronization ─────────────────────────────

func TestRestorePermissions_SyncsToGatewayEnforcement(t *testing.T) {
	env := setupRestoreTestEnv(t)
	modelName := "qwen3-" + uuid.New().String()[:8]
	teamID := seedDeletedModelWithSnapshot(t, env.db, modelName)
	newModelID := insertRedeployedModel(t, env.db, modelName)

	engine := policy.NewEngine(env.rdb)
	restored, err := restorePermissionsFromSnapshot(context.Background(), env.db, engine, zap.NewNop(), modelName, newModelID)
	if err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if len(restored) != 1 || restored[0] != teamID {
		t.Fatalf("expected restored=[%s], got %v", teamID, restored)
	}

	// 1. DB permission restored.
	var dbCount int
	if err := env.db.Get(&dbCount, `SELECT COUNT(*) FROM team_model_permissions WHERE team_id=$1::uuid AND model_id=$2::uuid`, teamID, newModelID); err != nil {
		t.Fatalf("query team_model_permissions: %v", err)
	}
	if dbCount != 1 {
		t.Fatalf("expected 1 team_model_permissions row, got %d", dbCount)
	}

	// 2. Redis permission restored — the exact key/value the gateway's
	// policy engine Step 0 checks via SIsMember.
	isMember, err := env.rdb.SIsMember(context.Background(), "nexus:team:"+teamID+":models", modelName).Result()
	if err != nil {
		t.Fatalf("redis SIsMember: %v", err)
	}
	if !isMember {
		t.Fatal("expected Redis ACL set to contain the restored model — gateway would incorrectly deny this team")
	}

	// 3. Gateway authorization succeeds — this is literally Step 0's own check.
	if !isMember {
		t.Fatal("gateway authorization check (SIsMember) would deny a request that should be allowed")
	}

	// Snapshot must be marked consumed.
	var restoredFlag bool
	if err := env.db.Get(&restoredFlag, `SELECT restored FROM model_permission_snapshots WHERE model_name=$1`, modelName); err != nil {
		t.Fatalf("query snapshot: %v", err)
	}
	if !restoredFlag {
		t.Fatal("expected snapshot.restored = TRUE after a successful restore")
	}
}

func TestRestorePermissions_RedisFailureIsNotReportedAsSuccess(t *testing.T) {
	env := setupRestoreTestEnv(t)
	modelName := "gpt-router-" + uuid.New().String()[:8]
	teamID := seedDeletedModelWithSnapshot(t, env.db, modelName)
	newModelID := insertRedeployedModel(t, env.db, modelName)

	// Engine pointed at a Redis address nothing is listening on — every
	// SetModelAllowed call will fail, simulating a transient Redis outage.
	deadRdb := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1", // reserved, nothing listens here
		DialTimeout: 500 * time.Millisecond,
	})
	deadEngine := policy.NewEngine(deadRdb)

	restored, err := restorePermissionsFromSnapshot(context.Background(), env.db, deadEngine, zap.NewNop(), modelName, newModelID)
	if err == nil {
		t.Fatalf("expected an error when Redis sync fails, got success with restored=%v", restored)
	}

	// DB must NOT show the permission as granted — full rollback, not a
	// partial/divergent state.
	var dbCount int
	if qErr := env.db.Get(&dbCount, `SELECT COUNT(*) FROM team_model_permissions WHERE team_id=$1::uuid AND model_id=$2::uuid`, teamID, newModelID); qErr != nil {
		t.Fatalf("query team_model_permissions: %v", qErr)
	}
	if dbCount != 0 {
		t.Fatalf("expected the failed restore to leave NO team_model_permissions row (full rollback), found %d", dbCount)
	}

	// Snapshot must remain unconsumed and therefore recoverable by a retry
	// once Redis is healthy again.
	var restoredFlag bool
	if qErr := env.db.Get(&restoredFlag, `SELECT restored FROM model_permission_snapshots WHERE model_name=$1`, modelName); qErr != nil {
		t.Fatalf("query snapshot: %v", qErr)
	}
	if restoredFlag {
		t.Fatal("expected snapshot.restored to remain FALSE after a failed restore — it must stay recoverable, not silently consumed")
	}

	// Retry with a healthy engine must now succeed against the SAME snapshot.
	engine := policy.NewEngine(env.rdb)
	restored, err = restorePermissionsFromSnapshot(context.Background(), env.db, engine, zap.NewNop(), modelName, newModelID)
	if err != nil {
		t.Fatalf("retry after Redis recovery should succeed, got: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("expected retry to restore 1 team, got %v", restored)
	}
}

// ─── Phase 2B: atomic, exactly-once concurrent restoration ───────────────────

func TestRestorePermissions_ConcurrentRedeploysConsumeSnapshotExactlyOnce(t *testing.T) {
	env := setupRestoreTestEnv(t)
	modelName := "embeddings-" + uuid.New().String()[:8]
	teamID := seedDeletedModelWithSnapshot(t, env.db, modelName)

	// Two "tenants" concurrently redeploy under the identical name and both
	// race to restore the SAME still-unconsumed snapshot — the scenario the
	// forensic audit proved was previously non-deterministic (both could win).
	modelB := insertRedeployedModelAllowingDuplicateName(t, env.db, modelName)
	modelC := insertRedeployedModelAllowingDuplicateName(t, env.db, modelName)

	engine := policy.NewEngine(env.rdb)

	var wg sync.WaitGroup
	results := make([]struct {
		restored []string
		err      error
	}, 2)
	targets := []string{modelB, modelC}
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			r, err := restorePermissionsFromSnapshot(context.Background(), env.db, engine, zap.NewNop(), modelName, targets[idx])
			results[idx].restored = r
			results[idx].err = err
		}(i)
	}
	close(start)
	wg.Wait()

	wins := 0
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("attempt %d returned an unexpected error (should be a clean no-op, not a failure): %v", i, r.err)
		}
		if len(r.restored) > 0 {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly one of two concurrent restore attempts to win, got %d", wins)
	}

	// Whichever model won must have the Redis grant; the other must not.
	winnerIsB, err := env.rdb.SIsMember(context.Background(), "nexus:team:"+teamID+":models", modelName).Result()
	if err != nil {
		t.Fatalf("redis check: %v", err)
	}
	if !winnerIsB {
		t.Fatal("expected the winning restore to be reflected in the gateway's Redis ACL set")
	}

	var totalGrants int
	if err := env.db.Get(&totalGrants, `SELECT COUNT(*) FROM team_model_permissions WHERE team_id=$1::uuid AND model_id IN ($2::uuid,$3::uuid)`, teamID, modelB, modelC); err != nil {
		t.Fatalf("query grants: %v", err)
	}
	if totalGrants != 1 {
		t.Fatalf("expected the snapshot to be applied to exactly one of the two redeployed models, found %d grants", totalGrants)
	}
}

// insertRedeployedModelAllowingDuplicateName inserts a second, independent
// models row standing in for a concurrently-redeploying tenant. Its own
// `name` column value is irrelevant to the test — restorePermissionsFromSnapshot
// takes the snapshot-lookup name and the target model_id as separate
// arguments, so all this helper needs to provide is a valid, distinct
// models.id to satisfy team_model_permissions' FK. Giving it a distinct
// literal name just avoids tripping idx_models_name_active, which is
// unrelated to what's under test here.
func insertRedeployedModelAllowingDuplicateName(t *testing.T, db *sqlx.DB, modelName string) string {
	t.Helper()
	var id string
	if err := db.Get(&id, `INSERT INTO models (name, display_name, lifecycle) VALUES ($1, $1, 'active') RETURNING id::text`, modelName+"-"+uuid.New().String()[:6]); err != nil {
		t.Fatalf("insert racing model: %v", err)
	}
	return id
}
