// Package placement implements the resource-aware placement engine.
// It chooses which node, GPUs, CPU cores, and NUMA node to assign to a
// service when it is deployed. The engine is intentionally node-topology-aware
// so it can make good placement decisions on today's single H200 server and
// remain correct when additional nodes are added later.
package placement

import "time"

// ─────────────────────────────────────────────────────────────────────────────
// Service & runtime classification
// ─────────────────────────────────────────────────────────────────────────────

// ServiceType is the primary service classification used for routing.
type ServiceType string

const (
	ServiceChat      ServiceType = "CHAT"
	ServiceEmbedding ServiceType = "EMBEDDING"
	ServiceRerank    ServiceType = "RERANK"
	ServiceSTT       ServiceType = "STT"
	ServiceTTS       ServiceType = "TTS"
	ServiceOCR       ServiceType = "OCR"
	ServiceAgent     ServiceType = "AGENT"
	ServiceMCP       ServiceType = "MCP"
)

// RuntimeType distinguishes GPU-accelerated from CPU-native workloads.
type RuntimeType string

const (
	RuntimeGPU RuntimeType = "GPU_RUNTIME"
	RuntimeCPU RuntimeType = "CPU_RUNTIME"
)

// Priority controls placement preference relative to other services.
type Priority string

const (
	PriorityCritical   Priority = "critical"
	PriorityHigh       Priority = "high"
	PriorityNormal     Priority = "normal"
	PriorityLow        Priority = "low"
	PriorityBestEffort Priority = "best_effort"
)

// priorityScore maps a Priority to an integer for comparisons.
var priorityScore = map[Priority]int{
	PriorityCritical:   100,
	PriorityHigh:       75,
	PriorityNormal:     50,
	PriorityLow:        25,
	PriorityBestEffort: 10,
}

// Score returns the numeric priority score.
func (p Priority) Score() int {
	if v, ok := priorityScore[p]; ok {
		return v
	}
	return 50
}

// ─────────────────────────────────────────────────────────────────────────────
// Placement request / result
// ─────────────────────────────────────────────────────────────────────────────

// Request describes everything the placement engine needs to choose resources.
type Request struct {
	// Service identity
	ModelID     string
	ModelName   string
	ServiceType ServiceType
	RuntimeType RuntimeType
	Priority    Priority

	// GPU requirements (only relevant for GPU_RUNTIME)
	MinVRAMMB    int64 // minimum VRAM per GPU in MB
	MaxVRAMMB    int64 // maximum VRAM to allocate (0 = no cap)
	GPUCount     int   // number of GPUs needed (tensor parallel size)

	// CPU requirements (relevant for CPU_RUNTIME, optional hint for GPU_RUNTIME)
	CPUCores    int // 0 = no affinity
	NUMANode    int // -1 = no preference
	RAMMBLimit  int64 // 0 = no limit

	// Node selection hints
	PreferNodeID   string // prefer a specific node (empty = any)
	RequireNodeID  string // must use this node (empty = any)
}

// Decision is the placement engine's output.
type Decision struct {
	NodeID     string
	NodeHost   string

	// GPU assignment (empty for CPU_RUNTIME)
	GPUDeviceIndices []int
	GPUDeviceIDs     []string
	TotalVRAMMB      int64

	// CPU assignment
	CPUCores int
	NUMANode int // -1 if no specific NUMA node assigned
	RAMMBLimit int64

	// Scoring
	Strategy string
	Score    float64
	Reason   string

	// Metadata
	DecidedAt time.Time
}
