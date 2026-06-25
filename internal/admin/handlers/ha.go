// Package handlers — ha.go
//
// Admin API for High Availability replica management.
//
// Routes (registered in cmd/admin/main.go):
//
//	GET    /admin/v1/ha/status                      — cluster-wide HA status
//	GET    /admin/v1/ha/status/:model_id            — per-model replica status
//	PUT    /admin/v1/ha/models/:model_id/replicas   — set desired_replicas
//	GET    /admin/v1/ha/recovery-log                — recent recovery events
//	GET    /admin/v1/ha/recovery-log/:model_id      — model recovery history
package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/ha"
)

// HAHandler exposes HA management API endpoints.
type HAHandler struct {
	db *sqlx.DB
}

// NewHAHandler constructs an HAHandler.
func NewHAHandler(db *sqlx.DB) *HAHandler {
	return &HAHandler{db: db}
}

// ─── GET /admin/v1/ha/status ─────────────────────────────────────────────────

// GetClusterHAStatus returns the replica status for every enabled model.
func (h *HAHandler) GetClusterHAStatus(c *gin.Context) {
	var rows []ha.ReplicaStatus
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT model_id, model_name, desired_replicas, min_available,
		       placement_policy, auto_recover,
		       active_replicas, starting_replicas, idle_replicas,
		       lost_replicas, node_count, ha_status
		FROM runtime_replica_status
		ORDER BY ha_status DESC, model_name`); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if rows == nil {
		rows = []ha.ReplicaStatus{}
	}

	// Summary counts
	var degraded, unavailable, healthy int
	for _, r := range rows {
		switch r.HAStatus {
		case "healthy":
			healthy++
		case "degraded":
			degraded++
		case "unavailable":
			unavailable++
		}
	}

	// Reconciler last sweep
	var lastSweep time.Time
	var recoveriesTriggered int
	_ = h.db.QueryRowContext(c.Request.Context(),
		`SELECT last_sweep_at, recoveries_triggered FROM reconciler_state WHERE singleton=TRUE`,
	).Scan(&lastSweep, &recoveriesTriggered)

	c.JSON(http.StatusOK, gin.H{
		"models":                rows,
		"total":                 len(rows),
		"healthy":               healthy,
		"degraded":              degraded,
		"unavailable":           unavailable,
		"reconciler_last_sweep": lastSweep,
		"recoveries_triggered":  recoveriesTriggered,
	})
}

// ─── GET /admin/v1/ha/status/:model_id ───────────────────────────────────────

// GetModelHAStatus returns the per-model replica detail including spec and live status.
func (h *HAHandler) GetModelHAStatus(c *gin.Context) {
	modelID := c.Param("model_id")

	var status ha.ReplicaStatus
	err := h.db.GetContext(c.Request.Context(), &status, `
		SELECT model_id, model_name, desired_replicas, min_available,
		       placement_policy, auto_recover,
		       active_replicas, starting_replicas, idle_replicas,
		       lost_replicas, node_count, ha_status
		FROM runtime_replica_status
		WHERE model_id = $1`, modelID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "model not found or has no replica spec"})
		return
	}

	// Per-replica detail (node distribution)
	type replicaRow struct {
		RuntimeID    string    `db:"id"           json:"runtime_id"`
		NodeID       string    `db:"node_id"      json:"node_id"`
		NodeHostname string    `db:"hostname"     json:"node_hostname"`
		State        string    `db:"state"        json:"state"`
		BindHost     string    `db:"bind_host"    json:"bind_host"`
		BindPort     int       `db:"bind_port"    json:"bind_port"`
		UpdatedAt    time.Time `db:"updated_at"   json:"updated_at"`
	}
	var replicas []replicaRow
	_ = h.db.SelectContext(c.Request.Context(), &replicas, `
		SELECT ar.id, ar.node_id::text AS node_id, n.hostname,
		       ar.state, ar.bind_host, ar.bind_port, ar.updated_at
		FROM agent_runtimes ar
		JOIN nodes n ON n.id = ar.node_id
		WHERE ar.model_id = $1
		  AND ar.state NOT IN ('stopped','deleted','archived','unloaded')
		ORDER BY ar.state, ar.updated_at DESC`, modelID)
	if replicas == nil {
		replicas = []replicaRow{}
	}

	c.JSON(http.StatusOK, gin.H{
		"status":   status,
		"replicas": replicas,
	})
}

// ─── PUT /admin/v1/ha/models/:model_id/replicas ───────────────────────────────

// SetReplicaSpec creates or updates the replica spec for a model.
func (h *HAHandler) SetReplicaSpec(c *gin.Context) {
	modelID := c.Param("model_id")

	var input struct {
		DesiredReplicas *int    `json:"desired_replicas"`
		MinAvailable    *int    `json:"min_available"`
		PlacementPolicy *string `json:"placement_policy"` // spread|pack|anti_affinity
		AutoRecover     *bool   `json:"auto_recover"`
		RecoveryDelayS  *int    `json:"recovery_delay_s"`
		MaxSurge        *int    `json:"max_surge"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Validate
	if input.DesiredReplicas != nil && (*input.DesiredReplicas < 0 || *input.DesiredReplicas > 32) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "desired_replicas must be between 0 and 32"})
		return
	}
	if input.MinAvailable != nil && *input.MinAvailable < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "min_available must be ≥ 0"})
		return
	}
	if input.PlacementPolicy != nil {
		switch *input.PlacementPolicy {
		case "spread", "pack", "anti_affinity":
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "placement_policy must be: spread, pack, or anti_affinity"})
			return
		}
	}

	// Verify model exists
	var modelName string
	if err := h.db.GetContext(c.Request.Context(), &modelName,
		`SELECT name FROM models WHERE id=$1`, modelID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
		return
	}

	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO model_replica_specs
		  (model_id, desired_replicas, min_available, placement_policy,
		   auto_recover, recovery_delay_s, max_surge)
		VALUES ($1,
		    COALESCE($2, 1), COALESCE($3, 1), COALESCE($4, 'spread'),
		    COALESCE($5, TRUE), COALESCE($6, 30), COALESCE($7, 1))
		ON CONFLICT (model_id) DO UPDATE SET
		    desired_replicas  = COALESCE($2, model_replica_specs.desired_replicas),
		    min_available     = COALESCE($3, model_replica_specs.min_available),
		    placement_policy  = COALESCE($4, model_replica_specs.placement_policy),
		    auto_recover      = COALESCE($5, model_replica_specs.auto_recover),
		    recovery_delay_s  = COALESCE($6, model_replica_specs.recovery_delay_s),
		    max_surge         = COALESCE($7, model_replica_specs.max_surge),
		    updated_at        = NOW()`,
		modelID,
		input.DesiredReplicas, input.MinAvailable, input.PlacementPolicy,
		input.AutoRecover, input.RecoveryDelayS, input.MaxSurge,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":    "replica spec updated",
		"model_id":   modelID,
		"model_name": modelName,
	})
}

// ─── GET /admin/v1/ha/recovery-log ───────────────────────────────────────────

// GetRecoveryLog returns recent recovery events across all models.
func (h *HAHandler) GetRecoveryLog(c *gin.Context) {
	limit := 100
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	var rows []ha.RecoveryLogEntry
	_ = h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT id, model_id::text AS model_id, model_name,
		       lost_runtime_id::text AS lost_runtime_id,
		       lost_node_id::text AS lost_node_id,
		       new_runtime_id::text AS new_runtime_id,
		       new_node_id::text AS new_node_id,
		       replica_index, trigger, status, reason, created_at, completed_at
		FROM runtime_recovery_log
		ORDER BY created_at DESC
		LIMIT $1`, limit)
	if rows == nil {
		rows = []ha.RecoveryLogEntry{}
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": len(rows)})
}

// ─── GET /admin/v1/ha/recovery-log/:model_id ─────────────────────────────────

// GetModelRecoveryLog returns recovery history for a specific model.
func (h *HAHandler) GetModelRecoveryLog(c *gin.Context) {
	modelID := c.Param("model_id")
	limit := 50
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	var rows []ha.RecoveryLogEntry
	_ = h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT id, model_id::text AS model_id, model_name,
		       lost_runtime_id::text AS lost_runtime_id,
		       lost_node_id::text AS lost_node_id,
		       new_runtime_id::text AS new_runtime_id,
		       new_node_id::text AS new_node_id,
		       replica_index, trigger, status, reason, created_at, completed_at
		FROM runtime_recovery_log
		WHERE model_id = $1
		ORDER BY created_at DESC
		LIMIT $2`, modelID, limit)
	if rows == nil {
		rows = []ha.RecoveryLogEntry{}
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": len(rows), "model_id": modelID})
}
