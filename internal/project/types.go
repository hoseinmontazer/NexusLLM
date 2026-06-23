// Package project defines types and constants for the Project entity.
package project

import "time"

// Priority tiers — ordered CRITICAL > HIGH > NORMAL > LOW > BEST_EFFORT.
type Priority string

const (
	PriorityCritical   Priority = "CRITICAL"
	PriorityHigh       Priority = "HIGH"
	PriorityNormal     Priority = "NORMAL"
	PriorityLow        Priority = "LOW"
	PriorityBestEffort Priority = "BEST_EFFORT"
)

// Score returns the integer scheduling score for a priority tier.
// CRITICAL=100, HIGH=75, NORMAL=50, LOW=25, BEST_EFFORT=10.
func (p Priority) Score() int {
	switch p {
	case PriorityCritical:
		return 100
	case PriorityHigh:
		return 75
	case PriorityNormal:
		return 50
	case PriorityLow:
		return 25
	case PriorityBestEffort:
		return 10
	default:
		return 50
	}
}

// IsValid returns true if the priority value is one of the five allowed tiers.
func (p Priority) IsValid() bool {
	switch p {
	case PriorityCritical, PriorityHigh, PriorityNormal, PriorityLow, PriorityBestEffort:
		return true
	}
	return false
}

// CanPreempt returns true if this priority may preempt runtimes belonging to
// the target priority. Implements the preemption rule matrix:
//   CRITICAL  → may preempt LOW, BEST_EFFORT
//   HIGH      → may preempt BEST_EFFORT
//   NORMAL    → no preemption
//   LOW       → no preemption
//   BEST_EFFORT → no preemption
// General rule: requestor score > target score (strict greater-than).
func (p Priority) CanPreempt(target Priority) bool {
	return p.Score() > target.Score()
}

// AdmissionPolicy governs what happens when a deployment cannot be
// immediately scheduled.
type AdmissionPolicy string

const (
	// AdmissionQueue adds the request to the Deployment_Queue (default).
	AdmissionQueue AdmissionPolicy = "queue"
	// AdmissionPreemptThenQueue attempts preemption first; queues on failure.
	AdmissionPreemptThenQueue AdmissionPolicy = "preempt_then_queue"
	// AdmissionReject immediately rejects with HTTP 409.
	AdmissionReject AdmissionPolicy = "reject"
)

// ─────────────────────────────────────────────────────────────────────────────
// Domain objects
// ─────────────────────────────────────────────────────────────────────────────

// Project is the core domain entity.
type Project struct {
	ID             string    `db:"id"              json:"id"`
	OrganizationID string    `db:"organization_id" json:"organization_id"`
	TeamID         string    `db:"team_id"         json:"team_id"`
	Name           string    `db:"name"            json:"name"`
	Description    string    `db:"description"     json:"description"`
	Priority       Priority  `db:"priority"        json:"priority"`
	Status         string    `db:"status"          json:"status"`
	CreatedAt      time.Time `db:"created_at"      json:"created_at"`
	UpdatedAt      time.Time `db:"updated_at"      json:"updated_at"`
}

// Reservation holds guaranteed resource minimums for a project.
type Reservation struct {
	ProjectID         string    `db:"project_id"          json:"project_id"`
	ReservedVRAMMB    int64     `db:"reserved_vram_mb"    json:"reserved_vram_mb"`
	ReservedCPUCores  int       `db:"reserved_cpu_cores"  json:"reserved_cpu_cores"`
	ReservedMemoryMB  int64     `db:"reserved_memory_mb"  json:"reserved_memory_mb"`
	UpdatedAt         time.Time `db:"updated_at"          json:"updated_at"`
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
	ID                   string     `db:"id"                     json:"id"`
	NodeID               *string    `db:"node_id"                json:"node_id"`
	PreemptedRuntimeID   *string    `db:"preempted_runtime_id"   json:"preempted_runtime_id"`
	PreemptedProjectID   *string    `db:"preempted_project_id"   json:"preempted_project_id"`
	PreemptedPriority    *string    `db:"preempted_priority"     json:"preempted_priority"`
	RequestingRuntimeID  *string    `db:"requesting_runtime_id"  json:"requesting_runtime_id"`
	RequestingProjectID  *string    `db:"requesting_project_id"  json:"requesting_project_id"`
	RequestingPriority   *string    `db:"requesting_priority"    json:"requesting_priority"`
	Trigger              string     `db:"trigger"                json:"trigger"`
	PressureValue        *float64   `db:"pressure_value"         json:"pressure_value"`
	CreatedAt            time.Time  `db:"created_at"             json:"created_at"`
}

// DeploymentQueueEntry represents a queued deployment request.
type DeploymentQueueEntry struct {
	ID              string     `db:"id"               json:"id"`
	ProjectID       *string    `db:"project_id"       json:"project_id"`
	RuntimeConfig   []byte     `db:"runtime_config"   json:"runtime_config"`
	PriorityScore   int        `db:"priority_score"   json:"priority_score"`
	AdmissionPolicy string     `db:"admission_policy" json:"admission_policy"`
	Status          string     `db:"status"           json:"status"`
	Attempts        int        `db:"attempts"         json:"attempts"`
	EnqueuedAt      time.Time  `db:"enqueued_at"      json:"enqueued_at"`
	ExpiresAt       *time.Time `db:"expires_at"       json:"expires_at"`
	LastAttemptAt   *time.Time `db:"last_attempt_at"  json:"last_attempt_at"`
	ErrorMsg        string     `db:"error_msg"        json:"error_msg"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Triggers
// ─────────────────────────────────────────────────────────────────────────────

const (
	TriggerGPUUtilization  = "gpu_utilization"
	TriggerVRAMExhaustion  = "vram_exhaustion"
	TriggerMemExhaustion   = "memory_exhaustion"
	TriggerAdmission       = "admission"
)

// GPUPressureThresholdPct is the GPU utilisation percentage above which a node
// is classified as under resource pressure.
const GPUPressureThresholdPct = 95

// RAMPressureThresholdPct is the RAM usage percentage above which a node is
// classified as under RAM pressure. (100 - 5 = 95%)
const RAMPressureFreeThresholdPct = 5
