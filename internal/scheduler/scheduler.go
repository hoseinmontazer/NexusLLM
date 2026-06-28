// Package scheduler implements the automatic placement and scheduling engine.
//
// The scheduler decides where to run models based on:
//   - Node capacity (CPU, RAM, GPU VRAM)
//   - Runtime requirements (from model_runtime_configs)
//   - Project priority_weight [0–1000] + effective_priority (with aging/bonuses/penalties)
//   - Workload policy (lazy_load vs always_on)
//
// When capacity is exhausted, the scheduler:
//   - Queues low-priority deployments
//   - Preempts low-priority runtimes for high-priority requests
//   - Unloads idle runtimes to free resources
//
// The scheduler never executes tasks directly — it enqueues START_MODEL tasks
// for the node agent to execute.
package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/preemption"
	"github.com/nexusllm/nexusllm/internal/taskmanager"
	"go.uber.org/zap"
)

// Scheduler is the automatic placement and scheduling engine.
type Scheduler struct {
	db        *sqlx.DB
	taskMgr   *taskmanager.Manager
	preemptor *preemption.Engine
	log       *zap.Logger

	// Configuration
	queuePollInterval time.Duration
	placementTimeout  time.Duration
}

// NewScheduler constructs a Scheduler.
func NewScheduler(
	db *sqlx.DB,
	taskMgr *taskmanager.Manager,
	preemptor *preemption.Engine,
	log *zap.Logger,
) *Scheduler {
	return &Scheduler{
		db:                db,
		taskMgr:           taskMgr,
		preemptor:         preemptor,
		log:               log,
		queuePollInterval: 30 * time.Second,
		placementTimeout:  10 * time.Second,
	}
}

// Start begins the queue processor background loop.
// Blocks until ctx is cancelled.
func (s *Scheduler) Start(ctx context.Context) {
	s.log.Info("scheduler queue processor started",
		zap.Duration("poll_interval", s.queuePollInterval),
	)

	ticker := time.NewTicker(s.queuePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Info("scheduler queue processor stopped")
			return
		case <-ticker.C:
			s.processQueue(ctx)
		}
	}
}

// processQueue retries queued deployments that couldn't be placed initially.
func (s *Scheduler) processQueue(ctx context.Context) {
	var queued []QueuedDeployment
	err := s.db.SelectContext(ctx, &queued, `
		SELECT id, project_id, runtime_config, priority_score,
		       required_vram_mb, required_ram_mb, required_cpu,
		       execution_mode, prefer_node_id,
		       attempts, enqueued_at, last_attempt_at
		FROM deployment_queue
		WHERE status = 'pending'
		  AND (next_retry_at IS NULL OR next_retry_at <= NOW())
		ORDER BY priority_score DESC, enqueued_at ASC
		LIMIT 10
	`)

	if err != nil {
		s.log.Warn("processQueue: query failed", zap.Error(err))
		return
	}

	if len(queued) == 0 {
		return
	}

	s.log.Info("processing deployment queue", zap.Int("pending", len(queued)))

	for _, item := range queued {
		req, err := item.ToPlacementRequest()
		if err != nil {
			s.log.Warn("invalid queue item", zap.String("id", item.ID), zap.Error(err))
			continue
		}

		// Attempt placement
		dec, err := s.Decide(ctx, req)
		if err != nil {
			// Still no capacity — exponential backoff
			nextRetry := time.Now().Add(s.backoff(item.Attempts))
			_, _ = s.db.ExecContext(ctx, `
				UPDATE deployment_queue
				SET attempts = attempts + 1,
				    last_attempt_at = NOW(),
				    next_retry_at = $1,
				    last_error = $2
				WHERE id = $3`,
				nextRetry, err.Error(), item.ID,
			)
			continue
		}

		// Success — mark deployed and remove from queue
		_, _ = s.db.ExecContext(ctx, `
			UPDATE deployment_queue
			SET status = 'deployed', last_attempt_at = NOW()
			WHERE id = $1`,
			item.ID,
		)

		s.log.Info("queued deployment placed",
			zap.String("queue_id", item.ID),
			zap.String("model_id", req.ModelID),
			zap.String("node_id", dec.NodeID),
		)
	}
}

// Decide selects the best node for a placement request.
// Returns PlacementDecision or ErrInsufficientCapacity if no node can fit.
func (s *Scheduler) Decide(ctx context.Context, req PlacementRequest) (*PlacementDecision, error) {
	start := time.Now()

	// 1. Load candidate nodes
	candidates, err := s.loadCandidateNodes(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("load candidates: %w", err)
	}

	if len(candidates) == 0 {
		return s.handleNoCapacity(ctx, req)
	}

	// 2. Score each candidate
	scored := make([]ScoredNode, 0, len(candidates))
	for _, node := range candidates {
		score := s.scoreNode(ctx, node, req)
		scored = append(scored, ScoredNode{Node: node, Score: score})
	}

	// 3. Sort by score descending (highest score wins)
	sortScoredNodes(scored)

	// 4. Build decision from best candidate
	best := scored[0]
	dec := s.buildDecision(ctx, best, req, scored[1:])

	// 5. Record decision
	decisionID := s.recordDecision(ctx, req, dec)
	dec.DecisionID = decisionID

	s.log.Info("placement decided",
		zap.String("model_id", req.ModelID),
		zap.String("node_id", dec.NodeID),
		zap.String("node_hostname", dec.NodeHostname),
		zap.Float64("score", dec.NodeScore),
		zap.String("reason", dec.Reason),
		zap.Duration("latency", time.Since(start)),
	)

	return dec, nil
}

// Apply executes the placement decision by enqueuing a START_MODEL task.
func (s *Scheduler) Apply(ctx context.Context, dec *PlacementDecision, req PlacementRequest) (string, error) {
	// The START_MODEL task is enqueued by the RuntimeActivator or admin handler.
	// The scheduler's role ends at Decide() — it only selects WHERE to place,
	// not HOW to deploy (that's the RuntimeActivator's responsibility).
	//
	// Mark the decision as applied in the audit log.
	_, _ = s.db.ExecContext(ctx, `
		UPDATE scheduler_decisions
		SET outcome = 'success', completed_at = NOW()
		WHERE id = $1`,
		dec.DecisionID,
	)

	return dec.DecisionID, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Scheduler helpers — node loading, scoring, decision building
// ─────────────────────────────────────────────────────────────────────────────

// loadCandidateNodes returns online, uncordoned nodes that meet the hard constraints.
func (s *Scheduler) loadCandidateNodes(ctx context.Context, req PlacementRequest) ([]Node, error) {
	type dbRow struct {
		ID           string  `db:"id"`
		Hostname     string  `db:"hostname"`
		Status       string  `db:"status"`
		TotalCPU     int     `db:"total_cpu"`
		TotalRAMMB   int64   `db:"total_ram_mb"`
		TotalVRAMMB  int64   `db:"total_vram_mb"`
		CPUUtilPct   float64 `db:"cpu_util_pct"`
		RuntimeCount int     `db:"runtime_count"`
		Labels       []byte  `db:"labels"`
	}

	// ── Build base query ──────────────────────────────────────────────────────
	q := `
		SELECT n.id, n.hostname, n.status,
		       n.total_cpu, n.total_ram_mb, n.total_vram_mb,
		       COALESCE(nt.cpu_util_pct, 0) AS cpu_util_pct,
		       COUNT(ar.id) FILTER (WHERE ar.state IN ('ready','active','warm','loading_model')) AS runtime_count,
		       COALESCE(n.labels, '{}') AS labels
		FROM nodes n
		LEFT JOIN LATERAL (
		    SELECT cpu_util_pct FROM node_telemetry
		    WHERE node_id = n.id ORDER BY recorded_at DESC LIMIT 1
		) nt ON TRUE
		LEFT JOIN agent_runtimes ar ON ar.node_id = n.id
		WHERE n.status IN ('online','degraded')
		  AND n.cordoned = FALSE`

	args := []interface{}{}
	argIdx := 1

	// ── Mode-specific filtering ───────────────────────────────────────────────
	switch req.Mode {
	case ModeSpecificNode:
		// Hard pin — only one candidate, validated below
		if req.SpecificNodeID == "" {
			return nil, fmt.Errorf("placement: specific_node mode requires node_id")
		}
		q += fmt.Sprintf(" AND n.id = $%d", argIdx)
		args = append(args, req.SpecificNodeID)
		argIdx++

	case ModeNodeGroup:
		if req.NodeGroupID == "" {
			return nil, fmt.Errorf("placement: node_group mode requires node_group_id")
		}
		// Nodes with label node_group=<id>
		q += fmt.Sprintf(" AND n.labels @> $%d::jsonb", argIdx)
		args = append(args, fmt.Sprintf(`{"node_group":"%s"}`, req.NodeGroupID))
		argIdx++

	case ModeLabelSelector:
		if len(req.NodeSelector) == 0 {
			return nil, fmt.Errorf("placement: label_selector mode requires at least one label")
		}
		// Build a single jsonb containment check: labels @> '{"k1":"v1","k2":"v2"}'
		selectorJSON, jerr := json.Marshal(req.NodeSelector)
		if jerr != nil {
			return nil, fmt.Errorf("placement: invalid node_selector: %w", jerr)
		}
		q += fmt.Sprintf(" AND n.labels @> $%d::jsonb", argIdx)
		args = append(args, string(selectorJSON))
		argIdx++

	default: // ModeAuto / StrategyPinned legacy
		// Strategy: pinned → hard node requirement
		switch req.Strategy {
		case StrategyPinned:
			if req.PinnedNodeID != "" {
				q += fmt.Sprintf(" AND n.id = $%d", argIdx)
				args = append(args, req.PinnedNodeID)
				argIdx++
			}
		default:
			if req.RequireNodeID != "" {
				q += fmt.Sprintf(" AND n.id = $%d", argIdx)
				args = append(args, req.RequireNodeID)
				argIdx++
			}
		}
	}

	q += " GROUP BY n.id, n.hostname, n.status, n.total_cpu, n.total_ram_mb, n.total_vram_mb, nt.cpu_util_pct, n.labels"
	q += " ORDER BY n.total_vram_mb DESC"

	var rows []dbRow
	if err := s.db.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("loadCandidateNodes: %w", err)
	}

	// ── Enrich nodes with live resource data ──────────────────────────────────
	nodes := make([]Node, 0, len(rows))
	for _, r := range rows {
		node := Node{
			ID:           r.ID,
			Hostname:     r.Hostname,
			Status:       r.Status,
			TotalCPU:     r.TotalCPU,
			TotalRAMMB:   r.TotalRAMMB,
			TotalVRAMMB:  r.TotalVRAMMB,
			CPUUtilPct:   r.CPUUtilPct,
			RuntimeCount: r.RuntimeCount,
		}

		node.GPUDevices, node.FreeVRAMMB = s.loadGPUState(ctx, r.ID)
		node.HasGPU = len(node.GPUDevices) > 0
		node.GPUCount = len(node.GPUDevices)

		var ramAvail int64
		_ = s.db.GetContext(ctx, &ramAvail,
			`SELECT COALESCE(ram_avail_mb, 0) FROM node_telemetry WHERE node_id=$1 ORDER BY recorded_at DESC LIMIT 1`,
			r.ID)
		if ramAvail == 0 && r.TotalRAMMB > 0 {
			ramAvail = r.TotalRAMMB / 2
		}
		node.FreeRAMMB = ramAvail

		var allocCPU int
		_ = s.db.GetContext(ctx, &allocCPU,
			`SELECT COALESCE(SUM(cpu_cores),0) FROM cpu_allocations WHERE node_id=$1 AND released_at IS NULL`, r.ID)
		node.FreeCPU = r.TotalCPU - allocCPU

		// ── Hard resource validation ──────────────────────────────────────────
		// For specific_node mode: reject with a descriptive error instead of silently skipping.
		if req.Mode == ModeSpecificNode {
			if err := s.validateNodeResources(node, req); err != nil {
				return nil, err // caller gets the validation error directly
			}
		} else {
			// For all other modes, silently skip nodes that can't accommodate the request
			if req.RequiredVRAMMB > 0 && node.FreeVRAMMB < req.RequiredVRAMMB {
				continue
			}
			if req.RequiredRAMMB > 0 && node.FreeRAMMB < req.RequiredRAMMB {
				continue
			}
			if req.RequiredCPU > 0 && node.FreeCPU < req.RequiredCPU {
				continue
			}
			if req.ExecutionMode == "gpu" && !node.HasGPU {
				continue
			}
		}

		// Accelerator requirement filter
		switch req.AcceleratorReq {
		case AcceleratorGPU:
			if !node.HasGPU {
				continue
			}
		case AcceleratorCPU:
			if node.HasGPU {
				continue
			}
		}

		nodes = append(nodes, node)
	}

	// ── Spread / packed ordering for non-specific modes ───────────────────────
	if req.Strategy == StrategySpread && req.ModelID != "" {
		var existingNodeIDs []string
		_ = s.db.SelectContext(ctx, &existingNodeIDs, `
			SELECT DISTINCT node_id::text FROM agent_runtimes
			WHERE model_id = $1
			  AND state NOT IN ('stopped','deleted','archived','unloaded','failed','lost')`,
			req.ModelID)
		existing := make(map[string]bool, len(existingNodeIDs))
		for _, id := range existingNodeIDs {
			existing[id] = true
		}
		var spread, other []Node
		for _, n := range nodes {
			if existing[n.ID] {
				other = append(other, n)
			} else {
				spread = append(spread, n)
			}
		}
		if len(spread) > 0 {
			nodes = append(spread, other...)
		}
	}

	if req.Strategy == StrategyPacked && req.ModelID != "" {
		var existingNodeIDs []string
		_ = s.db.SelectContext(ctx, &existingNodeIDs, `
			SELECT DISTINCT node_id::text FROM agent_runtimes
			WHERE model_id = $1
			  AND state NOT IN ('stopped','deleted','archived','unloaded','failed','lost')`,
			req.ModelID)
		existing := make(map[string]bool, len(existingNodeIDs))
		for _, id := range existingNodeIDs {
			existing[id] = true
		}
		var packed, other []Node
		for _, n := range nodes {
			if existing[n.ID] {
				packed = append(packed, n)
			} else {
				other = append(other, n)
			}
		}
		if len(packed) > 0 {
			nodes = append(packed, other...)
		}
	}

	return nodes, nil
}

// validateNodeResources checks whether a specific node can accommodate the request.
// Returns a PlacementValidationError with human-readable details if it cannot.
func (s *Scheduler) validateNodeResources(node Node, req PlacementRequest) error {
	requested := map[string]int64{}
	available := map[string]int64{}
	failures := []string{}

	if req.RequiredVRAMMB > 0 {
		requested["vram_mb"] = req.RequiredVRAMMB
		available["vram_mb"] = node.FreeVRAMMB
		if node.FreeVRAMMB < req.RequiredVRAMMB {
			failures = append(failures,
				fmt.Sprintf("VRAM: requested %dMB (%.0fGB), available %dMB (%.0fGB)",
					req.RequiredVRAMMB, float64(req.RequiredVRAMMB)/1024,
					node.FreeVRAMMB, float64(node.FreeVRAMMB)/1024))
		}
	}
	if req.RequiredRAMMB > 0 {
		requested["ram_mb"] = req.RequiredRAMMB
		available["ram_mb"] = node.FreeRAMMB
		if node.FreeRAMMB < req.RequiredRAMMB {
			failures = append(failures,
				fmt.Sprintf("RAM: requested %dMB (%.0fGB), available %dMB (%.0fGB)",
					req.RequiredRAMMB, float64(req.RequiredRAMMB)/1024,
					node.FreeRAMMB, float64(node.FreeRAMMB)/1024))
		}
	}
	if req.RequiredCPU > 0 {
		requested["cpu"] = int64(req.RequiredCPU)
		available["cpu"] = int64(node.FreeCPU)
		if node.FreeCPU < req.RequiredCPU {
			failures = append(failures,
				fmt.Sprintf("CPU: requested %d cores, available %d cores",
					req.RequiredCPU, node.FreeCPU))
		}
	}
	if req.AcceleratorReq == AcceleratorGPU && !node.HasGPU {
		failures = append(failures, "GPU required but node has no GPU devices")
	}
	if req.ExecutionMode == "gpu" && !node.HasGPU {
		failures = append(failures, "GPU execution mode requested but node has no GPUs")
	}

	if len(failures) == 0 {
		return nil
	}

	reason := fmt.Sprintf("node %q cannot accommodate deployment: %s",
		node.Hostname, joinStrings(failures, "; "))
	return &PlacementValidationError{
		Reason:    reason,
		Requested: requested,
		Available: available,
	}
}

func joinStrings(ss []string, sep string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += sep
		}
		result += s
	}
	return result
}

// loadGPUState loads GPU devices for a node and computes total free VRAM.
func (s *Scheduler) loadGPUState(ctx context.Context, nodeID string) ([]GPUDevice, int64) {
	type row struct {
		ID             string `db:"id"`
		DeviceIndex    int    `db:"device_index"`
		Name           string `db:"name"`
		VRAMMB         int64  `db:"vram_mb"`
		MemUsedMB      int64  `db:"mem_used_mb"`
		UtilizationPct int    `db:"utilization_pct"`
		TemperatureC   int    `db:"temperature_c"`
		NUMANode       int    `db:"numa_node"`
	}
	var rows []row
	_ = s.db.SelectContext(ctx, &rows, `
		SELECT d.id, d.device_index, d.name, d.vram_mb,
		       COALESCE(gt.memory_used_mb,0) AS mem_used_mb,
		       COALESCE(d.utilization_pct,0) AS utilization_pct,
		       COALESCE(d.temperature_c,0) AS temperature_c,
		       COALESCE(d.numa_node,0) AS numa_node
		FROM gpu_devices d
		JOIN gpu_nodes gn ON gn.id = d.node_id
		LEFT JOIN LATERAL (
		    SELECT memory_used_mb FROM gpu_telemetry WHERE device_id=d.id ORDER BY recorded_at DESC LIMIT 1
		) gt ON TRUE
		WHERE gn.node_id=$1`, nodeID)

	var freeVRAM int64
	devices := make([]GPUDevice, 0, len(rows))
	for _, r := range rows {
		devices = append(devices, GPUDevice{
			ID:             r.ID,
			DeviceIndex:    r.DeviceIndex,
			Name:           r.Name,
			VRAMMB:         r.VRAMMB,
			MemUsedMB:      r.MemUsedMB,
			UtilizationPct: r.UtilizationPct,
			TemperatureC:   r.TemperatureC,
			NUMANode:       r.NUMANode,
		})
		freeVRAM += r.VRAMMB - r.MemUsedMB
	}
	return devices, freeVRAM
}

// scoreNode computes a desirability score [0–1000] for a candidate node.
// Components: capacity (0–400) + load (0–300) + locality (0–200) + priority bonus (0–200).
func (s *Scheduler) scoreNode(ctx context.Context, node Node, req PlacementRequest) float64 {
	var score float64

	// ── Capacity score (0–400) ────────────────────────────────────────────────
	if req.RequiredVRAMMB > 0 && node.TotalVRAMMB > 0 {
		ratio := float64(node.FreeVRAMMB) / float64(node.TotalVRAMMB)
		score += ratio * 200
	} else {
		score += 100
	}
	if node.TotalRAMMB > 0 {
		ratio := float64(node.FreeRAMMB) / float64(node.TotalRAMMB)
		score += ratio * 100
	} else {
		score += 50
	}
	if node.TotalCPU > 0 {
		ratio := float64(node.FreeCPU) / float64(node.TotalCPU)
		score += ratio * 100
	} else {
		score += 50
	}

	// ── Load score (0–300) ────────────────────────────────────────────────────
	gpuUtilScore := 0.0
	for _, gpu := range node.GPUDevices {
		gpuUtilScore += (1.0 - float64(gpu.UtilizationPct)/100.0) * 150.0 / float64(max(1, len(node.GPUDevices)))
	}
	if len(node.GPUDevices) == 0 {
		gpuUtilScore = 75
	}
	score += gpuUtilScore

	// Runtime density penalty
	if node.RuntimeCount < 5 {
		score += 100
	} else if node.RuntimeCount < 10 {
		score += 50
	}

	// Node health bonus
	if node.Status == "online" {
		score += 50
	}

	// ── Locality score (0–200) ────────────────────────────────────────────────
	// Check if model weights are already cached on this node
	if req.ModelID != "" {
		var cacheHit int
		_ = s.db.GetContext(ctx, &cacheHit,
			`SELECT COUNT(*) FROM node_model_cache WHERE node_id=$1 AND model_id=$2`, node.ID, req.ModelID)
		if cacheHit > 0 {
			score += 150
		}
	}

	// ── Priority weight bonus (0–200) ─────────────────────────────────────────
	// Higher-priority projects get a scheduling boost to ensure they land on best nodes
	bonus := float64(req.EffectivePriority) / 1000.0 * 200.0
	score += bonus

	return score
}

// sortScoredNodes sorts candidates descending by score.
// Ties broken by: free VRAM DESC → fewer runtimes → lower node ID (deterministic).
func sortScoredNodes(nodes []ScoredNode) {
	for i := 1; i < len(nodes); i++ {
		for j := i; j > 0 && less(nodes[j], nodes[j-1]); j-- {
			nodes[j], nodes[j-1] = nodes[j-1], nodes[j]
		}
	}
}

func less(a, b ScoredNode) bool {
	if a.Score != b.Score {
		return a.Score > b.Score // higher score first
	}
	if a.Node.FreeVRAMMB != b.Node.FreeVRAMMB {
		return a.Node.FreeVRAMMB > b.Node.FreeVRAMMB
	}
	if a.Node.RuntimeCount != b.Node.RuntimeCount {
		return a.Node.RuntimeCount < b.Node.RuntimeCount
	}
	return a.Node.ID < b.Node.ID
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// buildDecision constructs a PlacementDecision from the winning candidate.
func (s *Scheduler) buildDecision(ctx context.Context, best ScoredNode, req PlacementRequest, alternatives []ScoredNode) *PlacementDecision {
	// Assign GPU devices if needed
	var gpuIndices []int
	for i, gpu := range best.Node.GPUDevices {
		if i >= req.RequiredGPUs {
			break
		}
		gpuIndices = append(gpuIndices, gpu.DeviceIndex)
	}

	// Build alternative summaries
	altSummaries := make([]CandidateNodeSummary, 0, len(alternatives))
	for _, alt := range alternatives {
		if len(altSummaries) >= 5 {
			break
		}
		altSummaries = append(altSummaries, CandidateNodeSummary{
			NodeID:     alt.Node.ID,
			Hostname:   alt.Node.Hostname,
			Score:      alt.Score,
			FreeVRAMMB: alt.Node.FreeVRAMMB,
			FreeRAMMB:  alt.Node.FreeRAMMB,
		})
	}

	trace := DecisionTrace{
		BaseWeight:        int(req.PriorityWeight),
		WaitingBonus:      req.EffectivePriority - int(req.PriorityWeight),
		EffectivePriority: req.EffectivePriority,
		Candidates:        altSummaries,
		Selected:          best.Node.ID,
		Reason: fmt.Sprintf("node=%s score=%.1f free_vram=%dMB runtimes=%d",
			best.Node.Hostname, best.Score, best.Node.FreeVRAMMB, best.Node.RuntimeCount),
	}

	return &PlacementDecision{
		NodeID:            best.Node.ID,
		NodeHostname:      best.Node.Hostname,
		GPUDeviceIndices:  gpuIndices,
		NUMANode:          -1,
		PriorityWeight:    int(req.PriorityWeight),
		EffectivePriority: req.EffectivePriority,
		NodeScore:         best.Score,
		Reason:            trace.Reason,
		Trace:             trace,
		DecidedAt:         time.Now(),
	}
}

// handleNoCapacity determines the fallback action when no node can fit the request.
func (s *Scheduler) handleNoCapacity(ctx context.Context, req PlacementRequest) (*PlacementDecision, error) {
	s.log.Warn("no capacity available",
		zap.String("model_id", req.ModelID),
		zap.Int("required_vram_mb", int(req.RequiredVRAMMB)),
		zap.Int("effective_priority", req.EffectivePriority),
	)
	// Queue for later retry — the processQueue loop will retry with backoff
	return nil, ErrInsufficientCapacity
}

// recordDecision persists a scheduler_decisions row for audit.
func (s *Scheduler) recordDecision(ctx context.Context, req PlacementRequest, dec *PlacementDecision) string {
	id := newUUID()
	_, _ = s.db.ExecContext(ctx, `
		INSERT INTO scheduler_decisions
		  (id, model_id, model_name, project_id, node_id,
		   decision_type, priority_weight, effective_priority, waiting_bonus,
		   reservation_bonus, resource_penalty, node_score, reason, outcome, decided_at)
		VALUES ($1,$2,$3,$4,$5,'placement',$6,$7,$8,$9,$10,$11,$12,'pending',NOW())`,
		id,
		nilIfEmpty(req.ModelID),
		req.ModelName,
		nilIfEmpty(req.ProjectID),
		dec.NodeID,
		dec.PriorityWeight,
		dec.EffectivePriority,
		dec.Trace.WaitingBonus,
		dec.Trace.ReservationBonus,
		0, // resource_penalty
		dec.NodeScore,
		dec.Reason,
	)
	return id
}

// backoff returns exponential retry delay: 30s * 2^attempts, capped at 30 min.
func (s *Scheduler) backoff(attempts int) time.Duration {
	base := 30 * time.Second
	delay := base
	for i := 0; i < attempts && i < 6; i++ {
		delay *= 2
	}
	maxDelay := 30 * time.Minute
	if delay > maxDelay {
		delay = maxDelay
	}
	return delay
}

// newUUID generates a new UUID string.
func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
