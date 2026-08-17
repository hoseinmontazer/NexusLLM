package policy

// Regression tests for project-level model authorization (Option A —
// effective_access = team_model_access AND project_model_access), added
// because a forensic audit confirmed req.ProjectID previously had zero
// effect on Public/managed-model authorization: a project-scoped token got
// exactly the same access as a plain team token for the same team.
//
// Runs against a disposable redis:7-alpine container.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func setupTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available — skipping policy engine integration tests")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable — skipping policy engine integration tests")
	}

	suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:8]
	name := "nexus-test-redis-" + suffix
	port := 16300 + int(time.Now().UnixNano()%2000)

	run := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-p", fmt.Sprintf("%d:6379", port), "redis:7-alpine")
	if out, err := run.CombinedOutput(); err != nil {
		t.Skipf("could not start disposable redis container (%v): %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	rdb := redis.NewClient(&redis.Options{Addr: fmt.Sprintf("127.0.0.1:%d", port)})
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if lastErr = rdb.Ping(context.Background()).Err(); lastErr == nil {
			return rdb
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("redis never became ready: %v", lastErr)
	return nil
}

func newTestIDs() (team, project, otherTeam, otherProject string) {
	return uuid.New().String(), uuid.New().String(), uuid.New().String(), uuid.New().String()
}

// TestEvaluate_LegacyTeamToken_NoProjectID proves Scenario 6: a token with no
// ProjectID must keep exactly its existing team-only behavior.
func TestEvaluate_LegacyTeamToken_NoProjectID(t *testing.T) {
	rdb := setupTestRedis(t)
	e := NewEngine(rdb)
	ctx := context.Background()
	team, _, _, _ := newTestIDs()

	if err := e.SetModelAllowed(ctx, team, "model-a"); err != nil {
		t.Fatalf("SetModelAllowed: %v", err)
	}

	d := e.Evaluate(ctx, &InferenceRequest{Model: "model-a", TeamID: team, ProjectID: ""}, 500, nil)
	if !d.Allowed {
		t.Fatalf("expected legacy team-only token to be allowed for a team-granted model, got denied: %s", d.RejectReason)
	}

	d = e.Evaluate(ctx, &InferenceRequest{Model: "model-b", TeamID: team, ProjectID: ""}, 500, nil)
	if d.Allowed {
		t.Fatal("expected legacy team-only token to be denied for a model the team never granted")
	}
}

// TestEvaluate_UnconfiguredProject_PassthroughToTeam is the critical backward-
// compatibility test: a project that has NEVER had an explicit model grant
// must inherit its team's full access unchanged — every existing project-
// scoped token in production has zero project_model_permissions rows today
// and must keep working exactly as before this feature shipped.
func TestEvaluate_UnconfiguredProject_PassthroughToTeam(t *testing.T) {
	rdb := setupTestRedis(t)
	e := NewEngine(rdb)
	ctx := context.Background()
	team, project, _, _ := newTestIDs()

	if err := e.SetModelAllowed(ctx, team, "model-a"); err != nil {
		t.Fatalf("SetModelAllowed: %v", err)
	}
	// Note: project is NEVER touched — no grant, no revoke, ever.

	d := e.Evaluate(ctx, &InferenceRequest{Model: "model-a", TeamID: team, ProjectID: project}, 500, nil)
	if !d.Allowed {
		t.Fatalf("expected an unconfigured project to pass through to its team's access, got denied: %s", d.RejectReason)
	}
}

// TestEvaluate_ProjectAuthorization is the core Option-A proof: once a
// project has at least one explicit grant, it moves into restricted mode —
// team AND project semantics apply.
func TestEvaluate_ProjectAuthorization(t *testing.T) {
	rdb := setupTestRedis(t)
	e := NewEngine(rdb)
	ctx := context.Background()
	team, project, _, _ := newTestIDs()

	for _, m := range []string{"model-a", "model-b", "model-c"} {
		if err := e.SetModelAllowed(ctx, team, m); err != nil {
			t.Fatalf("SetModelAllowed(%s): %v", m, err)
		}
	}
	// Project only ever granted A and B — never C.
	if err := e.SetProjectModelAllowed(ctx, project, "model-a"); err != nil {
		t.Fatalf("SetProjectModelAllowed: %v", err)
	}
	if err := e.SetProjectModelAllowed(ctx, project, "model-b"); err != nil {
		t.Fatalf("SetProjectModelAllowed: %v", err)
	}

	cases := []struct {
		model string
		want  bool
	}{
		{"model-a", true},
		{"model-b", true},
		{"model-c", false}, // team allows it, project never granted it — Scenario "Project restriction"
	}
	for _, tc := range cases {
		d := e.Evaluate(ctx, &InferenceRequest{Model: tc.model, TeamID: team, ProjectID: project}, 500, nil)
		if d.Allowed != tc.want {
			t.Fatalf("model=%s: expected allowed=%v, got allowed=%v (reason=%s)", tc.model, tc.want, d.Allowed, d.RejectReason)
		}
	}
}

// TestEvaluate_TeamUpperBound proves Scenario "Team upper bound": a project
// grant can never widen access beyond what the team allows.
func TestEvaluate_TeamUpperBound(t *testing.T) {
	rdb := setupTestRedis(t)
	e := NewEngine(rdb)
	ctx := context.Background()
	team, project, _, _ := newTestIDs()

	// Team never grants model-c at all.
	if err := e.SetModelAllowed(ctx, team, "model-a"); err != nil {
		t.Fatalf("SetModelAllowed: %v", err)
	}
	// Project grants model-c anyway (misconfiguration or team later revoked it).
	if err := e.SetProjectModelAllowed(ctx, project, "model-c"); err != nil {
		t.Fatalf("SetProjectModelAllowed: %v", err)
	}

	d := e.Evaluate(ctx, &InferenceRequest{Model: "model-c", TeamID: team, ProjectID: project}, 500, nil)
	if d.Allowed {
		t.Fatal("expected team to remain an upper bound — a project grant must not override a missing/revoked team grant")
	}
}

// TestEvaluate_TeamRevocation proves Scenario 3: team revokes a model the
// project still (independently) grants — must be denied.
func TestEvaluate_TeamRevocation(t *testing.T) {
	rdb := setupTestRedis(t)
	e := NewEngine(rdb)
	ctx := context.Background()
	team, project, _, _ := newTestIDs()

	if err := e.SetModelAllowed(ctx, team, "model-b"); err != nil {
		t.Fatalf("SetModelAllowed: %v", err)
	}
	if err := e.SetProjectModelAllowed(ctx, project, "model-b"); err != nil {
		t.Fatalf("SetProjectModelAllowed: %v", err)
	}
	if d := e.Evaluate(ctx, &InferenceRequest{Model: "model-b", TeamID: team, ProjectID: project}, 500, nil); !d.Allowed {
		t.Fatalf("expected allowed before team revocation, got denied: %s", d.RejectReason)
	}

	if err := e.RemoveModelAllowed(ctx, team, "model-b"); err != nil {
		t.Fatalf("RemoveModelAllowed: %v", err)
	}
	d := e.Evaluate(ctx, &InferenceRequest{Model: "model-b", TeamID: team, ProjectID: project}, 500, nil)
	if d.Allowed {
		t.Fatal("expected immediate denial once the team revokes a model, even though the project still grants it")
	}
}

// TestEvaluate_ProjectRevocation proves Scenario 4: project revokes a model
// the team still allows — must be denied immediately, no cache window.
func TestEvaluate_ProjectRevocation(t *testing.T) {
	rdb := setupTestRedis(t)
	e := NewEngine(rdb)
	ctx := context.Background()
	team, project, _, _ := newTestIDs()

	if err := e.SetModelAllowed(ctx, team, "model-a"); err != nil {
		t.Fatalf("SetModelAllowed: %v", err)
	}
	if err := e.SetProjectModelAllowed(ctx, project, "model-a"); err != nil {
		t.Fatalf("SetProjectModelAllowed: %v", err)
	}
	if d := e.Evaluate(ctx, &InferenceRequest{Model: "model-a", TeamID: team, ProjectID: project}, 500, nil); !d.Allowed {
		t.Fatalf("expected allowed before project revocation, got denied: %s", d.RejectReason)
	}

	if err := e.RemoveProjectModelAllowed(ctx, project, "model-a"); err != nil {
		t.Fatalf("RemoveProjectModelAllowed: %v", err)
	}
	d := e.Evaluate(ctx, &InferenceRequest{Model: "model-a", TeamID: team, ProjectID: project}, 500, nil)
	if d.Allowed {
		t.Fatal("expected immediate denial once the project revokes a model, even though the team still allows it")
	}

	// Revoking the LAST model must leave the project in deny-all mode, not
	// revert to legacy team-passthrough (that would silently re-grant every
	// team model the instant a project's grant list becomes empty).
	d2 := e.Evaluate(ctx, &InferenceRequest{Model: "model-a", TeamID: team, ProjectID: project}, 500, nil)
	if d2.Allowed {
		t.Fatal("expected a project with zero remaining grants to stay in deny-all mode, not fall back to team passthrough")
	}
}

// TestEvaluate_ProjectIsolation proves token isolation: Project A's grants
// must never leak into Project B's evaluation, even under the same team.
func TestEvaluate_ProjectIsolation(t *testing.T) {
	rdb := setupTestRedis(t)
	e := NewEngine(rdb)
	ctx := context.Background()
	team, projectA, _, projectB := newTestIDs()

	if err := e.SetModelAllowed(ctx, team, "model-a"); err != nil {
		t.Fatalf("SetModelAllowed: %v", err)
	}
	if err := e.SetProjectModelAllowed(ctx, projectA, "model-a"); err != nil {
		t.Fatalf("SetProjectModelAllowed: %v", err)
	}
	// projectB is explicitly configured but never granted model-a.
	if err := e.SetProjectModelAllowed(ctx, projectB, "model-x"); err != nil {
		t.Fatalf("SetProjectModelAllowed: %v", err)
	}

	if d := e.Evaluate(ctx, &InferenceRequest{Model: "model-a", TeamID: team, ProjectID: projectA}, 500, nil); !d.Allowed {
		t.Fatalf("expected project A token to access model-a, got denied: %s", d.RejectReason)
	}
	if d := e.Evaluate(ctx, &InferenceRequest{Model: "model-a", TeamID: team, ProjectID: projectB}, 500, nil); d.Allowed {
		t.Fatal("expected project B token to be denied model-a — project A's grant must not leak into project B")
	}
}

// TestEvaluate_RedisFailure_DoesNotGrantAccess proves requirement #8: if the
// Redis connection is broken, the model ACL check must fail closed (deny),
// never fail open.
func TestEvaluate_RedisFailure_DoesNotGrantAccess(t *testing.T) {
	// A client pointed at nothing — every call errors.
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	e := NewEngine(rdb)
	ctx := context.Background()
	team, project, _, _ := newTestIDs()

	d := e.Evaluate(ctx, &InferenceRequest{Model: "model-a", TeamID: team, ProjectID: project}, 500, nil)
	if d.Allowed {
		t.Fatal("expected a Redis connection failure to fail closed (deny), not grant access")
	}
	if d.RejectReason != "model_not_allowed" {
		t.Fatalf("expected model_not_allowed on Redis failure, got %q", d.RejectReason)
	}
}
