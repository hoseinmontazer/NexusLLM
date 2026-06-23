package runtimemgr

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
)

// ResourceGuard prevents CPU/RAM/GPU oversubscription before starting a
// new container. It reads current node telemetry and active runtime memory
// usage from the DB to determine available headroom.
type ResourceGuard struct {
	db  *sqlx.DB
	log *zap.Logger
}

// NewResourceGuard constructs a ResourceGuard.
func NewResourceGuard(db *sqlx.DB, log *zap.Logger) *ResourceGuard {
	return &ResourceGuard{db: db, log: log}
}

// CanStart returns nil if the node has enough headroom for the given model.
// It checks:
//  1. Available RAM (total - used by running containers)
//  2. GPU VRAM availability (if GPUDevices is non-empty), accounting for
//     higher-or-equal priority project reservations on this node
//  3. No more than MaxConcurrentModels containers already running
func (g *ResourceGuard) CanStart(ctx context.Context, nodeID string, req ResourceRequest) error {
	if req.RAMMBNeeded == 0 {
		// No resource check requested — allow (caller didn't configure requirements)
		return nil
	}

	// ── RAM check ────────────────────────────────────────────────────────────
	var telRow struct {
		RAMTotalMB int64 `db:"ram_total_mb"`
		RAMUsedMB  int64 `db:"ram_used_mb"`
	}
	if err := g.db.GetContext(ctx, &telRow, `
		SELECT ram_total_mb, ram_used_mb
		FROM node_telemetry
		WHERE node_id = $1
		ORDER BY recorded_at DESC
		LIMIT 1`, nodeID); err == nil {
		freeRAMMB := telRow.RAMTotalMB - telRow.RAMUsedMB
		if freeRAMMB < req.RAMMBNeeded {
			g.log.Warn("insufficient RAM for model",
				zap.String("model", req.ModelName),
				zap.String("node_id", nodeID),
				zap.Int64("needed_mb", req.RAMMBNeeded),
				zap.Int64("free_mb", freeRAMMB),
			)
			return fmt.Errorf("%w: need %d MB RAM, only %d MB free on node %s",
				ErrInsufficientResources, req.RAMMBNeeded, freeRAMMB, nodeID)
		}
	}
	// If telemetry is unavailable, allow (degraded mode — agent hasn't pushed yet)

	// ── GPU VRAM check ────────────────────────────────────────────────────────
	if len(req.GPUDevices) > 0 {
		type gpuRow struct {
			DeviceIndex int   `db:"device_index"`
			VRAMMb      int64 `db:"vram_mb"`
			MemUsedMb   int64 `db:"memory_used_mb"`
		}
		var gpus []gpuRow
		if err := g.db.SelectContext(ctx, &gpus, `
			SELECT d.device_index,
			       d.vram_mb,
			       COALESCE(
			         (SELECT gt.memory_used_mb FROM gpu_telemetry gt
			          WHERE gt.device_id = d.id
			          ORDER BY gt.recorded_at DESC LIMIT 1),
			         0
			       ) AS memory_used_mb
			FROM gpu_devices d
			JOIN gpu_nodes gn ON gn.id = d.node_id
			WHERE gn.node_id = $1
			ORDER BY d.device_index`, nodeID); err == nil {

			// How much VRAM is reserved by higher-or-equal priority projects on this node?
			// We subtract this from the raw free VRAM so lower-priority callers cannot
			// consume resources that belong to higher-priority projects.
			var reservedForHigherPriority int64
			if req.ProjectPriority != "" {
				_ = g.db.GetContext(ctx, &reservedForHigherPriority, `
					SELECT COALESCE(SUM(pr.reserved_vram_mb), 0)
					FROM project_reservations pr
					JOIN projects p ON p.id = pr.project_id
					JOIN agent_runtimes ar ON ar.project_id = p.id AND ar.node_id = $1
					WHERE project_priority_score(p.priority) >= project_priority_score($2::varchar)
					  AND p.id::text != COALESCE($3, '')
					  AND ar.state IN ('active','warm','idle','loading')`,
					nodeID, req.ProjectPriority, req.ProjectID)
			}

			// Build a map of device_index → free VRAM (minus reserved headroom, proportioned)
			freeVRAM := make(map[int]int64)
			var totalRawFree int64
			for _, gpu := range gpus {
				raw := gpu.VRAMMb - gpu.MemUsedMb
				freeVRAM[gpu.DeviceIndex] = raw
				totalRawFree += raw
			}

			// Distribute the reservation deduction evenly across selected GPUs
			reservedPerGPU := int64(0)
			if len(gpus) > 0 && totalRawFree > 0 {
				reservedPerGPU = reservedForHigherPriority / int64(len(req.GPUDevices))
			}

			// Each assigned GPU must have enough headroom
			perGPU := req.RAMMBNeeded
			if len(req.GPUDevices) > 0 {
				perGPU = req.RAMMBNeeded / int64(len(req.GPUDevices))
			}
			for _, idx := range req.GPUDevices {
				free, ok := freeVRAM[idx]
				if !ok {
					continue
				}
				effectiveFree := free - reservedPerGPU
				if effectiveFree < perGPU {
					g.log.Warn("insufficient GPU VRAM (accounting for reservations)",
						zap.String("model", req.ModelName),
						zap.Int("gpu", idx),
						zap.Int64("needed_mb", perGPU),
						zap.Int64("free_mb", free),
						zap.Int64("reserved_mb", reservedPerGPU),
						zap.Int64("effective_free_mb", effectiveFree),
					)
					return fmt.Errorf("%w: GPU %d has only %d MB effective VRAM (raw free: %d, reserved: %d), need %d MB",
						ErrInsufficientResources, idx, effectiveFree, free, reservedPerGPU, perGPU)
				}
			}
		}
	}

	return nil
}
