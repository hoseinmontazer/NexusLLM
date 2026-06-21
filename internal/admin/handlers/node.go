package handlers

// node.go — Admin API handlers for cluster node management.
//
// Routes (to be registered in cmd/admin/main.go):
//   POST   /admin/v1/nodes                  — register a new node
//   GET    /admin/v1/nodes                  — list all nodes
//   GET    /admin/v1/nodes/:id              — get node details + live telemetry
//   PUT    /admin/v1/nodes/:id/labels       — update node labels
//   POST   /admin/v1/nodes/:id/heartbeat    — receive node agent heartbeat
//   GET    /admin/v1/nodes/:id/telemetry    — last N telemetry snapshots
//   GET    /admin/v1/nodes/:id/inventory    — latest inventory snapshot
//   POST   /admin/v1/nodes/:id/inventory    — receive inventory push from node agent

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// NodeHandler manages cluster node admin operations.
type NodeHandler struct {
	db *sqlx.DB
}

// NewNodeHandler constructs a NodeHandler.
func NewNodeHandler(db *sqlx.DB) *NodeHandler {
	return &NodeHandler{db: db}
}

// ─── Node CRUD ────────────────────────────────────────────────────────────────

// RegisterNode handles POST /admin/v1/nodes
func (h *NodeHandler) RegisterNode(c *gin.Context) {
	var input struct {
		Hostname    string                 `json:"hostname"      binding:"required"`
		DisplayName string                 `json:"display_name"`
		TotalCPU    int                    `json:"total_cpu"`
		TotalRAMMB  int64                  `json:"total_ram_mb"`
		TotalVRAMMB int64                  `json:"total_vram_mb"`
		Labels      map[string]interface{} `json:"labels"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.DisplayName == "" {
		input.DisplayName = input.Hostname
	}

	labelsJSON := "{}"
	if len(input.Labels) > 0 {
		if b, err := marshalLabels(input.Labels); err == nil {
			labelsJSON = string(b)
		}
	}

	nodeID := uuid.New().String()
	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO nodes
		  (id, hostname, display_name, total_cpu, total_ram_mb, total_vram_mb,
		   status, labels, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,'unknown',$7,NOW(),NOW())`,
		nodeID, input.Hostname, input.DisplayName,
		input.TotalCPU, input.TotalRAMMB, input.TotalVRAMMB,
		labelsJSON,
	)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "node hostname already registered: " + err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"id":           nodeID,
		"hostname":     input.Hostname,
		"display_name": input.DisplayName,
		"status":       "unknown",
	})
}

// ListNodes handles GET /admin/v1/nodes
func (h *NodeHandler) ListNodes(c *gin.Context) {
	type nodeRow struct {
		ID               string     `db:"id"               json:"id"`
		Hostname         string     `db:"hostname"         json:"hostname"`
		DisplayName      string     `db:"display_name"     json:"display_name"`
		TotalCPU         int        `db:"total_cpu"        json:"total_cpu"`
		TotalRAMMB       int64      `db:"total_ram_mb"     json:"total_ram_mb"`
		TotalVRAMMB      int64      `db:"total_vram_mb"    json:"total_vram_mb"`
		Status           string     `db:"status"           json:"status"`
		AgentVersion     string     `db:"agent_version"    json:"agent_version"`
		LastHeartbeatAt  *time.Time `db:"last_heartbeat_at" json:"last_heartbeat_at"`
		Labels           []byte     `db:"labels"           json:"-"`
		LabelsStr        string     `json:"labels"`
		CreatedAt        time.Time  `db:"created_at"       json:"created_at"`
	}
	var rows []nodeRow
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT id, hostname, display_name, total_cpu, total_ram_mb, total_vram_mb,
		       status, COALESCE(agent_version,'') AS agent_version,
		       last_heartbeat_at, labels, created_at
		FROM nodes ORDER BY hostname`); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	for i := range rows {
		if rows[i].Labels != nil {
			rows[i].LabelsStr = string(rows[i].Labels)
		} else {
			rows[i].LabelsStr = "{}"
		}
	}
	if rows == nil {
		rows = []nodeRow{}
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": len(rows)})
}

// GetNode handles GET /admin/v1/nodes/:id
func (h *NodeHandler) GetNode(c *gin.Context) {
	nodeID := c.Param("id")

	type nodeDetail struct {
		ID              string     `db:"id"               json:"id"`
		Hostname        string     `db:"hostname"         json:"hostname"`
		DisplayName     string     `db:"display_name"     json:"display_name"`
		TotalCPU        int        `db:"total_cpu"        json:"total_cpu"`
		TotalRAMMB      int64      `db:"total_ram_mb"     json:"total_ram_mb"`
		TotalVRAMMB     int64      `db:"total_vram_mb"    json:"total_vram_mb"`
		Status          string     `db:"status"           json:"status"`
		AgentVersion    string     `db:"agent_version"    json:"agent_version"`
		LastHeartbeatAt *time.Time `db:"last_heartbeat_at" json:"last_heartbeat_at"`
		Labels          []byte     `db:"labels"           json:"-"`
		LabelsStr       string     `json:"labels"`
	}

	var node nodeDetail
	if err := h.db.GetContext(c.Request.Context(), &node, `
		SELECT id, hostname, display_name, total_cpu, total_ram_mb, total_vram_mb,
		       status, COALESCE(agent_version,'') AS agent_version,
		       last_heartbeat_at, labels
		FROM nodes WHERE id = $1`, nodeID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "node not found"})
		return
	}
	if node.Labels != nil {
		node.LabelsStr = string(node.Labels)
	} else {
		node.LabelsStr = "{}"
	}

	// Latest telemetry
	type telRow struct {
		CPUUtilPct float64 `db:"cpu_util_pct" json:"cpu_util_pct"`
		RAMUsedMB  int64   `db:"ram_used_mb"  json:"ram_used_mb"`
		RAMAvailMB int64   `db:"ram_avail_mb" json:"ram_avail_mb"`
		RecordedAt time.Time `db:"recorded_at" json:"recorded_at"`
	}
	var tel *telRow
	var telData telRow
	if err := h.db.GetContext(c.Request.Context(), &telData, `
		SELECT cpu_util_pct, ram_used_mb, ram_avail_mb, recorded_at
		FROM node_telemetry WHERE node_id = $1
		ORDER BY recorded_at DESC LIMIT 1`, nodeID); err == nil {
		tel = &telData
	}

	c.JSON(http.StatusOK, gin.H{
		"node":      node,
		"telemetry": tel,
	})
}

// UpdateLabels handles PUT /admin/v1/nodes/:id/labels
func (h *NodeHandler) UpdateLabels(c *gin.Context) {
	nodeID := c.Param("id")
	var input struct {
		Labels map[string]interface{} `json:"labels" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	labelsJSON := "{}"
	if b, err := marshalLabels(input.Labels); err == nil {
		labelsJSON = string(b)
	}
	res, err := h.db.ExecContext(c.Request.Context(),
		`UPDATE nodes SET labels = $1, updated_at = NOW() WHERE id = $2`,
		labelsJSON, nodeID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "node not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "labels updated", "node_id": nodeID})
}

// ─── Agent API ────────────────────────────────────────────────────────────────

// Heartbeat handles POST /admin/v1/nodes/:id/heartbeat
// Called by the node agent to signal liveness.
func (h *NodeHandler) Heartbeat(c *gin.Context) {
	nodeID := c.Param("id")
	var input struct {
		AgentVersion string `json:"agent_version"`
		Status       string `json:"status"`
	}
	_ = c.ShouldBindJSON(&input)
	if input.Status == "" {
		input.Status = "online"
	}

	res, err := h.db.ExecContext(c.Request.Context(), `
		UPDATE nodes
		SET status = $1, last_heartbeat_at = NOW(),
		    agent_version = COALESCE(NULLIF($2,''), agent_version),
		    updated_at = NOW()
		WHERE id = $3`,
		input.Status, input.AgentVersion, nodeID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "node not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"acknowledged": true,
		"node_id":      nodeID,
		"server_time":  time.Now().UTC(),
	})
}

// PushInventory handles POST /admin/v1/nodes/:id/inventory
// Called by the node agent to push a full hardware inventory snapshot.
func (h *NodeHandler) PushInventory(c *gin.Context) {
	nodeID := c.Param("id")
	var snapshot map[string]interface{}
	if err := c.ShouldBindJSON(&snapshot); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	snapJSON := "{}"
	if b, err := marshalLabels(snapshot); err == nil {
		snapJSON = string(b)
	}

	agentVer := ""
	if v, ok := snapshot["agent_version"].(string); ok {
		agentVer = v
	}
	cpuCores := 0
	if v, ok := snapshot["cpu_cores"].(float64); ok {
		cpuCores = int(v)
	}
	var ramMB int64
	if v, ok := snapshot["ram_total_mb"].(float64); ok {
		ramMB = int64(v)
	}

	_, _ = h.db.ExecContext(c.Request.Context(), `
		UPDATE nodes
		SET total_cpu = CASE WHEN $1 > 0 THEN $1 ELSE total_cpu END,
		    total_ram_mb = CASE WHEN $2 > 0 THEN $2 ELSE total_ram_mb END,
		    agent_version = CASE WHEN $3 != '' THEN $3 ELSE agent_version END,
		    status = 'online', last_heartbeat_at = NOW(), updated_at = NOW()
		WHERE id = $4`,
		cpuCores, ramMB, agentVer, nodeID,
	)

	_, _ = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO node_inventory_snapshots
		  (id, node_id, snapshot, agent_ver, reported_at)
		VALUES (gen_random_uuid(), $1, $2, $3, NOW())`,
		nodeID, snapJSON, agentVer,
	)

	c.JSON(http.StatusOK, gin.H{
		"acknowledged": true,
		"node_id":      nodeID,
	})
}

// PushTelemetry handles POST /admin/v1/nodes/:id/telemetry
// Called by the node agent to push a hardware telemetry snapshot.
func (h *NodeHandler) PushTelemetry(c *gin.Context) {
	nodeID := c.Param("id")
	var input struct {
		CPUCoresTotal int     `json:"cpu_cores_total"`
		CPUUtilPct    float64 `json:"cpu_util_pct"`
		RAMTotalMB    int64   `json:"ram_total_mb"`
		RAMUsedMB     int64   `json:"ram_used_mb"`
		RAMAvailMB    int64   `json:"ram_avail_mb"`
		NUMANodes     int     `json:"numa_nodes"`
		DiskTotalGB   int64   `json:"disk_total_gb"`
		DiskUsedGB    int64   `json:"disk_used_gb"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO node_telemetry
		  (node_id, cpu_cores_total, cpu_util_pct,
		   ram_total_mb, ram_used_mb, ram_avail_mb,
		   numa_nodes, disk_total_gb, disk_used_gb, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NOW())`,
		nodeID,
		input.CPUCoresTotal, input.CPUUtilPct,
		input.RAMTotalMB, input.RAMUsedMB, input.RAMAvailMB,
		input.NUMANodes, input.DiskTotalGB, input.DiskUsedGB,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Update the node's total_ram_mb if the agent reports a different value
	// (e.g. first time or hardware change)
	if input.RAMTotalMB > 0 {
		_, _ = h.db.ExecContext(c.Request.Context(), `
			UPDATE nodes SET total_ram_mb = $1, updated_at = NOW()
			WHERE id = $2 AND total_ram_mb != $1`,
			input.RAMTotalMB, nodeID)
	}

	c.JSON(http.StatusOK, gin.H{"acknowledged": true})
}
func (h *NodeHandler) GetTelemetry(c *gin.Context) {
	nodeID := c.Param("id")
	type telRow struct {
		CPUCoresTotal int       `db:"cpu_cores_total" json:"cpu_cores_total"`
		CPUUtilPct    float64   `db:"cpu_util_pct"    json:"cpu_util_pct"`
		RAMTotalMB    int64     `db:"ram_total_mb"    json:"ram_total_mb"`
		RAMUsedMB     int64     `db:"ram_used_mb"     json:"ram_used_mb"`
		RAMAvailMB    int64     `db:"ram_avail_mb"    json:"ram_avail_mb"`
		NUMANodes     int       `db:"numa_nodes"      json:"numa_nodes"`
		DiskTotalGB   int64     `db:"disk_total_gb"   json:"disk_total_gb"`
		DiskUsedGB    int64     `db:"disk_used_gb"    json:"disk_used_gb"`
		RecordedAt    time.Time `db:"recorded_at"     json:"recorded_at"`
	}
	var rows []telRow
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT cpu_cores_total, cpu_util_pct, ram_total_mb, ram_used_mb, ram_avail_mb,
		       numa_nodes, disk_total_gb, disk_used_gb, recorded_at
		FROM node_telemetry WHERE node_id = $1
		ORDER BY recorded_at DESC LIMIT 60`, nodeID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if rows == nil {
		rows = []telRow{}
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "node_id": nodeID})
}

// GetInventory handles GET /admin/v1/nodes/:id/inventory
func (h *NodeHandler) GetInventory(c *gin.Context) {
	nodeID := c.Param("id")
	type invRow struct {
		ID         string    `db:"id"          json:"id"`
		Snapshot   []byte    `db:"snapshot"    json:"-"`
		SnapStr    string    `json:"snapshot"`
		AgentVer   string    `db:"agent_ver"   json:"agent_version"`
		ReportedAt time.Time `db:"reported_at" json:"reported_at"`
	}
	var row invRow
	if err := h.db.GetContext(c.Request.Context(), &row, `
		SELECT id, snapshot, agent_ver, reported_at
		FROM node_inventory_snapshots
		WHERE node_id = $1 ORDER BY reported_at DESC LIMIT 1`, nodeID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no inventory snapshot found"})
		return
	}
	if row.Snapshot != nil {
		row.SnapStr = string(row.Snapshot)
	}
	c.JSON(http.StatusOK, row)
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func marshalLabels(m map[string]interface{}) ([]byte, error) {
	return json.Marshal(m)
}
