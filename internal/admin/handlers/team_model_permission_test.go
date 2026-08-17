package handlers

// Regression test for Critical Finding #1 (production security re-audit):
// TeamHandler.RemoveModelPermission used a bare `SELECT id FROM models WHERE
// name=$1` with no ordering or lifecycle filter to resolve the model to
// revoke. Once a model has been through a delete+redeploy cycle, more than
// one row can share that name (idx_models_name_active only enforces
// uniqueness among non-deleted rows), so the lookup was non-deterministic
// and could silently resolve to the OLD, deleted model_id — the DELETE would
// then affect zero rows (the real, active grant is keyed to a different
// model_id), while the endpoint still reported 200 "removed". This test
// reproduces the exact lifecycle and proves the fix resolves to the CURRENT
// active model and actually removes its grant.

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nexusllm/nexusllm/internal/policy"
)

func TestRemoveModelPermission_DeleteRedeployThenRevoke(t *testing.T) {
	env := setupRestoreTestEnv(t)
	ctx := context.Background()

	modelName := "revoke-lifecycle-" + uuid.New().String()[:8]
	// Creates model v1, grants teamID, then soft-deletes v1 (firing the real
	// snapshot trigger) — exactly the delete half of the lifecycle.
	teamID := seedDeletedModelWithSnapshot(t, env.db, modelName)
	// Redeploy: model v2, same name, active. Only succeeds once v1 is
	// lifecycle='deleted', matching production's partial unique index.
	newModelID := insertRedeployedModel(t, env.db, modelName)

	// Re-grant on the NEW (active) model_id — mirroring what
	// restorePermissionsFromSnapshot or a fresh AddModelPermission call would
	// have created after the redeploy.
	if _, err := env.db.Exec(`INSERT INTO team_model_permissions (team_id, model_id) VALUES ($1::uuid, $2::uuid)`,
		teamID, newModelID); err != nil {
		t.Fatalf("seed active grant: %v", err)
	}

	engine := policy.NewEngine(env.rdb)
	if err := engine.SetModelAllowed(ctx, teamID, modelName); err != nil {
		t.Fatalf("seed redis grant: %v", err)
	}

	h := NewTeamHandler(env.db, env.rdb, engine)
	c, w := projectModelsTestContext("DELETE", "/", nil)
	c.Params = gin.Params{{Key: "id", Value: teamID}, {Key: "model", Value: modelName}}
	h.RemoveModelPermission(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// The ACTIVE model's grant (model_id = newModelID) must actually be
	// gone. Under the pre-fix ambiguous lookup, this could remain untouched
	// if the query happened to resolve to the stale, deleted model_id
	// instead.
	var count int
	if err := env.db.Get(&count,
		`SELECT COUNT(*) FROM team_model_permissions WHERE team_id=$1::uuid AND model_id=$2::uuid`,
		teamID, newModelID); err != nil {
		t.Fatalf("query team_model_permissions: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected the active model's grant to be removed, found %d row(s) — revoke resolved to the wrong model_id", count)
	}

	member, err := env.rdb.SIsMember(ctx, "nexus:team:"+teamID+":models", modelName).Result()
	if err != nil {
		t.Fatalf("redis SIsMember: %v", err)
	}
	if member {
		t.Fatal("expected the Redis ACL set to no longer contain the revoked model")
	}
}
