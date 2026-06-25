// Package project defines domain types for the Project entity.
//
// Priority model: continuous integer weight in [0, 1000].
// Higher weight = higher scheduling priority.
// Scheduling decisions use EffectivePriority, not the raw base weight.
//
// Preset labels (UI only — not enforced by scheduler):
//
//	1000  Emergency
//	950   Customer Production Chat
//	900   Revenue Critical Services
//	800   Core Internal Services
//	700   Core Internal (lower band)
//	500   Standard Business Workloads
//	300   Batch Processing
//	100   Development
//	50    Playground
//	0     Best Effort
package project

import "time"

// ─────────────────────────────────────────────────────────────────────────────
// Priority weight
// ─────────────────────────────────────────────────────────────────────────────

// PriorityWeight is the base scheduling weight for a project.
// Valid range: 0–1000. Higher is scheduled first.
type PriorityWeight int

const (
	WeightEmergency    PriorityWeight = 1000
	WeightProdCritical PriorityWeight = 950
	WeightRevenueCrit  PriorityWeight = 900
	WeightCoreHigh     PriorityWeight = 800
	WeightCoreInternal PriorityWeight = 700
	WeightStandard     PriorityWeight = 500
	WeightBatch        PriorityWeight = 300
	WeightDevelopment  PriorityWeight = 100
	WeightPlayground   PriorityWeight = 50
	WeightBestEffort   PriorityWeight = 0

	// MinWeight / MaxWeight are the enforced bounds.
	MinWeight PriorityWeight = 0
	MaxWeight PriorityWeight = 1000

	// DefaultWeight is used when no explicit weight is provided.
	DefaultWeight PriorityWeight = 500
)

// IsValid returns true if weight is within [0, 1000].
func (w PriorityWeight) IsValid() bool {
	return w >= MinWeight && w <= MaxWeight
}

// Label returns a human-readable label for UI display.
// The label is purely informational; the scheduler uses the numeric weight.
func (w PriorityWeight) Label() string {
	switch {
	case w >= 950:
		return "Emergency"
	case w >= 900:
		return "Production Critical"
	case w >= 800:
		return "Revenue Critical"
	case w >= 700:
		return "Core Internal"
	case w >= 500:
		return "Standard"
	case w >= 300:
		return "Batch"
	case w >= 100:
		return "Development"
	case w >= 50:
		return "Playground"
	default:
		return "Best Effort"
	}
}

// Color returns a Tailwind CSS color class for the weight band.
func (w PriorityWeight) Color() string {
	switch {
	case w >= 900:
		return "red"
	case w >= 700:
		return "orange"
	case w >= 500:
		return "blue"
	case w >= 300:
		return "gray"
	default:
		return "slate"
	}
}

// CanPreempt returns true if this weight can preempt a runtime running under
// targetWeight. Preemption requires strictly higher effective weight.
// The minimum gap required is PreemptionMinGap to avoid thrashing.
func (w PriorityWeight) CanPreempt(target PriorityWeight) bool {
	return int(w)-int(target) >= PreemptionMinGap
}

// PreemptionMinGap is the minimum priority weight difference needed for
// preemption to be allowed. Prevents thrashing between near-equal weights.
const PreemptionMinGap = 50

// ─────────────────────────────────────────────────────────────────────────────
// Effective priority (scheduler input)
// ─────────────────────────────────────────────────────────────────────────────

// EffectivePriority is the computed scheduling priority.
// It is computed per-queue-cycle from:
//
//	effective = base_weight + waiting_bonus + reservation_bonus + sla_bonus - resource_penalty
//
// Clamped to [0, 1000].
type EffectivePriority struct {
	BaseWeight       PriorityWeight
	WaitingBonus     int // +1 per 60 s in queue, capped at +200
	ReservationBonus int // +50 if project has active resource reservation
	SLABonus         int // future: +N for contractual SLA tier
	ResourcePenalty  int // -100 if project is consuming beyond max quota (stored positive)
	Effective        int // BaseWeight + bonuses - penalties, clamped [0,1000]
}

// Compute recalculates the Effective field from the component values.
func (ep *EffectivePriority) Compute() {
	v := int(ep.BaseWeight) + ep.WaitingBonus + ep.ReservationBonus + ep.SLABonus - ep.ResourcePenalty
	if v < 0 {
		v = 0
	}
	if v > 1000 {
		v = 1000
	}
	ep.Effective = v
}

// WaitingBonusFor computes the aging bonus for a given wait duration.
// +1 per 60 seconds, capped at +200.
func WaitingBonusFor(waitSecs float64) int {
	bonus := int(waitSecs / 60.0)
	if bonus > 200 {
		bonus = 200
	}
	if bonus < 0 {
		bonus = 0
	}
	return bonus
}

// ─────────────────────────────────────────────────────────────────────────────
// Admission policy
// ─────────────────────────────────────────────────────────────────────────────

// AdmissionPolicy governs what happens when a deployment cannot be immediately scheduled.
type AdmissionPolicy string

const (
	AdmissionQueue            AdmissionPolicy = "queue"
	AdmissionPreemptThenQueue AdmissionPolicy = "preempt_then_queue"
	AdmissionReject           AdmissionPolicy = "reject"
)

// ─────────────────────────────────────────────────────────────────────────────
// Domain objects
// ─────────────────────────────────────────────────────────────────────────────

// Project is the core domain entity.
type Project struct {
	ID             string         `db:"id"              json:"id"`
	OrganizationID string         `db:"organization_id" json:"organization_id"`
	TeamID         string         `db:"team_id"         json:"team_id"`
	Name           string         `db:"name"            json:"name"`
	Description    string         `db:"description"     json:"description"`
	PriorityWeight PriorityWeight `db:"priority_weight" json:"priority_weight"`
	Preemptible    bool           `db:"preemptible"     json:"preemptible"`
	MaxCPU         int            `db:"max_cpu"         json:"max_cpu"`
	MaxMemoryMB    int64          `db:"max_memory_mb"   json:"max_memory_mb"`
	MaxGPUVRAMMB   int64          `db:"max_gpu_vram_mb" json:"max_gpu_vram_mb"`
	Status         string         `db:"status"          json:"status"`
	CreatedAt      time.Time      `db:"created_at"      json:"created_at"`
	UpdatedAt      time.Time      `db:"updated_at"      json:"updated_at"`
}

// Reservation holds guaranteed resource minimums for a project.
type Reservation struct {
	ProjectID        string    `db:"project_id"          json:"project_id"`
	ReservedVRAMMB   int64     `db:"reserved_vram_mb"    json:"reserved_vram_mb"`
	ReservedCPUCores int       `db:"reserved_cpu_cores"  json:"reserved_cpu_cores"`
	ReservedMemoryMB int64     `db:"reserved_memory_mb"  json:"reserved_memory_mb"`
	MaxGPUVRAMMB     int64     `db:"max_gpu_vram_mb"     json:"max_gpu_vram_mb"`
	MaxCPU           int       `db:"max_cpu"             json:"max_cpu"`
	MaxMemoryMB      int64     `db:"max_memory_mb"       json:"max_memory_mb"`
	UpdatedAt        time.Time `db:"updated_at"          json:"updated_at"`
}

// Configuration holds runtime protection settings for a project.
type Configuration struct {
	ProjectID       string          `db:"project_id"       json:"project_id"`
	AlwaysRunning   bool            `db:"always_running"   json:"always_running"`
	Protected       bool            `db:"protected"        json:"protected"`
	MinimumReplicas int             `db:"minimum_replicas" json:"minimum_replicas"`
	AdmissionPolicy AdmissionPolicy `db:"admission_policy" json:"admission_policy"`
	UpdatedAt       time.Time       `db:"updated_at"       json:"updated_at"`
}

// PreemptionEvent records a preemption evaluation or execution.
type PreemptionEvent struct {
	ID                  string    `db:"id"                     json:"id"`
	NodeID              *string   `db:"node_id"                json:"node_id"`
	PreemptedRuntimeID  *string   `db:"preempted_runtime_id"   json:"preempted_runtime_id"`
	PreemptedProjectID  *string   `db:"preempted_project_id"   json:"preempted_project_id"`
	PreemptedWeight     *int      `db:"preempted_weight"       json:"preempted_weight"`
	RequestingRuntimeID *string   `db:"requesting_runtime_id"  json:"requesting_runtime_id"`
	RequestingProjectID *string   `db:"requesting_project_id"  json:"requesting_project_id"`
	RequestingWeight    *int      `db:"requesting_weight"      json:"requesting_weight"`
	Trigger             string    `db:"trigger"                json:"trigger"`
	PressureValue       *float64  `db:"pressure_value"         json:"pressure_value"`
	DecisionTrace       []byte    `db:"decision_trace"         json:"-"`
	CreatedAt           time.Time `db:"created_at"             json:"created_at"`
}

// DeploymentQueueEntry represents a queued deployment request.
type DeploymentQueueEntry struct {
	ID                string     `db:"id"                  json:"id"`
	ProjectID         *string    `db:"project_id"          json:"project_id"`
	ModelName         string     `db:"model_name"          json:"model_name"`
	RuntimeConfig     []byte     `db:"runtime_config"      json:"runtime_config"`
	PriorityWeight    int        `db:"priority_weight"     json:"priority_weight"`
	EffectivePriority int        `db:"effective_priority"  json:"effective_priority"`
	AdmissionPolicy   string     `db:"admission_policy"    json:"admission_policy"`
	Status            string     `db:"status"              json:"status"`
	Attempts          int        `db:"attempts"            json:"attempts"`
	WaitingSince      time.Time  `db:"waiting_since"       json:"waiting_since"`
	EnqueuedAt        time.Time  `db:"enqueued_at"         json:"enqueued_at"`
	ExpiresAt         *time.Time `db:"expires_at"          json:"expires_at"`
	LastAttemptAt     *time.Time `db:"last_attempt_at"     json:"last_attempt_at"`
	ErrorMsg          string     `db:"error_msg"           json:"error_msg"`
	PreemptionReason  string     `db:"preemption_reason"   json:"preemption_reason"`
	RequiredVRAMMB    int64      `db:"required_vram_mb"    json:"required_vram_mb"`
	RequiredRAMMB     int64      `db:"required_ram_mb"     json:"required_ram_mb"`
	RequiredCPU       int        `db:"required_cpu"        json:"required_cpu"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Trigger constants
// ─────────────────────────────────────────────────────────────────────────────

const (
	TriggerGPUUtilization = "gpu_utilization"
	TriggerVRAMExhaustion = "vram_exhaustion"
	TriggerMemExhaustion  = "memory_exhaustion"
	TriggerAdmission      = "admission"
)

// ─────────────────────────────────────────────────────────────────────────────
// Resource pressure thresholds
// ─────────────────────────────────────────────────────────────────────────────

const (
	GPUPressureThresholdPct     = 95
	RAMPressureFreeThresholdPct = 5
)

// ─────────────────────────────────────────────────────────────────────────────
// Preset table (for UI)
// ─────────────────────────────────────────────────────────────────────────────

// PriorityPreset is a named preset shown in the UI priority picker.
type PriorityPreset struct {
	Weight int    `json:"weight"`
	Label  string `json:"label"`
	Color  string `json:"color"`
}

// StandardPresets is the canonical list returned by GET /admin/v1/scheduler/priority-presets.
var StandardPresets = []PriorityPreset{
	{1000, "Emergency", "red"},
	{950, "Customer Production Chat", "red"},
	{900, "Revenue Critical Services", "red"},
	{800, "Core Internal Services", "orange"},
	{700, "Core Internal (lower)", "orange"},
	{500, "Standard Business Workloads", "blue"},
	{300, "Batch Processing", "gray"},
	{100, "Development", "gray"},
	{50, "Playground", "slate"},
	{0, "Best Effort", "slate"},
}
