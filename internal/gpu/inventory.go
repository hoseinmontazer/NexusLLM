// Package gpu manages GPU inventory, telemetry, and allocation.
package gpu

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
)

// ─────────────────────────────────────────────────────────────────────────────
// Domain types
// ─────────────────────────────────────────────────────────────────────────────

// Node represents a physical or virtual machine with one or more GPUs.
type Node struct {
	ID          string    `db:"id"           json:"id"`
	Name        string    `db:"name"         json:"name"`
	Host        string    `db:"host"         json:"host"`
	DriverType  string    `db:"driver_type"  json:"driver_type"`
	TotalVRAMMB int       `db:"total_vram_mb" json:"total_vram_mb"`
	IsAvailable bool      `db:"is_available" json:"is_available"`
	CreatedAt   time.Time `db:"created_at"   json:"created_at"`
}

// Device represents a single GPU device on a node.
type Device struct {
	ID             string    `db:"id"              json:"id"`
	NodeID         string    `db:"node_id"         json:"node_id"`
	DeviceIndex    int       `db:"device_index"    json:"device_index"`
	Name           string    `db:"name"            json:"name"`
	VRAM_MB        int       `db:"vram_mb"         json:"vram_mb"`
	Status         string    `db:"status"          json:"status"`
	UtilizationPct int       `db:"utilization_pct" json:"utilization_pct"`
	TemperatureC   int       `db:"temperature_c"   json:"temperature_c"`
	PowerDrawW     int       `db:"power_draw_w"    json:"power_draw_w"`
	LastSeenAt     time.Time `db:"last_seen_at"    json:"last_seen_at"`
}

// AllocationRequest describes a request to allocate GPU resources for an endpoint.
type AllocationRequest struct {
	EndpointID     string
	RequiredVRAMMB int
	GPUCount       int
	Strategy       AllocationStrategy
	NodePreference string // optional: prefer a specific node
}

// AllocationResult is the result of a successful GPU allocation.
type AllocationResult struct {
	Devices       []Device
	TotalVRAMMB   int
	NodeID        string
}

// AllocationStrategy controls which GPU selection algorithm is used.
type AllocationStrategy string

const (
	StrategyBestFit       AllocationStrategy = "best_fit"
	StrategyFirstFit      AllocationStrategy = "first_fit"
	StrategyLeastUtilized AllocationStrategy = "least_utilized"
)

// ─────────────────────────────────────────────────────────────────────────────
// Inventory service
// ─────────────────────────────────────────────────────────────────────────────

// Inventory manages GPU nodes, devices, and allocations.
type Inventory struct {
	db  *sqlx.DB
	log *zap.Logger
}

// NewInventory constructs an Inventory.
func NewInventory(db *sqlx.DB, log *zap.Logger) *Inventory {
	return &Inventory{db: db, log: log}
}

// RegisterNode adds a GPU node to the inventory.
func (inv *Inventory) RegisterNode(ctx context.Context, name, host, driverType string) (*Node, error) {
	n := &Node{
		ID:          uuid.New().String(),
		Name:        name,
		Host:        host,
		DriverType:  driverType,
		IsAvailable: true,
		CreatedAt:   time.Now(),
	}
	_, err := inv.db.ExecContext(ctx,
		`INSERT INTO gpu_nodes (id, name, host, driver_type, is_available, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,TRUE,$5,$5)`,
		n.ID, n.Name, n.Host, n.DriverType, n.CreatedAt,
	)
	return n, err
}

// RegisterDevice adds a GPU device under a node.
func (inv *Inventory) RegisterDevice(ctx context.Context, nodeID string, index int, name string, vramMB int) (*Device, error) {
	d := &Device{
		ID:          uuid.New().String(),
		NodeID:      nodeID,
		DeviceIndex: index,
		Name:        name,
		VRAM_MB:     vramMB,
		Status:      "available",
		LastSeenAt:  time.Now(),
	}
	_, err := inv.db.ExecContext(ctx,
		`INSERT INTO gpu_devices (id, node_id, device_index, name, vram_mb, status, last_seen_at, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,'available',$6,$6,$6)`,
		d.ID, d.NodeID, d.DeviceIndex, d.Name, d.VRAM_MB, d.LastSeenAt,
	)
	return d, err
}

// Allocate selects GPUs for an endpoint using the requested strategy.
// It marks selected devices as 'allocated' and inserts gpu_allocations rows.
func (inv *Inventory) Allocate(ctx context.Context, req AllocationRequest) (*AllocationResult, error) {
	devices, err := inv.selectDevices(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("gpu allocation (%s): %w", req.Strategy, err)
	}
	if len(devices) < req.GPUCount {
		return nil, fmt.Errorf("insufficient GPUs: need %d, found %d available with ≥%d MB VRAM",
			req.GPUCount, len(devices), req.RequiredVRAMMB)
	}
	chosen := devices[:req.GPUCount]

	// Mark devices allocated + insert allocation rows
	for _, d := range chosen {
		_, err = inv.db.ExecContext(ctx,
			`UPDATE gpu_devices SET status = 'allocated', updated_at = NOW() WHERE id = $1`, d.ID)
		if err != nil {
			return nil, err
		}
		_, err = inv.db.ExecContext(ctx,
			`INSERT INTO gpu_allocations (id, endpoint_id, gpu_device_id, vram_allocated_mb)
			 VALUES ($1,$2,$3,$4)`,
			uuid.New().String(), req.EndpointID, d.ID, d.VRAM_MB,
		)
		if err != nil {
			return nil, err
		}
	}

	total := 0
	for _, d := range chosen {
		total += d.VRAM_MB
	}
	nodeID := ""
	if len(chosen) > 0 {
		nodeID = chosen[0].NodeID
	}

	inv.log.Info("GPUs allocated",
		zap.String("endpoint_id", req.EndpointID),
		zap.Int("gpu_count", len(chosen)),
		zap.Int("total_vram_mb", total),
	)
	return &AllocationResult{Devices: chosen, TotalVRAMMB: total, NodeID: nodeID}, nil
}

// Release frees all GPU devices allocated to an endpoint.
func (inv *Inventory) Release(ctx context.Context, endpointID string) error {
	// Get allocated device IDs
	var deviceIDs []string
	_ = inv.db.SelectContext(ctx, &deviceIDs,
		`SELECT gpu_device_id FROM gpu_allocations
		 WHERE endpoint_id = $1 AND released_at IS NULL`, endpointID)

	// Mark released
	_, _ = inv.db.ExecContext(ctx,
		`UPDATE gpu_allocations SET released_at = NOW() WHERE endpoint_id = $1 AND released_at IS NULL`,
		endpointID)

	// Mark devices available again
	for _, id := range deviceIDs {
		_, _ = inv.db.ExecContext(ctx,
			`UPDATE gpu_devices SET status = 'available', updated_at = NOW() WHERE id = $1`, id)
	}
	inv.log.Info("GPUs released", zap.String("endpoint_id", endpointID), zap.Int("count", len(deviceIDs)))
	return nil
}

// UpdateTelemetry refreshes utilization/temperature/power for a device.
func (inv *Inventory) UpdateTelemetry(ctx context.Context, deviceID string, utilPct, tempC, powerW int) {
	_, _ = inv.db.ExecContext(ctx,
		`UPDATE gpu_devices SET utilization_pct=$1, temperature_c=$2, power_draw_w=$3,
		 last_seen_at=NOW(), updated_at=NOW() WHERE id=$4`,
		utilPct, tempC, powerW, deviceID)
}

// ListNodes returns all registered GPU nodes with their devices.
func (inv *Inventory) ListNodes(ctx context.Context) ([]Node, error) {
	var nodes []Node
	err := inv.db.SelectContext(ctx, &nodes,
		`SELECT id, name, host, driver_type, total_vram_mb, is_available, created_at
		 FROM gpu_nodes WHERE is_available = TRUE ORDER BY name`)
	return nodes, err
}

// ListDevices returns all devices, optionally filtered by node.
func (inv *Inventory) ListDevices(ctx context.Context, nodeID string) ([]Device, error) {
	var devices []Device
	query := `SELECT id, node_id, device_index, name, vram_mb, status,
	           utilization_pct, temperature_c, power_draw_w, last_seen_at
	          FROM gpu_devices`
	args := []interface{}{}
	if nodeID != "" {
		query += " WHERE node_id = $1"
		args = append(args, nodeID)
	}
	query += " ORDER BY node_id, device_index"
	err := inv.db.SelectContext(ctx, &devices, query, args...)
	return devices, err
}

// ─── allocation algorithms ────────────────────────────────────────────────────

func (inv *Inventory) selectDevices(ctx context.Context, req AllocationRequest) ([]Device, error) {
	baseQ := `
		SELECT d.id, d.node_id, d.device_index, d.name, d.vram_mb,
		       d.status, d.utilization_pct, d.temperature_c, d.power_draw_w, d.last_seen_at
		FROM gpu_devices d
		JOIN gpu_nodes n ON n.id = d.node_id
		WHERE d.status = 'available'
		  AND d.vram_mb >= $1
		  AND n.is_available = TRUE`

	args := []interface{}{req.RequiredVRAMMB}
	if req.NodePreference != "" {
		baseQ += " AND n.name = $2"
		args = append(args, req.NodePreference)
	}

	switch req.Strategy {
	case StrategyLeastUtilized:
		baseQ += " ORDER BY d.utilization_pct ASC, d.vram_mb ASC"
	case StrategyBestFit:
		// Best fit: device whose VRAM is closest to (but ≥) requirement
		baseQ += " ORDER BY d.vram_mb ASC"
	default: // first_fit
		baseQ += " ORDER BY n.name, d.device_index ASC"
	}

	var devices []Device
	err := inv.db.SelectContext(ctx, &devices, baseQ, args...)
	return devices, err
}
