package controller

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

const lifecycleChan = "nexus:lifecycle:events"

// EndpointRecord is a lightweight DB projection used by the controller.
type EndpointRecord struct {
	ID             string  `db:"id"`
	ModelID        string  `db:"model_id"`
	ModelName      string  `db:"model_name"`
	BackendType    string  `db:"backend_type"`
	Host           string  `db:"host"`
	Port           int     `db:"port"`
	ContainerID    string  `db:"container_id"`
	LifecycleState string  `db:"lifecycle_state"`
	TensorParallel int     `db:"tensor_parallel"`
	GPUMemoryUtil  float64 `db:"gpu_memory_util"`
	MaxModelLen    int     `db:"max_model_len"`
	Dtype          string  `db:"dtype"`
	Quantization   string  `db:"quantization"`
	RuntimeImage   string  `db:"runtime_image"`
	// Placement fields (populated from migration 005 columns)
	RuntimeType    string  `db:"runtime_type"`
	CPUThreads     int     `db:"cpu_threads"`
	NUMANode       int     `db:"numa_node"`
	MemoryLimit    string  `db:"memory_limit"`
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

// ─── StartRaw ────────────────────────────────────────────────────────────────
// StartRaw launches a container for a pre-registered endpoint.
// It returns immediately (async) — check endpoint lifecycle_state for progress.
func (c *ModelController) StartRaw(ctx context.Context, endpointID, modelID string, spec RuntimeSpec, actor string) (string, error) {
	opID := c.beginOp(ctx, modelID, endpointID, "start", actor)

	// Immediately mark as downloading so the UI shows activity
	_, _ = c.db.ExecContext(ctx,
		`UPDATE model_endpoints SET lifecycle_state = 'downloading', updated_at = NOW() WHERE id = $1`,
		endpointID)
	c.transition(ctx, endpointID, "registered", "downloading", "async deploy started", actor)

	// Clone values for the goroutine (avoid capturing loop variables)
	epID := endpointID
	mID := modelID
	opIDCopy := opID

	go func() {
		bg := context.Background()

		c.log.Info("launching container",
			zap.String("backend", spec.BackendType),
			zap.String("image", spec.Image),
			zap.String("model", spec.ServedModelName),
		)

		containerID, err := c.driver.Start(bg, spec)
		if err != nil {
			c.log.Error("container start failed",
				zap.String("model", spec.ServedModelName),
				zap.Error(err),
			)
			c.failOpID(bg, opIDCopy, err)
			c.transition(bg, epID, "downloading", "failed", err.Error(), actor)
			_, _ = c.db.ExecContext(bg,
				`UPDATE model_endpoints SET lifecycle_state = 'failed', updated_at = NOW() WHERE id = $1`, epID)
			return
		}

		// Container is running
		_, _ = c.db.ExecContext(bg,
			`UPDATE model_endpoints SET container_id = $1, lifecycle_state = 'loading', updated_at = NOW() WHERE id = $2`,
			containerID, epID)
		c.transition(bg, epID, "downloading", "loading", "container started", actor)
		c.succeedOpID(bg, opIDCopy, containerID)

		c.log.Info("container running",
			zap.String("container_id", containerID),
			zap.String("model", spec.ServedModelName),
			zap.String("backend", spec.BackendType),
		)

		// Ollama needs `ollama pull <model>` after the server starts
		if spec.BackendType == "ollama" && spec.ServedModelName != "" {
			c.log.Info("waiting for Ollama server to init", zap.String("model", spec.ServedModelName))
			time.Sleep(8 * time.Second)

			out, pullErr := exec.Command(
				"docker", "exec", containerID,
				"ollama", "pull", spec.ServedModelName,
			).CombinedOutput()

			if pullErr != nil {
				c.log.Error("ollama pull failed",
					zap.String("model", spec.ServedModelName),
					zap.String("output", string(out)),
					zap.Error(pullErr),
				)
				_, _ = c.db.ExecContext(bg,
					`UPDATE model_endpoints SET lifecycle_state = 'failed', updated_at = NOW() WHERE id = $1`, epID)
			} else {
				c.log.Info("ollama model ready", zap.String("model", spec.ServedModelName))
				_, _ = c.db.ExecContext(bg,
					`UPDATE model_endpoints SET lifecycle_state = 'active', health_status = 'healthy', updated_at = NOW() WHERE id = $1`, epID)
				c.publishEvent(bg, epID, "active")
			}
		}

		// Suppress unused variable warnings
		_ = mID
	}()

	return "", nil
}

// ─── Lifecycle operations ────────────────────────────────────────────────────

// Start launches the runtime for an existing endpoint record.
func (c *ModelController) Start(ctx context.Context, endpointID, actor string) error {
	ep, err := c.loadEndpoint(ctx, endpointID)
	if err != nil {
		return err
	}
	spec := c.buildSpec(ep)
	_, err = c.StartRaw(ctx, endpointID, ep.ModelID, spec, actor)
	return err
}

// Stop gracefully stops the runtime.
func (c *ModelController) Stop(ctx context.Context, endpointID, actor string) error {
	ep, err := c.loadEndpoint(ctx, endpointID)
	if err != nil {
		return err
	}
	opID := c.beginOp(ctx, ep.ModelID, endpointID, "stop", actor)
	if err := c.driver.Stop(ctx, ep.ContainerID, 30*time.Second); err != nil {
		c.failOpID(ctx, opID, err)
		return fmt.Errorf("stop endpoint %s: %w", endpointID, err)
	}
	_, _ = c.db.ExecContext(ctx,
		`UPDATE model_endpoints SET lifecycle_state = 'unloaded', health_status = 'down', updated_at = NOW() WHERE id = $1`,
		endpointID)
	c.transition(ctx, endpointID, ep.LifecycleState, "unloaded", "stopped by "+actor, actor)
	c.succeedOpID(ctx, opID, "")
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
		c.failOpID(ctx, opID, err)
		c.transition(ctx, endpointID, ep.LifecycleState, "failed", "restart failed: "+err.Error(), actor)
		return fmt.Errorf("restart endpoint %s: %w", endpointID, err)
	}
	_, _ = c.db.ExecContext(ctx,
		`UPDATE model_endpoints SET container_id = $1, lifecycle_state = 'loading', health_status = 'unknown', updated_at = NOW() WHERE id = $2`,
		newID, endpointID)
	c.transition(ctx, endpointID, ep.LifecycleState, "loading", "restarted by "+actor, actor)
	c.succeedOpID(ctx, opID, newID)
	return nil
}

// Drain marks the endpoint as draining.
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
	c.succeedOpID(ctx, opID, "")
	c.publishEvent(ctx, endpointID, "draining")
	return nil
}

// Upgrade performs a rolling upgrade with a new image.
func (c *ModelController) Upgrade(ctx context.Context, endpointID, newImage, actor string) error {
	ep, err := c.loadEndpoint(ctx, endpointID)
	if err != nil {
		return err
	}
	opID := c.beginOp(ctx, ep.ModelID, endpointID, "upgrade", actor)
	spec := c.buildSpec(ep)
	spec.Image = newImage
	oldID := ep.ContainerID
	newID, err := c.driver.Restart(ctx, oldID, spec, 60*time.Second)
	if err != nil {
		c.failOpID(ctx, opID, err)
		return fmt.Errorf("upgrade: %w", err)
	}
	_, _ = c.db.ExecContext(ctx,
		`UPDATE model_endpoints SET container_id = $1, runtime_image = $2, lifecycle_state = 'loading', health_status = 'unknown', updated_at = NOW() WHERE id = $3`,
		newID, newImage, endpointID)
	c.transition(ctx, endpointID, ep.LifecycleState, "loading", "upgraded to "+newImage, actor)
	c.succeedOpID(ctx, opID, newID)
	return nil
}

// Rollback restarts with a previous image.
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
		c.failOpID(ctx, opID, err)
		return fmt.Errorf("rollback: %w", err)
	}
	_, _ = c.db.ExecContext(ctx,
		`UPDATE model_endpoints SET container_id = $1, runtime_image = $2, lifecycle_state = 'loading', health_status = 'unknown', updated_at = NOW() WHERE id = $3`,
		newID, previousImage, endpointID)
	c.transition(ctx, endpointID, ep.LifecycleState, "loading", "rolled back to "+previousImage, actor)
	c.succeedOpID(ctx, opID, newID)
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
			COALESCE(me.container_id, '')         AS container_id,
			COALESCE(me.lifecycle_state,'registered') AS lifecycle_state,
			COALESCE(mrc.tensor_parallel, 1)      AS tensor_parallel,
			COALESCE(mrc.gpu_memory_util, 0.9)    AS gpu_memory_util,
			COALESCE(mrc.max_batch_size, 256)     AS max_model_len,
			COALESCE(mrc.dtype, 'auto')           AS dtype,
			COALESCE(mrc.quantization, '')        AS quantization,
			COALESCE(me.runtime_image, 'vllm/vllm-openai:latest') AS runtime_image,
			COALESCE(me.runtime_type, 'GPU_RUNTIME') AS runtime_type,
			COALESCE(mrc.cpu_threads, 0)          AS cpu_threads,
			COALESCE(mrc.numa_node, -1)           AS numa_node,
			COALESCE(mrc.memory_limit, '')        AS memory_limit
		FROM model_endpoints me
		JOIN models m ON m.id = me.model_id
		LEFT JOIN model_runtime_configs mrc ON mrc.model_id = me.model_id
		WHERE me.id = $1`, endpointID)
	return ep, err
}

func (c *ModelController) buildSpec(ep EndpointRecord) RuntimeSpec {
	return RuntimeSpec{
		ModelName:       ep.ModelName,
		ServedModelName: ep.ModelName,
		EndpointID:      ep.ID,
		BackendType:     ep.BackendType,
		Image:           ep.RuntimeImage,
		BindHost:        ep.Host,
		BindPort:        ep.Port,
		TensorParallel:  ep.TensorParallel,
		GPUMemoryUtil:   ep.GPUMemoryUtil,
		MaxModelLen:     ep.MaxModelLen,
		Dtype:           ep.Dtype,
		Quantization:    ep.Quantization,
		RuntimeType:     ep.RuntimeType,
		NUMANode:        ep.NUMANode,
		MemoryLimit:     ep.MemoryLimit,
		Env:             map[string]string{},
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

func (c *ModelController) succeedOpID(ctx context.Context, opID, containerID string) {
	meta := fmt.Sprintf(`{"container_id":%q}`, containerID)
	_, _ = c.db.ExecContext(ctx,
		`UPDATE controller_operations SET status='success', completed_at=NOW(), metadata=$1 WHERE id=$2`,
		meta, opID)
}

func (c *ModelController) failOpID(ctx context.Context, opID string, err error) {
	_, _ = c.db.ExecContext(ctx,
		`UPDATE controller_operations SET status='failed', completed_at=NOW(), error_msg=$1 WHERE id=$2`,
		err.Error(), opID)
}
