package handlers

// Regression tests proving Start/Restart/Upgrade/Rollback cannot recreate a
// runtime for a model an admin has disabled or soft-deleted, and that
// ensureRuntimeRow now respects replica capacity under concurrency
// (forensic audit, Case File 003, round 6, items 2 and 4).

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"

	"github.com/nexusllm/nexusllm/internal/taskmanager"
)

// gin.SetMode writes a package-level global; calling it from concurrent
// goroutines (as TestEnsureRuntimeRow_ConcurrentStart_RespectsCapacity does)
// is a benign-value-but-still-flagged data race under -race. Set it once.
var ginModeOnce sync.Once

func setGinTestMode() {
	ginModeOnce.Do(func() { gin.SetMode(gin.TestMode) })
}

func newLifecycleTestHandler(db *sqlx.DB) *ControllerHandler {
	taskMgr := taskmanager.NewManager(db, zap.NewNop())
	return NewControllerHandler(db, taskMgr, zap.NewNop())
}

// seedLifecycleEndpoint creates a node, a model with the given enabled/
// lifecycle values, and an endpoint+existing runtime for it, returning the
// endpoint ID.
func seedLifecycleEndpoint(t *testing.T, db *sqlx.DB, enabled bool, lifecycle string) (endpointID string) {
	t.Helper()
	var nodeID, modelID string
	if err := db.Get(&nodeID, `INSERT INTO nodes (hostname, ip_address) VALUES ($1, '10.9.0.5') RETURNING id::text`,
		"node-"+uuid.New().String()[:8]); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	if err := db.Get(&modelID, `INSERT INTO models (name, backend_type, enabled, lifecycle) VALUES ($1,'llamacpp',$2,$3) RETURNING id::text`,
		"model-"+uuid.New().String()[:8], enabled, lifecycle); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if err := db.Get(&endpointID, `
		INSERT INTO model_endpoints (model_id, node_id, host, port, runtime_image)
		VALUES ($1,$2,'10.9.0.5',8100,'ghcr.io/ggml-org/llama.cpp:server') RETURNING id::text`,
		modelID, nodeID); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO agent_runtimes (node_id, endpoint_id, model_id, runtime_name, backend, bind_host, bind_port, state)
		VALUES ($1,$2,$3,'nexus-existing','llamacpp','10.9.0.5',8100,'failed')`,
		nodeID, endpointID, modelID); err != nil {
		t.Fatalf("seed existing runtime: %v", err)
	}
	return endpointID
}

func lifecycleTestContext(method, path string, body interface{}) *gin.Context {
	c, _ := lifecycleTestContextWithRecorder(method, path, body)
	return c
}

func lifecycleTestContextWithRecorder(method, path string, body interface{}) (*gin.Context, *httptest.ResponseRecorder) {
	setGinTestMode()
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

func setQueryEndpointID(c *gin.Context, endpointID string) {
	q := c.Request.URL.Query()
	q.Set("endpoint_id", endpointID)
	c.Request.URL.RawQuery = q.Encode()
}

// TestStartModel_RejectsDeletedModel is Test B: a soft-deleted model
// (enabled=false, lifecycle=deleted) must not be startable, must not create
// a new agent_runtimes row, and must not dispatch a START_MODEL task.
func TestStartModel_RejectsDeletedModel(t *testing.T) {
	db := setupControllerTestDB(t)
	h := newLifecycleTestHandler(db)
	endpointID := seedLifecycleEndpoint(t, db, false, "deleted")

	var before int
	if err := db.Get(&before, `SELECT COUNT(*) FROM agent_runtimes`); err != nil {
		t.Fatalf("count before: %v", err)
	}

	c := lifecycleTestContext("POST", "/", nil)
	setQueryEndpointID(c, endpointID)
	h.StartModel(c)

	if c.Writer.Status() != 409 {
		t.Fatalf("expected 409 Conflict, got %d", c.Writer.Status())
	}
	var after int
	if err := db.Get(&after, `SELECT COUNT(*) FROM agent_runtimes`); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != before {
		t.Fatalf("expected no new agent_runtimes row, before=%d after=%d", before, after)
	}
	var taskCount int
	if err := db.Get(&taskCount, `SELECT COUNT(*) FROM agent_tasks WHERE task_type='START_MODEL'`); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if taskCount != 0 {
		t.Fatalf("expected no START_MODEL task dispatched, got %d", taskCount)
	}
}

// TestRestartModel_RejectsDisabledModel is Test C, using the
// enabled=false/lifecycle=active combination (a plain Disable, not a full
// soft-delete) to prove the guard checks both fields independently.
func TestRestartModel_RejectsDisabledModel(t *testing.T) {
	db := setupControllerTestDB(t)
	h := newLifecycleTestHandler(db)
	endpointID := seedLifecycleEndpoint(t, db, false, "active")

	var before int
	if err := db.Get(&before, `SELECT COUNT(*) FROM agent_runtimes`); err != nil {
		t.Fatalf("count before: %v", err)
	}

	c := lifecycleTestContext("POST", "/", nil)
	setQueryEndpointID(c, endpointID)
	h.RestartModel(c)

	if c.Writer.Status() != 409 {
		t.Fatalf("expected 409 Conflict, got %d", c.Writer.Status())
	}
	var after int
	if err := db.Get(&after, `SELECT COUNT(*) FROM agent_runtimes`); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != before {
		t.Fatalf("expected no new agent_runtimes row, before=%d after=%d", before, after)
	}
}

// TestUpgradeAndRollback_RejectDeletedModel is Test D.
func TestUpgradeAndRollback_RejectDeletedModel(t *testing.T) {
	db := setupControllerTestDB(t)
	h := newLifecycleTestHandler(db)

	t.Run("upgrade", func(t *testing.T) {
		endpointID := seedLifecycleEndpoint(t, db, true, "deleted")
		var before int
		_ = db.Get(&before, `SELECT COUNT(*) FROM agent_runtimes`)

		c := lifecycleTestContext("POST", "/", map[string]string{"image": "vllm/vllm-openai:v2"})
		setQueryEndpointID(c, endpointID)
		h.UpgradeModel(c)

		if c.Writer.Status() != 409 {
			t.Fatalf("upgrade: expected 409, got %d", c.Writer.Status())
		}
		var after int
		_ = db.Get(&after, `SELECT COUNT(*) FROM agent_runtimes`)
		if after != before {
			t.Fatalf("upgrade: expected no new agent_runtimes row, before=%d after=%d", before, after)
		}
	})

	t.Run("rollback", func(t *testing.T) {
		endpointID := seedLifecycleEndpoint(t, db, true, "deleted")
		var before int
		_ = db.Get(&before, `SELECT COUNT(*) FROM agent_runtimes`)

		c := lifecycleTestContext("POST", "/", map[string]string{"previous_image": "vllm/vllm-openai:v1"})
		setQueryEndpointID(c, endpointID)
		h.RollbackModel(c)

		if c.Writer.Status() != 409 {
			t.Fatalf("rollback: expected 409, got %d", c.Writer.Status())
		}
		var after int
		_ = db.Get(&after, `SELECT COUNT(*) FROM agent_runtimes`)
		if after != before {
			t.Fatalf("rollback: expected no new agent_runtimes row, before=%d after=%d", before, after)
		}
	})
}

// TestStartModel_AllowsEnabledActiveModel is the explicit non-regression
// check: a normal, healthy model must still be startable through the same
// guard.
func TestStartModel_AllowsEnabledActiveModel(t *testing.T) {
	db := setupControllerTestDB(t)
	h := newLifecycleTestHandler(db)
	endpointID := seedLifecycleEndpoint(t, db, true, "active")

	c, w := lifecycleTestContextWithRecorder("POST", "/", nil)
	setQueryEndpointID(c, endpointID)
	h.StartModel(c)

	if c.Writer.Status() != 202 {
		t.Fatalf("expected 202 Accepted for an enabled/active model, got %d: %s", c.Writer.Status(), w.Body.String())
	}
	var runtimeCount int
	if err := db.Get(&runtimeCount, `SELECT COUNT(*) FROM agent_runtimes WHERE endpoint_id=$1 AND state='pending'`, endpointID); err != nil {
		t.Fatalf("count pending runtimes: %v", err)
	}
	if runtimeCount != 1 {
		t.Fatalf("expected exactly one new pending runtime row, got %d", runtimeCount)
	}
}

// TestEnsureRuntimeRow_ConcurrentStart_RespectsCapacity is Test F: N
// concurrent Start calls against the same endpoint must not create more
// than desired_replicas+max_surge non-terminal agent_runtimes rows.
func TestEnsureRuntimeRow_ConcurrentStart_RespectsCapacity(t *testing.T) {
	db := setupControllerTestDB(t)
	h := newLifecycleTestHandler(db)
	endpointID := seedLifecycleEndpoint(t, db, true, "active")

	var modelID string
	if err := db.Get(&modelID, `SELECT model_id::text FROM model_endpoints WHERE id=$1`, endpointID); err != nil {
		t.Fatalf("query model_id: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO model_replica_specs (model_id, desired_replicas, max_surge) VALUES ($1,1,1)`, modelID); err != nil {
		t.Fatalf("seed replica spec: %v", err)
	}
	// Clear the pre-seeded 'failed' runtime so capacity starts at zero —
	// isolates this test to purely the concurrent-claim behavior.
	if _, err := db.Exec(`DELETE FROM agent_runtimes WHERE endpoint_id=$1`, endpointID); err != nil {
		t.Fatalf("clear seeded runtime: %v", err)
	}

	const concurrency = 8
	var wg sync.WaitGroup
	var accepted int32
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := lifecycleTestContext("POST", "/", nil)
			setQueryEndpointID(c, endpointID)
			h.StartModel(c)
			if c.Writer.Status() == 202 {
				atomic.AddInt32(&accepted, 1)
			}
		}()
	}
	wg.Wait()

	var nonTerminal int
	if err := db.Get(&nonTerminal, `
		SELECT COUNT(*) FROM agent_runtimes
		WHERE endpoint_id = $1
		  AND state NOT IN ('stopped','deleted','archived','unloaded','lost','draining','failed','unhealthy')`,
		endpointID); err != nil {
		t.Fatalf("count non-terminal: %v", err)
	}
	// desired_replicas=1 + max_surge=1 = 2 concurrent non-terminal rows max.
	if nonTerminal > 2 {
		t.Fatalf("expected at most 2 non-terminal rows (desired+max_surge) despite %d concurrent Start calls, got %d", concurrency, nonTerminal)
	}
	if nonTerminal < 1 {
		t.Fatalf("expected at least 1 row to have won the race, got %d", nonTerminal)
	}
	t.Logf("concurrency=%d accepted=%d non_terminal_rows=%d", concurrency, accepted, nonTerminal)
}
