// Package scheduler — types.go
// All scheduling decisions use numeric priority_weight [0–1000], never enum strings.
package scheduler

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/nexusllm/nexusllm/internal/project"
)

// ─────────────────────────────────────────────────────────────────────────────
// Request & Decision types
// ─────────────────────────────────────────────────────────────────────────────

// PlacementMode is the high-level deployment targeting mode.
type PlacementMode string

const (
	// ModeAuto — scheduler picks the best available node (default).
	ModeAuto PlacementMode = "auto"
	// ModeSpecificNode — must deploy to a single named node_id.
	// Validated: request fails if node lacks resources.
	ModeSpecificNode PlacementMode = "specific_node"
	// ModeNodeGroup — deploy to any node that belongs to a named group
	// (nodes share the label "node_group=<group_id>").
	ModeNodeGroup PlacementMode = "node_group"
	// ModeLabelSelector — deploy to any node whose labels match all
	// key=value pairs in NodeSelector (AND semantics).
	ModeLabelSelector PlacementMode = "label_selector"
)

// PlacementValidationError carries a human-readable rejection reason
// for specific_node or label_selector placements that fail resource checks.
type PlacementValidationError struct {
	Reason    string
	Requested map[string]int64 // resource name → requested amount
	Available map[string]int64 // resource name → available on the target
}

func (e *PlacementValidationError) Error() string { return e.Reason }

// PlacementStrategy controls how the scheduler picks a node.
type PlacementStrategy string

const (
	// StrategyAuto — scheduler picks the best node (default).
	StrategyAuto PlacementStrategy = "auto"
	// StrategyPinned — must use the specified node_id (legacy alias for ModeSpecificNode).
	StrategyPinned PlacementStrategy = "pinned"
	// StrategySpread — prefer nodes that don't already have a replica of this model.
	StrategySpread PlacementStrategy = "spread"
	// StrategyPacked — prefer nodes that already carry replicas (minimise fragmentation).
	StrategyPacked PlacementStrategy = "packed"
)

// AcceleratorType describes the required compute hardware.
type AcceleratorType string

const (
	AcceleratorAny AcceleratorType = "any" // default — use GPU if available
	AcceleratorGPU AcceleratorType = "gpu" // must have at least one GPU
	AcceleratorCPU AcceleratorType = "cpu" // must be CPU-only
)

// PlacementRequest describes a model that needs to be placed on a node.
type PlacementRequest struct {
	// Identity
	ModelID   string
	ModelName string
	ProjectID string

	// Requirements
	RequiredCPU    int
	RequiredRAMMB  int64
	RequiredVRAMMB int64
	RequiredGPUs   int
	ExecutionMode  string // cpu | gpu | auto

	// ── Placement targeting ───────────────────────────────────────────────────
	// Mode drives which targeting path the scheduler uses.
	Mode PlacementMode // auto | specific_node | node_group | label_selector

	// SpecificNodeID is the hard-pinned node (Mode == ModeSpecificNode).
	// The scheduler validates resource availability and returns
	// PlacementValidationError if the node cannot accommodate the request.
	SpecificNodeID string

	// NodeGroupID targets any node with label "node_group=<id>" (Mode == ModeNodeGroup).
	NodeGroupID string

	// NodeSelector is a map of label key → value. ALL pairs must match
	// (Mode == ModeLabelSelector). Example: {"accelerator":"h200","storage":"nvme"}
	NodeSelector map[string]string

	// Strategy controls intra-mode tie-breaking (spread vs packed vs auto).
	Strategy       PlacementStrategy // auto | pinned | spread | packed
	AcceleratorReq AcceleratorType   // any | gpu | cpu
	PinnedNodeID   string            // legacy: used when Strategy == StrategyPinned

	// ─────────────────────────────────────────────────────────────────────────
	// Priority (numeric weight + computed effective value)
	PriorityWeight    project.PriorityWeight
	EffectivePriority int // computed by scheduler: weight + aging + bonuses - penalties

	// Policy
	WorkloadPolicy string // lazy_load | always_on
	Preemptible    bool   // may this project be preempted to serve others?

	// Soft hints (used when Mode == auto)
	PreferNodeID  string
	RequireNodeID string
}

// PlacementDecision is the scheduler's decision on where to place a model.
type PlacementDecision struct {
	DecisionID   string
	NodeID       string
	NodeHostname string

	// Assigned resources
	GPUDeviceIndices []int
	CPUSetCPUs       string
	NUMANode         int
	RAMMBLimit       int64

	// Priority context at decision time
	PriorityWeight    int
	EffectivePriority int

	// Scoring metadata
	NodeScore float64
	Reason    string
	Trace     DecisionTrace
	DecidedAt time.Time
}

// DecisionTrace records the full reasoning for audit/UI display.
type DecisionTrace struct {
	BaseWeight        int                    `json:"base_weight"`
	WaitingBonus      int                    `json:"waiting_bonus"`
	ReservationBonus  int                    `json:"reservation_bonus"`
	ResourcePenalty   int                    `json:"resource_penalty"`
	EffectivePriority int                    `json:"effective_priority"`
	Candidates        []CandidateNodeSummary `json:"candidates"`
	Selected          string                 `json:"selected_node_id"`
	Reason            string                 `json:"reason"`
}

// CandidateNodeSummary is one node's summary in a DecisionTrace.
type CandidateNodeSummary struct {
	NodeID       string  `json:"node_id"`
	Hostname     string  `json:"hostname"`
	Score        float64 `json:"score"`
	FreeVRAMMB   int64   `json:"free_vram_mb"`
	FreeRAMMB    int64   `json:"free_ram_mb"`
	GPUUtilPct   float64 `json:"gpu_util_pct"`
	RuntimeCount int     `json:"runtime_count"`
	Rejected     bool    `json:"rejected"`
	RejectReason string  `json:"reject_reason,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Node types
// ─────────────────────────────────────────────────────────────────────────────

// Node is a cluster node with current resource state.
type Node struct {
	ID       string
	Hostname string
	Status   string

	TotalCPU    int
	TotalRAMMB  int64
	TotalVRAMMB int64

	FreeCPU    int
	FreeRAMMB  int64
	FreeVRAMMB int64

	CPUUtilPct   float64
	RAMUsedMB    int64
	RuntimeCount int
	GPUDevices   []GPUDevice
	HasGPU       bool
	GPUCount     int
}

// GPUDevice is a single GPU on a node.
type GPUDevice struct {
	ID             string
	DeviceIndex    int
	Name           string
	VRAMMB         int64
	MemUsedMB      int64
	UtilizationPct int
	TemperatureC   int
	NUMANode       int
	Status         string
}

// ScoredNode is a node paired with its computed placement score.
type ScoredNode struct {
	Node  Node
	Score float64
}

// ─────────────────────────────────────────────────────────────────────────────
// Queue types
// ─────────────────────────────────────────────────────────────────────────────

// QueuedDeployment is a pending deployment in deployment_queue.
type QueuedDeployment struct {
	ID                string     `db:"id"`
	ProjectID         string     `db:"project_id"`
	ModelName         string     `db:"model_name"`
	RuntimeConfig     string     `db:"runtime_config"` // JSON
	PriorityWeight    int        `db:"priority_weight"`
	EffectivePriority int        `db:"effective_priority"`
	RequiredVRAMMB    int64      `db:"required_vram_mb"`
	RequiredRAMMB     int64      `db:"required_ram_mb"`
	RequiredCPU       int        `db:"required_cpu"`
	ExecutionMode     string     `db:"execution_mode"`
	PreferNodeID      *string    `db:"prefer_node_id"`
	Attempts          int        `db:"attempts"`
	WaitingSince      time.Time  `db:"waiting_since"`
	EnqueuedAt        time.Time  `db:"enqueued_at"`
	LastAttemptAt     *time.Time `db:"last_attempt_at"`
	ErrorMsg          string     `db:"error_msg"`
}

// ToPlacementRequest converts a queue item into a PlacementRequest.
func (q *QueuedDeployment) ToPlacementRequest() (PlacementRequest, error) {
	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(q.RuntimeConfig), &cfg); err != nil {
		return PlacementRequest{}, err
	}
	modelID, _ := cfg["model_id"].(string)
	req := PlacementRequest{
		ModelID:           modelID,
		ModelName:         q.ModelName,
		ProjectID:         q.ProjectID,
		RequiredCPU:       q.RequiredCPU,
		RequiredRAMMB:     q.RequiredRAMMB,
		RequiredVRAMMB:    q.RequiredVRAMMB,
		ExecutionMode:     q.ExecutionMode,
		PriorityWeight:    project.PriorityWeight(q.PriorityWeight),
		EffectivePriority: q.EffectivePriority,
	}
	if q.PreferNodeID != nil {
		req.PreferNodeID = *q.PreferNodeID
	}
	return req, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Errors
// ─────────────────────────────────────────────────────────────────────────────

var (
	ErrInsufficientCapacity = errors.New("insufficient capacity on all nodes")
	ErrNoOnlineNodes        = errors.New("no online nodes available")
	ErrPreemptionFailed     = errors.New("preemption failed")
	ErrPreemptionNotAllowed = errors.New("preemption not allowed: insufficient priority gap")
)
