package runtimemgr

// Regression test for a production incident (gpt-oss-120b, forensic audit
// Case File 003 round 6-follow-up): the HA reconciler's rolling-replacement
// path (internal/ha/reconciler.go stepUnhealthyReplicas) now excludes
// lazy_load models — it does unconstrained free-placement across nodes,
// which picked the wrong node for a model whose GGUF file only existed on
// one specific node, and failed instantly. That means nothing else will
// ever retry a lazy_load model stuck in 'unhealthy' unless doStartModel
// itself re-enqueues directly. This proves it does.

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestDoStartModel_LazyLoadUnhealthy_ReenqueuesDirectly(t *testing.T) {
	db := setupBindHostTestDB(t)

	var nodeID, modelID string
	if err := db.Get(&nodeID, `INSERT INTO nodes (hostname, ip_address, status) VALUES ('node-gptoss', '192.168.0.108', 'online') RETURNING id::text`); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	if err := db.Get(&modelID, `INSERT INTO models (name) VALUES ('gpt-oss-120b-repro') RETURNING id::text`); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO model_runtime_configs (model_id, workload_policy) VALUES ($1::uuid, 'lazy_load')`, modelID); err != nil {
		t.Fatalf("seed runtime config: %v", err)
	}
	var endpointID string
	if err := db.Get(&endpointID, `
		INSERT INTO model_endpoints (model_id, node_id, host, port, lifecycle_state)
		VALUES ($1::uuid, $2::uuid, '192.168.0.108', 41635, 'active') RETURNING id::text`,
		modelID, nodeID); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
	var unhealthyID string
	if err := db.Get(&unhealthyID, `
		INSERT INTO agent_runtimes (node_id, endpoint_id, model_id, runtime_name, state, bind_host, bind_port, workload_policy)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'nexus-gpt-oss-120b-repro', 'unhealthy', '192.168.0.108', 41635, 'lazy_load')
		RETURNING id::text`,
		nodeID, endpointID, modelID); err != nil {
		t.Fatalf("seed unhealthy runtime: %v", err)
	}

	a := &RuntimeActivator{
		db:  db,
		log: zap.NewNop(),
		cfg: Config{ColdStartTimeout: 5 * time.Second},
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Logf("doStartModel panicked (expected: nil taskMgr, panics AFTER the DB commit inside enqueueStartModel): %v", r)
			}
		}()
		_, _ = a.doStartModel(context.Background(), "gpt-oss-120b-repro", time.Now())
	}()

	var newCount int
	if err := db.Get(&newCount, `
		SELECT COUNT(*) FROM agent_runtimes
		WHERE model_id = $1::uuid AND id != $2::uuid AND state = 'pending'`,
		modelID, unhealthyID); err != nil {
		t.Fatalf("count new runtime rows: %v", err)
	}
	if newCount != 1 {
		t.Fatalf("expected doStartModel to re-enqueue directly for an unhealthy lazy_load model (1 new pending row), got %d — nothing else will ever retry a lazy_load model since the reconciler now correctly ignores it", newCount)
	}
}
