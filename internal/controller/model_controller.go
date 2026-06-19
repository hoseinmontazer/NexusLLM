package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// ─────────────────────────────────────────────────────────────────────────────
// ModelController orchestrates start / stop / restart / upgrade / rollback
// operations for model runtimes. It persists all operations to PostgreSQL
// and publishes lifecycle state transitions to Redis pub/sub so the gateway
// registry can react immediately.
// ─────────────────────────────────────────────────────────────────────────────

const (
	lifecycleChan = "nexus:lifecycle:events" // Redis pub/sub channel
	opTimeout     = 10 * time.Minute
)

// EndpointRecord is a lightweight DB projection used by the controller.
type EndpointRecord struct {
	ID              string  `db:"id"`
	ModelID         string  `db:"model_id"`
	ModelName       string  `db:"model_name"`
	BackendType     string  `db:"backend_type"`
	Host            string  `db:"host"`
	Port            int     `db:"port"`
	ContainerID     string  `db:"container_id"`
	LifecycleState  string  `db:"lifecycle_state"`
	TensorParallel  int     `db:"tensor_parallel"`
	GPUMemoryUtil   float64 `db:"gpu_memory_util"`
	MaxModelLen     int     `db:"max_model_len"`
	Dtype           string  `db:"dtype"`
	Quantization    string  `db:"quantization"`
	RuntimeImage    string  `db:"runtime_image"`
}

// ModelController manages runtime lifecycle operations.
type ModelController struct {
	db     *sqlx.DB
	rdb    *redis.Client
	driver Driver
	log    *zap.Logger
}

// NewModelController constructs a ModelController.
func NewModelController(db *sqlx.DB, rdb *redis.Client, driver Driver, log *zap.Logger) *ModelController {
	return &ModelController{db: db, rdb: rdb, driver: driver, log: log}
}

// ─── Public operations ────────────────────────────────────────────────────────

// Start launches the runtime for the given endpoint.
func (c *ModelController) Start(ctx context.Context, endpointID, actor string) error {
	ep, err := c.loadEndpoint(ctx, endpointID)
	if err != nil {
		return err
	}
	opID := c.beginOp(ctx, ep.ModelID, endpointID, "start", actor)

	spec := c.buildSpec(ep)
	containerID, err := c.driver.Start(ctx, spec)
	if err != nil {
		c.failOp(ctx, opID, err)
		c.transition(ctx, endpointID, ep.LifecycleState, "failed", "start failed: "+err.Error(), actor)
		return fmt.Errorf("start endpoint %s: %w", endpointID, err)
	}

	// Persist container ID and transition state
	_, _ = c.db.ExecContext(ctx,
		`UPDATE model_endpoints SET container_id = $1, lifecycle_state = 'loading', updated_at = NOW() WHERE id = $2`,
		containerID, endpointID)
	c.transition(ctx, endpointID, ep.LifecycleState, "loading", "container started", actor)
	c.succeedOp(ctx, opID, map[string]string{"container_id": containerID})

	c.log.Info("endpoint started",
		zap.String("endpoint_id", endpointID),
		zap.String("container_id", containerID),
		zap.String("model", ep.ModelName),
	)
	return nil
}

// Stop gracefully stops the runtime.
func (c *ModelController) Stop(ctx context.Context, endpointID, actor string) error {
	ep, err := c.loadEndpoint(ctx, endpointID)
	if err != nil {
		return err
	}
	opID := c.beginOp(ctx, ep.ModelID, endpointID, "stop", actor)

	if err := c.driver.Stop(ctx, ep.ContainerID, 30*time.Second); err != nil {
		c.failOp(ctx, opID, err)
		return fmt.Errorf("stop endpoint %s: %w", endpointID, err)
	}

	_, _ = c.db.ExecContext(ctx,
		`UPDATE model_endpoints SET lifecycle_state = 'unloaded', health_status = 'down', updated_at = NOW() WHERE id = $1`,
		endpointID)
	c.transition(ctx, endpointID, ep.LifecycleState, "unloaded", "stopped by "+actor, actor)
	c.succeedOp(ctx, opID, nil)
	return nil
}

// Restart stops then starts the runtime.
func (c *ModelController) Restart(ctx context.Context, endpointID, actor string) error {
	ep, err := c.loadEndpoint(ctx, endpointID)
	if err != nil {
		return err
	}
	opID := c.beginOp(ctx, ep.ModelID, endpointID, "restart", actor)

	spec := c.buildSpec(ep)
	newID, err := c.driver.Restart(ctx, ep.ContainerID, spec, 30*time.Second)
	if err != nil {
		c.failOp(ctx, opID, err)
		c.transition(ctx, endpointID, ep.LifecycleState, "failed", "restart failed: "+err.Error(), actor)
		return fmt.Errorf("restart endpoint %s: %w", endpointID, err)
	}

	_, _ = c.db.ExecContext(ctx,
		`UPDATE model_endpoints SET container_id = $1, lifecycle_state = 'loading',
		 health_status = 'unknown', updated_at = NOW() WHERE id = $2`,
		newID, endpointID)
	c.transition(ctx, endpointID, ep.LifecycleState, "loading", "restarted by "+actor, actor)
	c.succeedOp(ctx, opID, map[string]string{"container_id": newID})
	return nil
}

// Drain marks the endpoint as draining — health watcher stops routing new traffic.
func (c *ModelController) Drain(ctx context.Context, endpointID, actor string) error {
	ep, err := c.loadEndpoint(ctx, endpointID)
	if err != nil {
		return err
	}
	opID := c.beginOp(ctx, ep.ModelID, endpointID, "drain", actor)
	_, _ = c.db.ExecContext(ctx,
		`UPDATE model_endpoints SET lifecycle_state = 'draining', health_status = 'draining', updated_at = NOW() WHERE id = $1`,
		endpointID)
	c.transition(ctx, endpointID, ep.LifecycleState, "draining", "drained by "+actor, actor)
	c.succeedOp(ctx, opID, nil)
	c.publishEvent(ctx, endpointID, "draining")
	return nil
}

// Upgrade performs a rolling upgrade: start new container, drain old, stop old.
func (c *ModelController) Upgrade(ctx context.Context, endpointID, newImage, actor string) error {
	ep, err := c.loadEndpoint(ctx, endpointID)
	if err != nil {
		return err
	}
	opID := c.beginOp(ctx, ep.ModelID, endpointID, "upgrade", actor)

	// Build spec with new image
	spec := c.buildSpec(ep)
	spec.Image = newImage
	spec.BindPort = ep.Port + 10000 // temp port for canary

	// Start new instance
	newContainerID, err := c.driver.Start(ctx, spec)
	if err != nil {
		c.failOp(ctx, opID, err)
		return fmt.Errorf("upgrade start new: %w", err)
	}

	// Give it 60s to become healthy before switching
	time.Sleep(60 * time.Second)

	// Drain old, switch port, stop old
	oldContainerID := ep.ContainerID
	_, _ = c.db.ExecContext(ctx,
		`UPDATE model_endpoints SET container_id = $1, runtime_image = $2, lifecycle_state = 'loading',
		 health_status = 'unknown', updated_at = NOW() WHERE id = $3`,
		newContainerID, newImage, endpointID)
	c.transition(ctx, endpointID, ep.LifecycleState, "loading", "upgraded to "+newImage, actor)

	// Stop old container in background
	go func() {
		bgCtx := context.Background()
		_ = c.driver.Stop(bgCtx, oldContainerID, 60*time.Second)
		_ = c.driver.Remove(bgCtx, oldContainerID)
	}()

	c.succeedOp(ctx, opID, map[string]string{"new_container_id": newContainerID, "image": newImage})
	c.publishEvent(ctx, endpointID, "loading")
	return nil
}

// Rollback stops current container and restarts with previous image.
func (c *ModelController) Rollback(ctx context.Context, endpointID, previousImage, actor string) error {
	ep, err := c.loadEndpoint(ctx, endpointID)
	if err != nil {
		return err
	}
	opID := c.beginOp(ctx, ep.ModelID, endpointID, "rollback", actor)

	spec := c.buildSpec(ep)
	spec.Image = previousImage

	newID, err := c.driver.Restart(ctx, ep.ContainerID, spec, 30*time.Second)
	if err != nil {
		c.failOp(ctx, opID, err)
		c.transition(ctx, endpointID, ep.LifecycleState, "failed", "rollback failed", actor)
		return fmt.Errorf("rollback: %w", err)
	}

	_, _ = c.db.ExecContext(ctx,
		`UPDATE model_endpoints SET container_id = $1, runtime_image = $2, lifecycle_state = 'loading',
		 health_status = 'unknown', updated_at = NOW() WHERE id = $3`,
		newID, previousImage, endpointID)
	c.transition(ctx, endpointID, ep.LifecycleState, "loading", "rolled back to "+previousImage, actor)
	c.succeedOp(ctx, opID, map[string]string{"container_id": newID, "image": previousImage})
	c.publishEvent(ctx, endpointID, "loading")
	return nil
}

// GetLogs returns recent container logs.
func (c *ModelController) GetLogs(ctx context.Context, endpointID string, tail int) (string, error) {
	ep, err := c.loadEndpoint(ctx, endpointID)
	if err != nil {
		return "", err
	}
	return c.driver.Logs(ctx, ep.ContainerID, tail)
}

// ─── private helpers ──────────────────────────────────────────────────────────

func (c *ModelController) loadEndpoint(ctx context.Context, endpointID string) (EndpointRecord, error) {
	var ep EndpointRecord
	err := c.db.GetContext(ctx, &ep, `
		SELECT
			me.id, me.model_id, m.name AS model_name, m.backend_type,
			me.host, me.port,
			COALESCE(me.container_id, '')    AS container_id,
			COALESCE(me.lifecycle_state, 'registered') AS lifecycle_state,
			COALESCE(mrc.tensor_parallel, 1)   AS tensor_parallel,
			COALESCE(mrc.gpu_memory_util, 0.9) AS gpu_memory_util,
			COALESCE(mrc.max_batch_size, 256)  AS max_model_len,
			COALESCE(mrc.dtype, 'auto')        AS dtype,
			COALESCE(mrc.quantization, '')     AS quantization,
			COALESCE(me.runtime_image, 'vllm/vllm-openai:latest') AS runtime_image
		FROM model_endpoints me
		JOIN models m ON m.id = me.model_id
		LEFT JOIN model_runtime_configs mrc ON mrc.model_id = me.model_id
		WHERE me.id = $1`, endpointID)
	return ep, err
}

func (c *ModelController) buildSpec(ep EndpointRecord) RuntimeSpec {
	return RuntimeSpec{
		ModelName:      ep.ModelName,
		EndpointID:     ep.ID,
		Image:          ep.RuntimeImage,
		BindHost:       ep.Host,
		BindPort:       ep.Port,
		TensorParallel: ep.TensorParallel,
		GPUMemoryUtil:  ep.GPUMemoryUtil,
		MaxModelLen:    ep.MaxModelLen,
		Dtype:          ep.Dtype,
		Quantization:   ep.Quantization,
		Env:            map[string]string{},
	}
}

func (c *ModelController) transition(ctx context.Context, endpointID, from, to, reason, actor string) {
	_, _ = c.db.ExecContext(ctx,
		`INSERT INTO model_lifecycle_events (endpoint_id, from_state, to_state, reason, actor)
		 VALUES ($1,$2,$3,$4,$5)`,
		endpointID, from, to, reason, actor)
	c.publishEvent(ctx, endpointID, to)
}

func (c *ModelController) publishEvent(ctx context.Context, endpointID, state string) {
	_ = c.rdb.Publish(ctx, lifecycleChan,
		fmt.Sprintf(`{"endpoint_id":%q,"state":%q}`, endpointID, state)).Err()
}

func (c *ModelController) beginOp(ctx context.Context, modelID, endpointID, op, actor string) string {
	id := uuid.New().String()
	_, _ = c.db.ExecContext(ctx,
		`INSERT INTO controller_operations
		 (id, model_id, endpoint_id, operation, status, initiated_by, started_at)
		 VALUES ($1,$2,$3,$4,'running',$5,NOW())`,
		id, modelID, endpointID, op, actor)
	return id
}

func (c *ModelController) succeedOp(ctx context.Context, opID string, meta map[string]string) {
	_, _ = c.db.ExecContext(ctx,
		`UPDATE controller_operations SET status='success', completed_at=NOW() WHERE id=$1`, opID)
}

func (c *ModelController) failOp(ctx context.Context, opID string, err error) {
	_, _ = c.db.ExecContext(ctx,
		`UPDATE controller_operations SET status='failed', completed_at=NOW(), error_msg=$1 WHERE id=$2`,
		err.Error(), opID)
}
