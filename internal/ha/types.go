// Package ha implements High Availability and self-healing for NexusLLM runtimes.
//
// The core abstraction is the Reconciler — a background loop that continuously
// compares desired state (model_replica_specs) against actual state
// (agent_runtimes) and automatically triggers recovery actions.
//
// Architecture:
//
//	NodeHealth Monitor
//	  ↓ node offline → runtimes marked LOST
//
//	Reconciler (runs every 30s)
//	  → queries runtime_replica_status view
//	  → for each model with lost/insufficient replicas:
//	      → selects a healthy node (respecting placement policy)
//	      → enqueues START_MODEL task
//	      → writes runtime_recovery_log entry
//
//	Gateway Registry
//	  → only routes to ACTIVE/WARM/READY/IDLE endpoints
//	  → LOST/STOPPING endpoints are removed from pool
package ha

import "time"

// PlacementPolicy controls how replicas are distributed across nodes.
type PlacementPolicy string

const (
	// PolicySpread — prefer different nodes (default, best for HA).
	PolicySpread PlacementPolicy = "spread"
	// PolicyPack — prefer same node (minimise resource fragmentation).
	PolicyPack PlacementPolicy = "pack"
	// PolicyAntiAffinity — hard rule: never two replicas on the same node.
	PolicyAntiAffinity PlacementPolicy = "anti_affinity"
)

// ReplicaSpec is the desired state for a model's replica set.
type ReplicaSpec struct {
	ID              string          `db:"id"               json:"id"`
	ModelID         string          `db:"model_id"         json:"model_id"`
	DesiredReplicas int             `db:"desired_replicas" json:"desired_replicas"`
	MinAvailable    int             `db:"min_available"    json:"min_available"`
	PlacementPolicy PlacementPolicy `db:"placement_policy" json:"placement_policy"`
	AutoRecover     bool            `db:"auto_recover"     json:"auto_recover"`
	RecoveryDelayS  int             `db:"recovery_delay_s" json:"recovery_delay_s"`
	MaxSurge        int             `db:"max_surge"        json:"max_surge"`
	CreatedAt       time.Time       `db:"created_at"       json:"created_at"`
	UpdatedAt       time.Time       `db:"updated_at"       json:"updated_at"`
}

// ReplicaStatus is the live view of a model's replica health.
// Read from the runtime_replica_status view.
type ReplicaStatus struct {
	ModelID          string `db:"model_id"          json:"model_id"`
	ModelName        string `db:"model_name"         json:"model_name"`
	DesiredReplicas  int    `db:"desired_replicas"   json:"desired_replicas"`
	MinAvailable     int    `db:"min_available"      json:"min_available"`
	PlacementPolicy  string `db:"placement_policy"   json:"placement_policy"`
	AutoRecover      bool   `db:"auto_recover"       json:"auto_recover"`
	ActiveReplicas   int    `db:"active_replicas"    json:"active_replicas"`
	StartingReplicas int    `db:"starting_replicas"  json:"starting_replicas"`
	IdleReplicas     int    `db:"idle_replicas"      json:"idle_replicas"`
	LostReplicas     int    `db:"lost_replicas"      json:"lost_replicas"`
	NodeCount        int    `db:"node_count"         json:"node_count"`
	HAStatus         string `db:"ha_status"          json:"ha_status"` // healthy|degraded|unavailable
}

// RecoveryLogEntry records a single recovery action.
type RecoveryLogEntry struct {
	ID            string     `db:"id"               json:"id"`
	ModelID       string     `db:"model_id"         json:"model_id"`
	ModelName     string     `db:"model_name"       json:"model_name"`
	LostRuntimeID *string    `db:"lost_runtime_id"  json:"lost_runtime_id"`
	LostNodeID    *string    `db:"lost_node_id"     json:"lost_node_id"`
	NewRuntimeID  *string    `db:"new_runtime_id"   json:"new_runtime_id"`
	NewNodeID     *string    `db:"new_node_id"      json:"new_node_id"`
	ReplicaIndex  *int       `db:"replica_index"    json:"replica_index"`
	Trigger       string     `db:"trigger"          json:"trigger"`
	Status        string     `db:"status"           json:"status"`
	Reason        string     `db:"reason"           json:"reason"`
	CreatedAt     time.Time  `db:"created_at"       json:"created_at"`
	CompletedAt   *time.Time `db:"completed_at"     json:"completed_at"`
}

// ReconcileAction describes what the reconciler wants to do.
type ReconcileAction struct {
	ModelID    string
	ModelName  string
	Action     string // "start_replica" | "skip" | "scale_down"
	TargetNode string // node to deploy to
	ReplicaIdx int
	Reason     string
}
