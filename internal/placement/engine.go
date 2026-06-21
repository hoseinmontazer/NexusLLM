package placement

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
)

// ─────────────────────────────────────────────────────────────────────────────
// Engine
// ─────────────────────────────────────────────────────────────────────────────

// Engine decides where to place a service given current cluster state.
// It is query-time stateless (reads from DB on every Decide call) so that
// concurrent deployments see an up-to-date resource picture.
type Engine struct {
	db  *sqlx.DB
	log *zap.Logger
}

// NewEngine constructs a placement Engine.
func NewEngine(db *sqlx.DB, log *zap.Logger) *Engine {
	return &Engine{db: db, log: log}
}

// ─── database projections ─────────────────────────────────────────────────────

type dbNode struct {
	ID           string `db:"id"`
	Hostname     string `db:"hostname"`
	Status       string `db:"status"`
	TotalCPU     int    `db:"total_cpu"`
	TotalRAMMB   int64  `db:"total_ram_mb"`
	TotalVRAMMB  int64  `db:"total_vram_mb"`
}

type dbGPU struct {
	ID          string `db:"id"`
	NodeID      string `db:"node_id"`
	DeviceIndex int    `db:"device_index"`
	Name        string `db:"name"`
	VRAMMB      int64  `db:"vram_mb"`
	Status      string `db:"status"`
	UtilPct     int    `db:"utilization_pct"`
	TempC       int    `db:"temperature_c"`
	PowerW      int    `db:"power_draw_w"`
	NUMANode    int    `db:"numa_node"`
}

type cpuUsage struct {
	NodeID      string `db:"node_id"`
	AllocCPU    int    `db:"alloc_cpu"`
	AllocRAMMB  int64  `db:"alloc_ram_mb"`
}

// ─── Decide ──────────────────────────────────────────────────────────────────

// Decide selects the best resources for the given placement request.
// It writes a placement_decisions row (applied=false) and returns the decision.
// The caller is responsible for calling Apply() once the deployment succeeds.
func (e *Engine) Decide(ctx context.Context, req Request) (*Decision, error) {
	if req.RuntimeType == RuntimeCPU {
		return e.decideCPU(ctx, req)
	}
	return e.decideGPU(ctx, req)
}

// Apply marks a previously returned decision as applied and persists the
// CPU or GPU allocations in the database.
func (e *Engine) Apply(ctx context.Context, dec *Decision, req Request, endpointID string) error {
	// Record the placement decision as applied
	_, _ = e.db.ExecContext(ctx, `
		UPDATE placement_decisions
		SET applied = TRUE
		WHERE model_id = $1 AND applied = FALSE
		ORDER BY created_at DESC
		LIMIT 1`,
		req.ModelID)

	if req.RuntimeType == RuntimeCPU {
		return e.applyCPU(ctx, dec, endpointID)
	}
	// GPU allocations are handled by the existing gpu.Inventory.Allocate path.
	// We record the decision link here.
	return nil
}

// ─── GPU placement ────────────────────────────────────────────────────────────

func (e *Engine) decideGPU(ctx context.Context, req Request) (*Decision, error) {
	// 1. Enumerate candidate nodes
	nodes, err := e.loadOnlineNodes(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("placement: load nodes: %w", err)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("placement: no online nodes available")
	}

	// 2. For each candidate node, score GPU fit
	type candidate struct {
		node    dbNode
		devices []dbGPU
		score   float64
		reason  string
	}

	var candidates []candidate

	for _, node := range nodes {
		devs, err := e.loadAvailableGPUs(ctx, node.ID, req.MinVRAMMB)
		if err != nil {
			continue
		}
		if len(devs) < req.GPUCount {
			continue
		}

		// Score: prefer low utilisation + low temperature + contiguous NUMA
		score := e.scoreGPU(devs[:req.GPUCount], req)
		candidates = append(candidates, candidate{
			node:    node,
			devices: devs[:req.GPUCount],
			score:   score,
			reason:  fmt.Sprintf("node=%s score=%.2f", node.Hostname, score),
		})
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("placement: insufficient GPU resources (need %d GPUs × %d MB VRAM)",
			req.GPUCount, req.MinVRAMMB)
	}

	// 3. Pick best candidate (highest score)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})
	best := candidates[0]

	deviceIndices := make([]int, len(best.devices))
	deviceIDs := make([]string, len(best.devices))
	var totalVRAM int64
	for i, d := range best.devices {
		deviceIndices[i] = d.DeviceIndex
		deviceIDs[i] = d.ID
		totalVRAM += d.VRAMMB
	}

	dec := &Decision{
		NodeID:           best.node.ID,
		NodeHost:         best.node.Hostname,
		GPUDeviceIndices: deviceIndices,
		GPUDeviceIDs:     deviceIDs,
		TotalVRAMMB:      totalVRAM,
		CPUCores:         req.CPUCores,
		NUMANode:         e.bestNUMAFromDevices(best.devices),
		Strategy:         "gpu_score",
		Score:            best.score,
		Reason:           best.reason,
		DecidedAt:        time.Now(),
	}

	e.recordDecision(ctx, req, dec, endpointID(""))
	e.log.Info("GPU placement decided",
		zap.String("model", req.ModelName),
		zap.String("node", dec.NodeHost),
		zap.Ints("gpus", dec.GPUDeviceIndices),
		zap.Int64("vram_mb", dec.TotalVRAMMB),
	)
	return dec, nil
}

// scoreGPU computes a desirability score for a set of GPU devices.
// Higher score = more desirable. Score is in [0, 1000].
func (e *Engine) scoreGPU(devs []dbGPU, req Request) float64 {
	if len(devs) == 0 {
		return 0
	}

	var totalUtil, totalTemp, totalPower float64
	sameNUMA := true
	firstNUMA := devs[0].NUMANode
	for _, d := range devs {
		totalUtil += float64(d.UtilPct)
		totalTemp += float64(d.TempC)
		totalPower += float64(d.PowerW)
		if d.NUMANode != firstNUMA {
			sameNUMA = false
		}
	}
	n := float64(len(devs))
	avgUtil := totalUtil / n
	avgTemp := totalTemp / n

	// Base score from low utilisation (0–400)
	utilScore := (1.0 - avgUtil/100.0) * 400.0

	// Temperature score: prefer cooler GPUs (0–200)
	// Assume max operating temp ~85°C
	tempScore := (1.0 - avgTemp/85.0) * 200.0
	if tempScore < 0 {
		tempScore = 0
	}

	// NUMA locality bonus (0–200)
	numaScore := 0.0
	if sameNUMA {
		numaScore = 200.0
	}
	if req.NUMANode >= 0 && devs[0].NUMANode == req.NUMANode {
		numaScore += 100.0 // bonus for matching requested NUMA
	}

	// VRAM headroom score: prefer devices with enough headroom for max_vram (0–200)
	vramScore := 0.0
	if req.MaxVRAMMB > 0 {
		for _, d := range devs {
			if d.VRAMMB >= req.MaxVRAMMB {
				vramScore += 200.0 / n
			} else {
				vramScore += float64(d.VRAMMB-req.MinVRAMMB) / float64(req.MaxVRAMMB-req.MinVRAMMB) * (200.0 / n)
			}
		}
	} else {
		vramScore = 100.0 // neutral
	}

	return utilScore + tempScore + numaScore + vramScore
}

// ─── CPU placement ────────────────────────────────────────────────────────────

func (e *Engine) decideCPU(ctx context.Context, req Request) (*Decision, error) {
	nodes, err := e.loadOnlineNodes(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("placement: load nodes: %w", err)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("placement: no online nodes available")
	}

	// Load current CPU allocations per node
	usages, err := e.loadCPUUsage(ctx)
	if err != nil {
		return nil, fmt.Errorf("placement: load cpu usage: %w", err)
	}
	usageMap := make(map[string]cpuUsage, len(usages))
	for _, u := range usages {
		usageMap[u.NodeID] = u
	}

	type candidate struct {
		node  dbNode
		score float64
	}
	var candidates []candidate

	neededCPU := req.CPUCores
	if neededCPU == 0 {
		neededCPU = 1 // allocate at least 1 core
	}
	neededRAM := req.RAMMBLimit

	for _, node := range nodes {
		usage := usageMap[node.ID]
		freeCPU := node.TotalCPU - usage.AllocCPU
		freeRAM := node.TotalRAMMB - usage.AllocRAMMB

		if freeCPU < neededCPU {
			continue
		}
		if neededRAM > 0 && freeRAM < neededRAM {
			continue
		}

		// Score: prefer node with more free CPU (normalised)
		cpuScore := float64(freeCPU) / float64(node.TotalCPU) * 500.0
		ramScore := 0.0
		if node.TotalRAMMB > 0 {
			ramScore = float64(freeRAM) / float64(node.TotalRAMMB) * 200.0
		}
		candidates = append(candidates, candidate{node: node, score: cpuScore + ramScore})
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("placement: insufficient CPU resources (need %d cores, %d MB RAM)",
			neededCPU, neededRAM)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})
	best := candidates[0]

	// Pick NUMA node: if requested, use it; otherwise pick the NUMA node that
	// has the most idle CPUs (heuristic: even NUMA on even models).
	numaNode := req.NUMANode
	if numaNode < 0 {
		numaNode = e.pickNUMANode(ctx, best.node.ID)
	}

	dec := &Decision{
		NodeID:    best.node.ID,
		NodeHost:  best.node.Hostname,
		CPUCores:  neededCPU,
		NUMANode:  numaNode,
		RAMMBLimit: neededRAM,
		Strategy:  "cpu_score",
		Score:     best.score,
		Reason: fmt.Sprintf("node=%s free_cpu=%d numa=%d",
			best.node.Hostname,
			best.node.TotalCPU-usageMap[best.node.ID].AllocCPU,
			numaNode,
		),
		DecidedAt: time.Now(),
	}

	e.recordDecision(ctx, req, dec, endpointID(""))
	e.log.Info("CPU placement decided",
		zap.String("model", req.ModelName),
		zap.String("node", dec.NodeHost),
		zap.Int("cpu_cores", dec.CPUCores),
		zap.Int("numa_node", dec.NUMANode),
	)
	return dec, nil
}

// ─── Apply CPU allocation ─────────────────────────────────────────────────────

func (e *Engine) applyCPU(ctx context.Context, dec *Decision, epID string) error {
	_, err := e.db.ExecContext(ctx, `
		INSERT INTO cpu_allocations (id, endpoint_id, node_id, cpu_cores, numa_node, ram_mb)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.New().String(), epID,
		nullableStr(dec.NodeID),
		dec.CPUCores, dec.NUMANode, dec.RAMMBLimit,
	)
	return err
}

// ReleaseCPU frees CPU allocation for an endpoint.
func (e *Engine) ReleaseCPU(ctx context.Context, endpointID string) error {
	_, err := e.db.ExecContext(ctx,
		`UPDATE cpu_allocations SET released_at = NOW()
		 WHERE endpoint_id = $1 AND released_at IS NULL`, endpointID)
	return err
}

// ─── DB helpers ───────────────────────────────────────────────────────────────

func (e *Engine) loadOnlineNodes(ctx context.Context, req Request) ([]dbNode, error) {
	q := `SELECT id, hostname, status, total_cpu, total_ram_mb, total_vram_mb
	      FROM nodes WHERE status IN ('online','degraded')`
	args := []interface{}{}

	if req.RequireNodeID != "" {
		q += " AND id = $1"
		args = append(args, req.RequireNodeID)
	} else if req.PreferNodeID != "" {
		// Return preferred node first; fall through to others if it lacks capacity
		q += " ORDER BY (CASE WHEN id = $1 THEN 0 ELSE 1 END)"
		args = append(args, req.PreferNodeID)
	} else {
		q += " ORDER BY hostname"
	}

	var nodes []dbNode
	err := e.db.SelectContext(ctx, &nodes, q, args...)
	return nodes, err
}

func (e *Engine) loadAvailableGPUs(ctx context.Context, nodeID string, minVRAMMB int64) ([]dbGPU, error) {
	var devs []dbGPU
	err := e.db.SelectContext(ctx, &devs, `
		SELECT d.id, d.node_id, d.device_index, d.name, d.vram_mb,
		       d.status, d.utilization_pct, d.temperature_c, d.power_draw_w,
		       COALESCE(d.numa_node, 0) AS numa_node
		FROM gpu_devices d
		JOIN gpu_nodes gn ON gn.id = d.node_id
		WHERE gn.node_id = $1
		  AND d.status = 'available'
		  AND d.vram_mb >= $2
		ORDER BY d.utilization_pct ASC, d.temperature_c ASC, d.device_index ASC`,
		nodeID, minVRAMMB,
	)
	return devs, err
}

func (e *Engine) loadCPUUsage(ctx context.Context) ([]cpuUsage, error) {
	var rows []cpuUsage
	err := e.db.SelectContext(ctx, &rows, `
		SELECT node_id,
		       COALESCE(SUM(cpu_cores), 0) AS alloc_cpu,
		       COALESCE(SUM(ram_mb), 0)    AS alloc_ram_mb
		FROM cpu_allocations
		WHERE released_at IS NULL
		GROUP BY node_id`)
	return rows, err
}

func (e *Engine) bestNUMAFromDevices(devs []dbGPU) int {
	if len(devs) == 0 {
		return -1
	}
	// Use the NUMA node of the first device (contiguous block = same NUMA)
	return devs[0].NUMANode
}

func (e *Engine) pickNUMANode(ctx context.Context, nodeID string) int {
	// Heuristic: pick the NUMA node with fewest CPU allocations
	var row struct {
		NUMANode int `db:"numa_node"`
	}
	err := e.db.GetContext(ctx, &row, `
		SELECT COALESCE(numa_node, 0) AS numa_node
		FROM cpu_allocations
		WHERE node_id = $1 AND released_at IS NULL
		GROUP BY numa_node
		ORDER BY COUNT(*) ASC
		LIMIT 1`, nodeID)
	if err != nil {
		return 0
	}
	return row.NUMANode
}

func (e *Engine) recordDecision(ctx context.Context, req Request, dec *Decision, epID string) {
	gpuJSON := "[]"
	if len(dec.GPUDeviceIndices) > 0 {
		gpuJSON = "["
		for i, idx := range dec.GPUDeviceIndices {
			if i > 0 {
				gpuJSON += ","
			}
			gpuJSON += fmt.Sprintf("%d", idx)
		}
		gpuJSON += "]"
	}

	nodeIDVal := interface{}(nil)
	if dec.NodeID != "" {
		nodeIDVal = dec.NodeID
	}

	_, _ = e.db.ExecContext(ctx, `
		INSERT INTO placement_decisions
		  (id, model_id, endpoint_id, node_id, gpu_devices,
		   cpu_cores, numa_node, ram_mb, strategy, score, reason, applied)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,FALSE)`,
		uuid.New().String(),
		nullableStr(req.ModelID),
		nullableStr(epID),
		nodeIDVal,
		gpuJSON,
		dec.CPUCores,
		dec.NUMANode,
		dec.RAMMBLimit,
		dec.Strategy,
		dec.Score,
		dec.Reason,
	)
}

// endpointID is used in Decide before we have an endpoint ID.
// We pass empty string and the caller calls recordDecision again on Apply.
func endpointID(s string) string { return s }

func nullableStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// ─── Placer interface ─────────────────────────────────────────────────────────

// Placer is the interface used by the RuntimeHandler to run auto-placement
// without creating a direct dependency on *Engine.
type Placer interface {
	Decide(ctx context.Context, req Request) (*Decision, error)
	Apply(ctx context.Context, dec *Decision, req Request, endpointID string) error
}

// Ensure *Engine implements Placer.
var _ Placer = (*Engine)(nil)
