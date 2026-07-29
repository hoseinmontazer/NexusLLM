package handlers

// agent.go — Agent API endpoints on the control plane.
//
// These routes are called BY node agents, not by human operators.
// All routes require a valid node token (agent JWT).
//
// Routes registered under /agent/v1 (separate group from /admin/v1):
//
//   POST   /agent/v1/register           — first-time node registration, returns token
//   POST   /agent/v1/heartbeat          — liveness + capabilities update
//   GET    /agent/v1/tasks/pending      — long-poll for pending tasks
//   POST   /agent/v1/tasks/:id/claim    — atomically claim a task
//   POST   /agent/v1/tasks/:id/running  — mark task as running
//   POST   /agent/v1/tasks/:id/complete — report success
//   POST   /agent/v1/tasks/:id/fail     — report failure
//   POST   /agent/v1/inventory          — push hardware inventory
//   POST   /agent/v1/telemetry          — push metrics snapshot
//   PUT    /agent/v1/runtimes/:id       — update runtime state
//   GET    /agent/v1/runtimes           — list runtimes on this node

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/agentauth"
	"github.com/nexusllm/nexusllm/internal/taskmanager"
	"go.uber.org/zap"
)

// AgentHandler handles all agent-to-control-plane communication.
type AgentHandler struct {
	db      *sqlx.DB
	authSvc *agentauth.Service
	taskMgr *taskmanager.Manager
	log     *zap.Logger
}

// NewAgentHandler constructs an AgentHandler.
// Pass the process-wide logger so UpdateRuntime diagnostic logs share the same
// encoder/output as all other structured logs.
func NewAgentHandler(db *sqlx.DB, authSvc *agentauth.Service, taskMgr *taskmanager.Manager, log *zap.Logger) *AgentHandler {
	if log == nil {
		log, _ = zap.NewProduction()
	}
	return &AgentHandler{db: db, authSvc: authSvc, taskMgr: taskMgr, log: log}
}

// ─── Registration ─────────────────────────────────────────────────────────────

// Register handles POST /agent/v1/register
// Called by a new agent on first boot. Auto-creates or finds the node row,
// issues a JWT token, and returns it to the agent.
// This endpoint does NOT require auth (it's how auth is bootstrapped).
func (h *AgentHandler) Register(c *gin.Context) {
	var input struct {
		Hostname     string                 `json:"hostname"      binding:"required"`
		IPAddress    string                 `json:"ip_address"`
		TotalCPU     int                    `json:"total_cpu"`
		TotalRAMMB   int64                  `json:"total_ram_mb"`
		TotalVRAMMB  int64                  `json:"total_vram_mb"`
		AgentVersion string                 `json:"agent_version"`
		Capabilities map[string]interface{} `json:"capabilities"`
		Labels       map[string]string      `json:"labels"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Find or create node
	var nodeID string
	err := h.db.QueryRowContext(c.Request.Context(),
		`SELECT id FROM nodes WHERE hostname = $1`, input.Hostname).Scan(&nodeID)

	if err != nil {
		// Node doesn't exist — create it
		nodeID = uuid.New().String()
		labelsJSON := "{}"
		if len(input.Labels) > 0 {
			if b, jerr := json.Marshal(input.Labels); jerr == nil {
				labelsJSON = string(b)
			}
		}
		capJSON := "{}"
		if len(input.Capabilities) > 0 {
			if b, jerr := json.Marshal(input.Capabilities); jerr == nil {
				capJSON = string(b)
			}
		}
		_, dbErr := h.db.ExecContext(c.Request.Context(), `
			INSERT INTO nodes
			  (id, hostname, display_name, total_cpu, total_ram_mb, total_vram_mb,
			   status, labels, capabilities, agent_version, last_heartbeat_at, created_at, updated_at)
			VALUES ($1,$2,$2,$3,$4,$5,'online',$6,$7,$8,NOW(),NOW(),NOW())`,
			nodeID, input.Hostname,
			input.TotalCPU, input.TotalRAMMB, input.TotalVRAMMB,
			labelsJSON, capJSON, input.AgentVersion,
		)
		if dbErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "create node: " + dbErr.Error()})
			return
		}
	} else {
		// Node exists — update it
		_, _ = h.db.ExecContext(c.Request.Context(), `
			UPDATE nodes
			SET total_cpu=$1, total_ram_mb=$2, total_vram_mb=$3,
			    status='online', agent_version=$4, last_heartbeat_at=NOW(), updated_at=NOW()
			WHERE id=$5`,
			input.TotalCPU, input.TotalRAMMB, input.TotalVRAMMB, input.AgentVersion, nodeID,
		)
	}

	// Update IP if provided
	if input.IPAddress != "" {
		_, _ = h.db.ExecContext(c.Request.Context(),
			`UPDATE nodes SET ip_address=$1 WHERE id=$2`, input.IPAddress, nodeID)
	}

	// Upsert capabilities
	if len(input.Capabilities) > 0 {
		h.upsertCapabilities(c.Request.Context(), nodeID, input.Capabilities)
	}

	// Issue JWT token
	token, err := h.authSvc.IssueToken(c.Request.Context(), nodeID, input.Hostname)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "issue token: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"node_id": nodeID,
		"token":   token,
		"message": "registered — store this token, it won't be shown again",
	})
}

// ─── Heartbeat ────────────────────────────────────────────────────────────────

// Heartbeat handles POST /agent/v1/heartbeat
// Requires valid agent token. Updates last_heartbeat_at and capabilities.
func (h *AgentHandler) Heartbeat(c *gin.Context) {
	claims := h.getAgentClaims(c)
	if claims == nil {
		return
	}

	var input struct {
		AgentVersion string                 `json:"agent_version"`
		Status       string                 `json:"status"`
		Capabilities map[string]interface{} `json:"capabilities"`
	}
	_ = c.ShouldBindJSON(&input)

	status := "online"
	if input.Status != "" {
		status = input.Status
	}

	_, _ = h.db.ExecContext(c.Request.Context(), `
		UPDATE nodes
		SET status=$1, last_heartbeat_at=NOW(),
		    agent_version=COALESCE(NULLIF($2,''), agent_version),
		    updated_at=NOW()
		WHERE id=$3`,
		status, input.AgentVersion, claims.NodeID,
	)

	if len(input.Capabilities) > 0 {
		h.upsertCapabilities(c.Request.Context(), claims.NodeID, input.Capabilities)
	}

	c.JSON(http.StatusOK, gin.H{
		"acknowledged": true,
		"server_time":  time.Now().UTC(),
		"node_id":      claims.NodeID,
	})
}

// ─── Task polling (long-poll) ─────────────────────────────────────────────────

// PollTasks handles GET /agent/v1/tasks/pending
// The agent calls this in a loop. If no tasks are available, the handler
// holds the connection open for up to `wait` seconds (long-poll pattern).
// This avoids tight polling without requiring WebSocket or gRPC streaming.
func (h *AgentHandler) PollTasks(c *gin.Context) {
	claims := h.getAgentClaims(c)
	if claims == nil {
		return
	}

	// Max wait in seconds (agent sends ?wait=30)
	waitSecs := 25
	if w := c.Query("wait"); w != "" {
		if n, err := strconv.Atoi(w); err == nil && n > 0 && n <= 60 {
			waitSecs = n
		}
	}
	limitStr := c.Query("limit")
	limit := 5
	if l, err := strconv.Atoi(limitStr); err == nil && l > 0 && l <= 20 {
		limit = l
	}

	deadline := time.Now().Add(time.Duration(waitSecs) * time.Second)
	pollInterval := 1 * time.Second

	for {
		tasks, err := h.taskMgr.PendingForNode(c.Request.Context(), claims.NodeID, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if len(tasks) > 0 {
			c.JSON(http.StatusOK, gin.H{"tasks": tasks, "count": len(tasks)})
			return
		}

		if time.Now().After(deadline) {
			// Long-poll timeout — return empty (agent will re-poll)
			c.JSON(http.StatusOK, gin.H{"tasks": []taskmanager.Task{}, "count": 0})
			return
		}

		// Check if client disconnected
		select {
		case <-c.Request.Context().Done():
			return
		case <-time.After(pollInterval):
			// continue polling
		}
	}
}

// ─── Task lifecycle ───────────────────────────────────────────────────────────

// ClaimTask handles POST /agent/v1/tasks/:id/claim
func (h *AgentHandler) ClaimTask(c *gin.Context) {
	claims := h.getAgentClaims(c)
	if claims == nil {
		return
	}
	taskID := c.Param("id")
	ok, err := h.taskMgr.ClaimTask(c.Request.Context(), taskID, claims.NodeID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !ok {
		c.JSON(http.StatusConflict, gin.H{"error": "task not found or already claimed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"claimed": true, "task_id": taskID})
}

// MarkTaskRunning handles POST /agent/v1/tasks/:id/running
func (h *AgentHandler) MarkTaskRunning(c *gin.Context) {
	claims := h.getAgentClaims(c)
	if claims == nil {
		return
	}
	taskID := c.Param("id")
	if err := h.taskMgr.MarkRunning(c.Request.Context(), taskID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"running": true})
}

// CompleteTask handles POST /agent/v1/tasks/:id/complete
func (h *AgentHandler) CompleteTask(c *gin.Context) {
	claims := h.getAgentClaims(c)
	if claims == nil {
		return
	}
	taskID := c.Param("id")
	var result map[string]interface{}
	_ = c.ShouldBindJSON(&result)

	if err := h.taskMgr.Complete(c.Request.Context(), taskID, result); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// If task has a runtime_id in the result, update the runtime state
	if runtimeID, ok := result["runtime_id"].(string); ok && runtimeID != "" {
		if state, ok := result["runtime_state"].(string); ok && state != "" {
			_, _ = h.db.ExecContext(c.Request.Context(), `
				UPDATE agent_runtimes SET state=$1, updated_at=NOW() WHERE id=$2`,
				state, runtimeID)
		}
		if containerID, ok := result["container_id"].(string); ok && containerID != "" {
			_, _ = h.db.ExecContext(c.Request.Context(), `
				UPDATE agent_runtimes SET container_id=$1, updated_at=NOW() WHERE id=$2`,
				containerID, runtimeID)
		}
		// If the agent resolved/downloaded a GGUF file, persist the path so the
		// next container start uses the cached volume file instead of re-downloading.
		if ggufPath, ok := result["gguf_path"].(string); ok && ggufPath != "" {
			// Persist to model_runtime_configs for all future starts of this model.
			_, _ = h.db.ExecContext(c.Request.Context(), `
				UPDATE model_runtime_configs
				SET gguf_path = $1, updated_at = NOW()
				WHERE model_id = (SELECT model_id FROM agent_runtimes WHERE id = $2)
				  AND (gguf_path IS NULL OR gguf_path = '')`,
				ggufPath, runtimeID)
		}
		// If the agent picked a different port (port conflict resolution), persist it
		// so the control plane polls the correct address during waitForReady.
		if bindPort, ok := result["bind_port"].(float64); ok && bindPort > 0 {
			actualPort := bindPort
			// Prefer actual_port when present — it is the post-inspect value from
			// discoverPortForBackend, which is always the definitive listening port.
			if ap, ok := result["actual_port"].(float64); ok && ap > 0 {
				actualPort = ap
			}
			// FIX-5: monotonic guard — prevents a late-arriving retry from
			// reverting a newer port value.
			_, _ = h.db.ExecContext(c.Request.Context(), `
				UPDATE agent_runtimes
				SET bind_port   = $1,
				    actual_port = $1,
				    updated_at  = NOW()
				WHERE id = $2
				  AND (bind_port = 0 OR updated_at < NOW() - INTERVAL '2 seconds')`,
				int(actualPort), runtimeID)

			// Primary sync via endpoint_id (activator-spawned runtimes).
			_, _ = h.db.ExecContext(c.Request.Context(), `
				UPDATE model_endpoints
				SET port                 = $1,
				    consecutive_failures = 0,
				    is_enabled           = TRUE,
				    lifecycle_state      = CASE
				      WHEN lifecycle_state IN ('unloaded','failed') THEN 'loading'
				      ELSE lifecycle_state
				    END,
				    updated_at = NOW()
				WHERE id = (
				    SELECT endpoint_id FROM agent_runtimes
				    WHERE id = $2 AND endpoint_id IS NOT NULL
				)`,
				int(actualPort), runtimeID)

			// FIX-4: secondary sync for HA replicas (endpoint_id IS NULL).
			// Only sync in terminal-startup states so we don't overwrite with a
			// port from a still-starting replica.
			if state, ok := result["runtime_state"].(string); ok &&
				(state == "ready" || state == "active" || state == "loading_model") {
				_, _ = h.db.ExecContext(c.Request.Context(), `
					UPDATE model_endpoints me
					SET    port       = $1,
					       updated_at = NOW()
					FROM   agent_runtimes ar
					WHERE  ar.id            = $2
					  AND  ar.endpoint_id   IS NULL
					  AND  me.model_id      = ar.model_id
					  AND  me.is_enabled    = TRUE`,
					int(actualPort), runtimeID)
			}
		}
	}

	// For UNLOAD_RUNTIME tasks: transition runtime to stopped and re-enable
	// after the agent confirms the container stopped cleanly.
	// The idle manager set state='stopping'; now we complete the transition.
	var taskType string
	_ = h.db.QueryRowContext(c.Request.Context(),
		`SELECT task_type FROM agent_tasks WHERE id=$1`, taskID,
	).Scan(&taskType)
	if taskType == "UNLOAD_RUNTIME" {
		if runtimeID, ok := result["runtime_id"].(string); ok && runtimeID != "" {
			_, _ = h.db.ExecContext(c.Request.Context(), `
				UPDATE agent_runtimes
				SET state = 'stopped', updated_at = NOW()
				WHERE id = $1 AND state = 'stopping'`, runtimeID)
		}
	}

	c.JSON(http.StatusOK, gin.H{"completed": true})
}

// FailTask handles POST /agent/v1/tasks/:id/fail
func (h *AgentHandler) FailTask(c *gin.Context) {
	claims := h.getAgentClaims(c)
	if claims == nil {
		return
	}
	taskID := c.Param("id")
	var input struct {
		Error        string `json:"error"`
		RuntimeID    string `json:"runtime_id"`
		RuntimeState string `json:"runtime_state"` // optional: "stopped" means needs redeploy, not broken
	}
	_ = c.ShouldBindJSON(&input)

	if err := h.taskMgr.Fail(c.Request.Context(), taskID, input.Error); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Mark associated runtime — use the reported state if provided, otherwise "failed".
	if input.RuntimeID != "" {
		targetState := "failed"
		if input.RuntimeState != "" {
			targetState = input.RuntimeState
		}
		_, _ = h.db.ExecContext(c.Request.Context(), `
			UPDATE agent_runtimes SET state=$1, updated_at=NOW(), container_id=''
			WHERE id=$2`, targetState, input.RuntimeID)

		// When the container is gone (state="stopped"), clear container_id and
		// let the activator's deployFresh create a new one on the next request.
		if targetState == "stopped" {
			_, _ = h.db.ExecContext(c.Request.Context(), `
				UPDATE agent_runtimes
				SET container_id = '', error_msg = $1, updated_at = NOW()
				WHERE id = $2`, input.Error, input.RuntimeID)
		}
	}

	c.JSON(http.StatusOK, gin.H{"failed": true})
}

// ─── Inventory & Telemetry ────────────────────────────────────────────────────

// PushInventory handles POST /agent/v1/inventory
func (h *AgentHandler) PushInventory(c *gin.Context) {
	claims := h.getAgentClaims(c)
	if claims == nil {
		return
	}
	var snapshot map[string]interface{}
	if err := c.ShouldBindJSON(&snapshot); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	b, _ := json.Marshal(snapshot)
	agentVer := ""
	if v, ok := snapshot["agent_version"].(string); ok {
		agentVer = v
	}
	_, _ = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO node_inventory_snapshots (id, node_id, snapshot, agent_ver, reported_at)
		VALUES (gen_random_uuid(),$1,$2,$3,NOW())`,
		claims.NodeID, string(b), agentVer,
	)
	// Update node totals
	if cpuCores, ok := snapshot["cpu_cores"].(float64); ok && cpuCores > 0 {
		_, _ = h.db.ExecContext(c.Request.Context(), `
			UPDATE nodes SET total_cpu=$1, updated_at=NOW() WHERE id=$2`,
			int(cpuCores), claims.NodeID)
	}
	if ramMB, ok := snapshot["ram_total_mb"].(float64); ok && ramMB > 0 {
		_, _ = h.db.ExecContext(c.Request.Context(), `
			UPDATE nodes SET total_ram_mb=$1, updated_at=NOW() WHERE id=$2`,
			int64(ramMB), claims.NodeID)
	}
	c.JSON(http.StatusOK, gin.H{"acknowledged": true})
}

// PushModelCache handles POST /agent/v1/model-cache
// Stores which models are cached on this node.
func (h *AgentHandler) PushModelCache(c *gin.Context) {
	claims := h.getAgentClaims(c)
	if claims == nil {
		return
	}
	var input struct {
		Models []struct {
			ModelRef  string `json:"model_ref"`
			Backend   string `json:"backend"`
			SizeBytes int64  `json:"size_bytes"`
			IsCached  bool   `json:"is_cached"`
		} `json:"models"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	for _, m := range input.Models {
		if m.ModelRef == "" || m.Backend == "" {
			continue
		}
		_, _ = h.db.ExecContext(c.Request.Context(), `
			INSERT INTO node_model_cache
			  (id, node_id, model_ref, backend, is_cached, size_bytes, cached_at, last_verified)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, NOW(), NOW())
			ON CONFLICT (node_id, model_ref, backend) DO UPDATE SET
			  is_cached     = EXCLUDED.is_cached,
			  size_bytes    = EXCLUDED.size_bytes,
			  cached_at     = CASE WHEN NOT node_model_cache.is_cached AND EXCLUDED.is_cached
			                       THEN NOW() ELSE node_model_cache.cached_at END,
			  last_verified = NOW()`,
			claims.NodeID, m.ModelRef, m.Backend, m.IsCached, m.SizeBytes,
		)
	}

	c.JSON(http.StatusOK, gin.H{"acknowledged": true, "count": len(input.Models)})
}

// GetModelCache handles GET /admin/v1/nodes/:id/model-cache
// Returns which models are cached on a node (used by deploy form).
func (h *AgentHandler) GetNodeModelCache(c *gin.Context) {
	nodeID := c.Param("id")
	type cacheRow struct {
		ModelRef  string     `db:"model_ref"  json:"model_ref"`
		Backend   string     `db:"backend"    json:"backend"`
		SizeBytes int64      `db:"size_bytes" json:"size_bytes"`
		IsCached  bool       `db:"is_cached"  json:"is_cached"`
		CachedAt  *time.Time `db:"cached_at" json:"cached_at"`
	}
	var rows []cacheRow
	_ = h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT model_ref, backend, size_bytes, is_cached, cached_at
		FROM node_model_cache
		WHERE node_id = $1 AND is_cached = TRUE
		ORDER BY backend, model_ref`, nodeID)
	if rows == nil {
		rows = []cacheRow{}
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "node_id": nodeID, "total": len(rows)})
}

// PushTelemetry handles POST /agent/v1/telemetry
func (h *AgentHandler) PushTelemetry(c *gin.Context) {
	claims := h.getAgentClaims(c)
	if claims == nil {
		return
	}
	var input struct {
		CPUCoresTotal int                      `json:"cpu_cores_total"`
		CPUUtilPct    float64                  `json:"cpu_util_pct"`
		RAMTotalMB    int64                    `json:"ram_total_mb"`
		RAMUsedMB     int64                    `json:"ram_used_mb"`
		RAMAvailMB    int64                    `json:"ram_avail_mb"`
		NUMANodes     int                      `json:"numa_nodes"`
		DiskTotalGB   int64                    `json:"disk_total_gb"`
		DiskUsedGB    int64                    `json:"disk_used_gb"`
		GPUs          []map[string]interface{} `json:"gpus"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	_, _ = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO node_telemetry
		  (node_id, cpu_cores_total, cpu_util_pct,
		   ram_total_mb, ram_used_mb, ram_avail_mb,
		   numa_nodes, disk_total_gb, disk_used_gb, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NOW())`,
		claims.NodeID,
		input.CPUCoresTotal, input.CPUUtilPct,
		input.RAMTotalMB, input.RAMUsedMB, input.RAMAvailMB,
		input.NUMANodes, input.DiskTotalGB, input.DiskUsedGB,
	)
	// Update node RAM total
	if input.RAMTotalMB > 0 {
		_, _ = h.db.ExecContext(c.Request.Context(), `
			UPDATE nodes SET total_ram_mb=$1, updated_at=NOW()
			WHERE id=$2 AND total_ram_mb != $1`, input.RAMTotalMB, claims.NodeID)
	}
	c.JSON(http.StatusOK, gin.H{"acknowledged": true})
}

// ─── Runtime state updates ────────────────────────────────────────────────────

// UpdateRuntime handles PUT /agent/v1/runtimes/:id
// The agent reports runtime state changes (pulling → starting → active, etc.)
func (h *AgentHandler) UpdateRuntime(c *gin.Context) {
	claims := h.getAgentClaims(c)
	if claims == nil {
		return
	}
	runtimeID := c.Param("id")
	var input struct {
		State        string `json:"state"`
		ContainerID  string `json:"container_id"`
		HealthStatus string `json:"health_status"`
		BindPort     int    `json:"bind_port"`
		ErrorMsg     string `json:"error_msg"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Only allow state update for runtimes owned by this node
	if input.State != "" {
		// ── Diagnostic: capture last_used_at BEFORE the update so we can see
		// if this state report is about to reset the idle clock.
		// The UPDATE below sets last_used_at = NOW() whenever state IN ('ready','active'),
		// which means every agent startup notification resets the idle countdown.
		// Log at WARN so it is always visible — this is the write path most
		// likely to cause premature UNLOAD_RUNTIME if the idle timeout is short.
		if input.State == "ready" || input.State == "active" {
			var prevLastUsedAt *time.Time
			_ = h.db.QueryRowContext(c.Request.Context(),
				`SELECT last_used_at FROM agent_runtimes WHERE id = $1`, runtimeID,
			).Scan(&prevLastUsedAt)
			prevStr := "<nil>"
			if prevLastUsedAt != nil {
				prevStr = prevLastUsedAt.Format(time.RFC3339)
			}
			h.log.Warn("UpdateRuntime: resetting last_used_at to NOW() — idle clock restarted",
				zap.String("caller", "agent.UpdateRuntime"),
				zap.String("runtime_id", runtimeID),
				zap.String("new_state", input.State),
				zap.String("prev_last_used_at", prevStr),
				zap.String("reason", "agent_state_report_ready_or_active"),
			)
		}

		_, err := h.db.ExecContext(c.Request.Context(), `
			UPDATE agent_runtimes
			SET state=$1::varchar, updated_at=NOW(),
			    started_at   = CASE WHEN $1::text IN ('ready','active') AND started_at IS NULL THEN NOW() ELSE started_at END,
			    last_used_at = CASE WHEN $1::text IN ('ready','active') THEN NOW() ELSE last_used_at END,
			    stopped_at   = CASE WHEN $1::text IN ('stopped','unloaded','deleted') THEN NOW() ELSE stopped_at END
			WHERE id=$2 AND node_id=$3`,
			input.State, runtimeID, claims.NodeID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	if input.ContainerID != "" {
		_, _ = h.db.ExecContext(c.Request.Context(), `
			UPDATE agent_runtimes SET container_id=$1, updated_at=NOW()
			WHERE id=$2 AND node_id=$3`,
			input.ContainerID, runtimeID, claims.NodeID)
	}
	if input.HealthStatus != "" {
		_, _ = h.db.ExecContext(c.Request.Context(), `
			UPDATE agent_runtimes SET health_status=$1, last_health_at=NOW(), updated_at=NOW()
			WHERE id=$2 AND node_id=$3`,
			input.HealthStatus, runtimeID, claims.NodeID)
	}
	if input.BindPort > 0 {
		// FIX-5: monotonic guard — only write if the row hasn't been updated
		// more recently (prevents a late-arriving retry from reverting a newer port).
		_, _ = h.db.ExecContext(c.Request.Context(), `
			UPDATE agent_runtimes
			SET bind_port  = $1,
			    actual_port = $1,
			    updated_at  = NOW()
			WHERE id       = $2
			  AND node_id  = $3
			  AND (bind_port = 0 OR updated_at < NOW() - INTERVAL '2 seconds')`,
			input.BindPort, runtimeID, claims.NodeID)

		// Primary sync via endpoint_id (works for activator-spawned runtimes).
		_, _ = h.db.ExecContext(c.Request.Context(), `
			UPDATE model_endpoints
			SET port                 = $1,
			    consecutive_failures = 0,
			    is_enabled           = TRUE,
			    lifecycle_state      = CASE
			      WHEN lifecycle_state IN ('unloaded','failed') THEN 'loading'
			      ELSE lifecycle_state
			    END,
			    updated_at = NOW()
			WHERE id = (
			    SELECT endpoint_id FROM agent_runtimes
			    WHERE id = $2 AND endpoint_id IS NOT NULL
			)`,
			input.BindPort, runtimeID)

		// FIX-4: secondary sync for HA replicas where endpoint_id IS NULL.
		// The reconciler inserts agent_runtimes rows with endpoint_id = NULL,
		// so the primary sync above matches zero rows for those replicas.
		// When the runtime transitions to ready/active, find the model_endpoints
		// row via model_id and sync the port so the table stays consistent.
		// Only update for active states to avoid overwriting with a port from
		// a stale replica that is still starting.
		if input.State == "ready" || input.State == "active" {
			_, _ = h.db.ExecContext(c.Request.Context(), `
				UPDATE model_endpoints me
				SET    port       = $1,
				       updated_at = NOW()
				FROM   agent_runtimes ar
				WHERE  ar.id        = $2
				  AND  ar.node_id   = $3
				  AND  ar.endpoint_id IS NULL
				  AND  me.model_id  = ar.model_id
				  AND  me.is_enabled = TRUE`,
				input.BindPort, runtimeID, claims.NodeID)
		}
	}

	// Sync back to model_endpoints if linked
	if input.State == "active" || input.State == "ready" || input.HealthStatus != "" {
		h.syncEndpointFromRuntime(c.Request.Context(), runtimeID, input.State, input.HealthStatus, input.ContainerID)
	}

	c.JSON(http.StatusOK, gin.H{"updated": true})
}

// ListRuntimes handles GET /agent/v1/runtimes
func (h *AgentHandler) ListRuntimes(c *gin.Context) {
	claims := h.getAgentClaims(c)
	if claims == nil {
		return
	}
	type runtimeRow struct {
		ID           string    `db:"id"            json:"id"`
		RuntimeName  string    `db:"runtime_name"  json:"runtime_name"`
		Backend      string    `db:"backend"       json:"backend"`
		State        string    `db:"state"         json:"state"`
		ContainerID  string    `db:"container_id"  json:"container_id"`
		HealthStatus string    `db:"health_status" json:"health_status"`
		BindPort     int       `db:"bind_port"     json:"bind_port"`
		UpdatedAt    time.Time `db:"updated_at"    json:"updated_at"`
	}
	var rows []runtimeRow
	_ = h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT id, runtime_name, backend, state, container_id,
		       health_status, bind_port, updated_at
		FROM agent_runtimes
		WHERE node_id=$1 AND state NOT IN ('deleted')
		ORDER BY updated_at DESC`, claims.NodeID)
	if rows == nil {
		rows = []runtimeRow{}
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": len(rows)})
}

// ─── Container death notification ────────────────────────────────────────────

// ContainerDied handles POST /agent/v1/container-died
// Called by the node agent's containerWatchLoop when it detects a nexus-*
// container has exited unexpectedly. Immediately marks the matching
// agent_runtimes row as 'failed' so the reconciler and stuck-runtime sweeper
// can react within one cycle instead of waiting for the 30s telemetry loop.
func (h *AgentHandler) ContainerDied(c *gin.Context) {
	claims := h.getAgentClaims(c)
	if claims == nil {
		return
	}
	var input struct {
		ContainerName string `json:"container_name"`
		ContainerID   string `json:"container_id"`
		Reason        string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.ContainerName == "" && input.ContainerID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "container_name or container_id required"})
		return
	}

	reason := input.Reason
	if reason == "" {
		reason = "container exited unexpectedly (detected by node agent watcher)"
	}
	// Tag the error with a sentinel so sweepFailedContainers can skip the
	// 5-minute grace period for containers that are confirmed gone.
	const containerDeadTag = "[container-dead]"
	if !strings.Contains(reason, containerDeadTag) {
		reason = containerDeadTag + " " + reason
	}

	// Match by container_id first (exact), fall back to runtime_name.
	// Only update rows that were in a live state — don't touch already-failed rows.
	var res interface{ RowsAffected() (int64, error) }
	var err error

	if input.ContainerID != "" {
		res, err = h.db.ExecContext(c.Request.Context(), `
			UPDATE agent_runtimes
			SET state     = 'failed',
			    error_msg = $1,
			    updated_at = NOW()
			WHERE node_id = $2
			  AND container_id = $3
			  AND state IN ('ready','active','warm','idle','loading_model',
			                'waiting_ready','loading','starting','pending')`,
			reason, claims.NodeID, input.ContainerID)
	} else {
		res, err = h.db.ExecContext(c.Request.Context(), `
			UPDATE agent_runtimes
			SET state     = 'failed',
			    error_msg = $1,
			    updated_at = NOW()
			WHERE node_id = $2
			  AND runtime_name = $3
			  AND state IN ('ready','active','warm','idle','loading_model',
			                'waiting_ready','loading','starting','pending')`,
			reason, claims.NodeID, input.ContainerName)
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	n, _ := res.RowsAffected()
	c.JSON(http.StatusOK, gin.H{"acknowledged": true, "rows_updated": n})
}

// ─── Middleware ───────────────────────────────────────────────────────────────

// AgentAuthMiddleware validates the node's JWT token.
// The token must be passed as: Authorization: Bearer <token>
func (h *AgentHandler) AgentAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" || len(header) < 8 || header[:7] != "Bearer " {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing agent token"})
			return
		}
		token := header[7:]
		claims, err := h.authSvc.Validate(c.Request.Context(), token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid agent token: " + err.Error()})
			return
		}
		c.Set("agent_claims", claims)
		c.Next()
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func (h *AgentHandler) getAgentClaims(c *gin.Context) *agentauth.NodeClaims {
	v, exists := c.Get("agent_claims")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
		return nil
	}
	claims, ok := v.(*agentauth.NodeClaims)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid claims"})
		return nil
	}
	return claims
}

func (h *AgentHandler) upsertCapabilities(ctx context.Context, nodeID string, caps map[string]interface{}) {
	b, err := json.Marshal(caps)
	if err != nil {
		return
	}
	_, _ = h.db.ExecContext(ctx, `
		UPDATE nodes SET capabilities=$1, updated_at=NOW() WHERE id=$2`,
		string(b), nodeID)

	// Also write to the typed node_capabilities table
	docker, _ := caps["docker"].(bool)
	vllm, _ := caps["vllm"].(bool)
	tgi, _ := caps["tgi"].(bool)
	whisper, _ := caps["whisper"].(bool)
	tts, _ := caps["tts"].(bool)
	embedding, _ := caps["embedding"].(bool)
	hasGPU, _ := caps["gpu"].(bool)
	gpuCount := 0
	if n, ok := caps["gpu_count"].(float64); ok {
		gpuCount = int(n)
	}

	// Build supported_backends JSONB array from the individual boolean flags.
	// This is the forward-compatible field; the boolean columns are kept for
	// backward compat but new code should read supported_backends.
	supportedBackends := `[]`
	{
		var backends []string
		if vllm {
			backends = append(backends, `"vllm"`)
		}
		if tgi {
			backends = append(backends, `"tgi"`)
		}
		if whisper {
			backends = append(backends, `"faster_whisper"`)
		}
		if tts {
			backends = append(backends, `"kokoro"`)
		}
		if embedding {
			backends = append(backends, `"cpu_native"`)
		}
		// openai_compat is available on any node that can run Docker
		if docker {
			backends = append(backends, `"openai_compat"`)
		}
		if len(backends) > 0 {
			supportedBackends = `[` + strings.Join(backends, ",") + `]`
		}
	}

	_, _ = h.db.ExecContext(ctx, `
		INSERT INTO node_capabilities
		  (node_id, has_docker, has_vllm, has_ollama, has_tgi,
		   has_whisper, has_tts, has_embedding, has_gpu, gpu_count,
		   gpu_available, gpu_vram_mb, supported_backends, updated_at)
		VALUES ($1,$2,$3,FALSE,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,NOW())
		ON CONFLICT (node_id) DO UPDATE SET
		  has_docker=$2, has_vllm=$3, has_ollama=FALSE, has_tgi=$4,
		  has_whisper=$5, has_tts=$6, has_embedding=$7,
		  has_gpu=$8, gpu_count=$9,
		  gpu_available=$10, gpu_vram_mb=$11,
		  supported_backends=$11::jsonb,
		  updated_at=NOW()`,
		nodeID, docker, vllm, tgi, whisper, tts, embedding, hasGPU, gpuCount,
		hasGPU, // gpu_available mirrors has_gpu
		func() int64 {
			if v, ok := caps["gpu_vram_mb"].(float64); ok {
				return int64(v)
			}
			return 0
		}(),
		supportedBackends,
	)
}

func (h *AgentHandler) syncEndpointFromRuntime(ctx context.Context, runtimeID, state, health, containerID string) {
	lifecycleMap := map[string]string{
		// New unified pipeline states
		"created":       "registered",
		"validating":    "loading",
		"downloading":   "downloading",
		"starting":      "loading",
		"loading_model": "loading",
		"waiting_ready": "loading",
		"ready":         "active",
		// Legacy / operational states
		"pulling":  "downloading",
		"loading":  "loading",
		"active":   "active",
		"warm":     "warm",
		"idle":     "idle",
		"stopping": "draining",
		"stopped":  "unloaded",
		"failed":   "failed",
		"lost":     "failed",
	}
	var epID string
	if err := h.db.QueryRowContext(ctx,
		`SELECT COALESCE(endpoint_id::text,'') FROM agent_runtimes WHERE id=$1`, runtimeID,
	).Scan(&epID); err != nil || epID == "" {
		return
	}
	if ls, ok := lifecycleMap[state]; ok && state != "" {
		_, _ = h.db.ExecContext(ctx, `
			UPDATE model_endpoints
			SET lifecycle_state=$1, updated_at=NOW()
			WHERE id=$2`, ls, epID)
	}
	if health != "" {
		_, _ = h.db.ExecContext(ctx, `
			UPDATE model_endpoints SET health_status=$1, updated_at=NOW()
			WHERE id=$2`, health, epID)
	}
	if containerID != "" {
		_, _ = h.db.ExecContext(ctx, `
			UPDATE model_endpoints SET container_id=$1, updated_at=NOW()
			WHERE id=$2`, containerID, epID)
	}
}
