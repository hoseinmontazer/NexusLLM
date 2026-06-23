// Package handlers — project.go
// Implements all Project admin API endpoints:
//   POST   /admin/v1/projects
//   GET    /admin/v1/projects
//   GET    /admin/v1/projects/:id
//   PUT    /admin/v1/projects/:id
//   DELETE /admin/v1/projects/:id
//   POST   /admin/v1/projects/:id/reserve
//   POST   /admin/v1/projects/:id/priority
//   PUT    /admin/v1/projects/:id/protection
//   GET    /admin/v1/projects/:id/runtimes
//   GET    /admin/v1/projects/:id/usage
//   GET    /admin/v1/projects/:id/preemptions
//   GET    /admin/v1/projects/:id/queue
package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/project"
)

// ProjectHandler handles all project admin operations.
type ProjectHandler struct {
	db *sqlx.DB
}

// NewProjectHandler constructs a ProjectHandler.
func NewProjectHandler(db *sqlx.DB) *ProjectHandler {
	return &ProjectHandler{db: db}
}

// ─── Create ──────────────────────────────────────────────────────────────────

// CreateProject handles POST /admin/v1/projects
func (h *ProjectHandler) CreateProject(c *gin.Context) {
	var input struct {
		OrganizationID string `json:"organization_id" binding:"required"`
		TeamID         string `json:"team_id"         binding:"required"`
		Name           string `json:"name"            binding:"required"`
		Description    string `json:"description"`
		Priority       string `json:"priority"`
		Status         string `json:"status"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Validate name length
	if len(input.Name) > 200 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name must be ≤ 200 characters"})
		return
	}
	if len(input.Description) > 1000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "description must be ≤ 1000 characters"})
		return
	}

	// Apply defaults
	if input.Priority == "" {
		input.Priority = string(project.PriorityNormal)
	}
	if input.Status == "" {
		input.Status = "active"
	}

	// Validate priority
	if !project.Priority(input.Priority).IsValid() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "priority must be one of: CRITICAL, HIGH, NORMAL, LOW, BEST_EFFORT"})
		return
	}

	// Validate status
	switch input.Status {
	case "active", "inactive", "archived":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "status must be one of: active, inactive, archived"})
		return
	}

	// Verify team exists and belongs to the org
	var teamCount int
	if err := h.db.GetContext(c.Request.Context(), &teamCount,
		`SELECT COUNT(*) FROM teams WHERE id=$1 AND org_id=$2 AND active=TRUE`,
		input.TeamID, input.OrganizationID); err != nil || teamCount == 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "team not found or not active in the specified organization"})
		return
	}

	id := uuid.New().String()
	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO projects (id, organization_id, team_id, name, description, priority, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		id, input.OrganizationID, input.TeamID, input.Name, input.Description, input.Priority, input.Status,
	)
	if err != nil {
		// Check for unique constraint violation (same name in same team)
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "a project with this name already exists in the team"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Seed default configs
	_, _ = h.db.ExecContext(c.Request.Context(),
		`INSERT INTO project_configurations (project_id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
	_, _ = h.db.ExecContext(c.Request.Context(),
		`INSERT INTO project_reservations (project_id) VALUES ($1) ON CONFLICT DO NOTHING`, id)

	c.JSON(http.StatusCreated, gin.H{"id": id, "name": input.Name, "priority": input.Priority, "status": input.Status})
}

// ─── List ─────────────────────────────────────────────────────────────────────

// ListProjects handles GET /admin/v1/projects
func (h *ProjectHandler) ListProjects(c *gin.Context) {
	type projectRow struct {
		ID             string    `db:"id"              json:"id"`
		OrganizationID string    `db:"organization_id" json:"organization_id"`
		TeamID         string    `db:"team_id"         json:"team_id"`
		Name           string    `db:"name"            json:"name"`
		Description    string    `db:"description"     json:"description"`
		Priority       string    `db:"priority"        json:"priority"`
		PriorityScore  int       `db:"priority_score"  json:"priority_score"`
		Status         string    `db:"status"          json:"status"`
		RuntimeCount   int       `db:"runtime_count"   json:"runtime_count"`
		ReservedVRAMMB int64     `db:"reserved_vram_mb" json:"reserved_vram_mb"`
		CreatedAt      time.Time `db:"created_at"      json:"created_at"`
		UpdatedAt      time.Time `db:"updated_at"      json:"updated_at"`
	}

	q := `SELECT p.id, p.organization_id, p.team_id, p.name, p.description,
	             p.priority, project_priority_score(p.priority) AS priority_score,
	             p.status, p.created_at, p.updated_at,
	             COUNT(ar.id) FILTER (WHERE ar.state IN ('active','warm')) AS runtime_count,
	             COALESCE(pr.reserved_vram_mb, 0) AS reserved_vram_mb
	      FROM projects p
	      LEFT JOIN agent_runtimes ar ON ar.project_id = p.id
	      LEFT JOIN project_reservations pr ON pr.project_id = p.id
	      WHERE 1=1`

	args := []interface{}{}
	idx := 1

	if orgID := c.Query("org_id"); orgID != "" {
		q += " AND p.organization_id = $" + strconv.Itoa(idx)
		args = append(args, orgID)
		idx++
	}
	if teamID := c.Query("team_id"); teamID != "" {
		q += " AND p.team_id = $" + strconv.Itoa(idx)
		args = append(args, teamID)
		idx++
	}
	if priority := c.Query("priority"); priority != "" {
		q += " AND p.priority = $" + strconv.Itoa(idx)
		args = append(args, priority)
		idx++
	}
	if status := c.Query("status"); status != "" {
		q += " AND p.status = $" + strconv.Itoa(idx)
		args = append(args, status)
		idx++
	}

	q += " GROUP BY p.id, pr.reserved_vram_mb ORDER BY project_priority_score(p.priority) DESC, p.name"

	var rows []projectRow
	if err := h.db.SelectContext(c.Request.Context(), &rows, q, args...); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if rows == nil {
		rows = []projectRow{}
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": len(rows)})
}

// ─── Get ──────────────────────────────────────────────────────────────────────

// GetProject handles GET /admin/v1/projects/:id
func (h *ProjectHandler) GetProject(c *gin.Context) {
	id := c.Param("id")
	type fullRow struct {
		ID              string     `db:"id"               json:"id"`
		OrganizationID  string     `db:"organization_id"  json:"organization_id"`
		TeamID          string     `db:"team_id"          json:"team_id"`
		Name            string     `db:"name"             json:"name"`
		Description     string     `db:"description"      json:"description"`
		Priority        string     `db:"priority"         json:"priority"`
		PriorityScore   int        `db:"priority_score"   json:"priority_score"`
		Status          string     `db:"status"           json:"status"`
		CreatedAt       time.Time  `db:"created_at"       json:"created_at"`
		UpdatedAt       time.Time  `db:"updated_at"       json:"updated_at"`
		ReservedVRAMMB  int64      `db:"reserved_vram_mb" json:"reserved_vram_mb"`
		ReservedCPU     int        `db:"reserved_cpu_cores" json:"reserved_cpu_cores"`
		ReservedMemMB   int64      `db:"reserved_memory_mb" json:"reserved_memory_mb"`
		AlwaysRunning   bool       `db:"always_running"   json:"always_running"`
		Protected       bool       `db:"protected"        json:"protected"`
		MinReplicas     int        `db:"minimum_replicas" json:"minimum_replicas"`
		AdmissionPolicy string     `db:"admission_policy" json:"admission_policy"`
	}
	var row fullRow
	err := h.db.GetContext(c.Request.Context(), &row, `
		SELECT p.id, p.organization_id, p.team_id, p.name, p.description,
		       p.priority, project_priority_score(p.priority) AS priority_score,
		       p.status, p.created_at, p.updated_at,
		       COALESCE(pr.reserved_vram_mb,0)   AS reserved_vram_mb,
		       COALESCE(pr.reserved_cpu_cores,0) AS reserved_cpu_cores,
		       COALESCE(pr.reserved_memory_mb,0) AS reserved_memory_mb,
		       COALESCE(pc.always_running,FALSE)  AS always_running,
		       COALESCE(pc.protected,FALSE)       AS protected,
		       COALESCE(pc.minimum_replicas,0)    AS minimum_replicas,
		       COALESCE(pc.admission_policy,'queue') AS admission_policy
		FROM projects p
		LEFT JOIN project_reservations pr ON pr.project_id = p.id
		LEFT JOIN project_configurations pc ON pc.project_id = p.id
		WHERE p.id = $1`, id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}
	c.JSON(http.StatusOK, row)
}

// ─── Update ───────────────────────────────────────────────────────────────────

// UpdateProject handles PUT /admin/v1/projects/:id
func (h *ProjectHandler) UpdateProject(c *gin.Context) {
	id := c.Param("id")

	// Verify exists
	var existing struct {
		Name     string `db:"name"`
		Priority string `db:"priority"`
	}
	if err := h.db.GetContext(c.Request.Context(), &existing,
		`SELECT name, priority FROM projects WHERE id=$1`, id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	var input struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Priority    *string `json:"priority"`
		Status      *string `json:"status"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if input.Name != nil && len(*input.Name) > 200 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name must be ≤ 200 characters"})
		return
	}
	if input.Description != nil && len(*input.Description) > 1000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "description must be ≤ 1000 characters"})
		return
	}
	if input.Priority != nil && !project.Priority(*input.Priority).IsValid() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "priority must be one of: CRITICAL, HIGH, NORMAL, LOW, BEST_EFFORT"})
		return
	}
	if input.Status != nil {
		switch *input.Status {
		case "active", "inactive", "archived":
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "status must be one of: active, inactive, archived"})
			return
		}
	}

	_, err := h.db.ExecContext(c.Request.Context(), `
		UPDATE projects SET
		  name        = COALESCE($2, name),
		  description = COALESCE($3, description),
		  priority    = COALESCE($4, priority),
		  status      = COALESCE($5, status),
		  updated_at  = NOW()
		WHERE id = $1`,
		id, input.Name, input.Description, input.Priority, input.Status)
	if err != nil {
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "a project with this name already exists in the team"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Audit priority change
	if input.Priority != nil && *input.Priority != existing.Priority {
		h.auditPriorityChange(c, id, existing.Priority, *input.Priority)
	}

	c.JSON(http.StatusOK, gin.H{"message": "project updated", "id": id})
}

// ─── Delete ───────────────────────────────────────────────────────────────────

// DeleteProject handles DELETE /admin/v1/projects/:id
func (h *ProjectHandler) DeleteProject(c *gin.Context) {
	id := c.Param("id")

	// Check for associated models or runtimes
	var count int
	_ = h.db.GetContext(c.Request.Context(), &count, `
		SELECT COUNT(*) FROM (
		  SELECT id FROM models WHERE project_id=$1
		  UNION ALL
		  SELECT id FROM agent_runtimes WHERE project_id=$1 AND state NOT IN ('deleted','archived')
		) combined`, id)
	if count > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "project still has associated models or runtimes — remove them first"})
		return
	}

	res, err := h.db.ExecContext(c.Request.Context(), `DELETE FROM projects WHERE id=$1`, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "project deleted", "id": id})
}

// ─── Reserve ──────────────────────────────────────────────────────────────────

// Reserve handles POST /admin/v1/projects/:id/reserve
func (h *ProjectHandler) Reserve(c *gin.Context) {
	id := c.Param("id")

	// Verify project exists
	var exists int
	if err := h.db.GetContext(c.Request.Context(), &exists,
		`SELECT COUNT(*) FROM projects WHERE id=$1`, id); err != nil || exists == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	var input struct {
		ReservedVRAMMB   *int64 `json:"reserved_vram_mb"`
		ReservedCPUCores *int   `json:"reserved_cpu_cores"`
		ReservedMemoryMB *int64 `json:"reserved_memory_mb"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Validate non-negative
	if input.ReservedVRAMMB != nil && *input.ReservedVRAMMB < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reserved_vram_mb must be ≥ 0"})
		return
	}
	if input.ReservedCPUCores != nil && *input.ReservedCPUCores < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reserved_cpu_cores must be ≥ 0"})
		return
	}
	if input.ReservedMemoryMB != nil && *input.ReservedMemoryMB < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reserved_memory_mb must be ≥ 0"})
		return
	}

	// Check cluster VRAM capacity if updating VRAM reservation
	if input.ReservedVRAMMB != nil {
		var clusterVRAM int64
		_ = h.db.GetContext(c.Request.Context(), &clusterVRAM,
			`SELECT COALESCE(SUM(total_vram_mb),0) FROM nodes WHERE status IN ('online','degraded')`)

		var orgID string
		_ = h.db.GetContext(c.Request.Context(), &orgID,
			`SELECT organization_id FROM projects WHERE id=$1`, id)

		var currentOrgReserved int64
		_ = h.db.GetContext(c.Request.Context(), &currentOrgReserved, `
			SELECT COALESCE(SUM(pr.reserved_vram_mb),0)
			FROM project_reservations pr
			JOIN projects p ON p.id = pr.project_id
			WHERE p.organization_id=$1 AND p.id != $2`, orgID, id)

		if currentOrgReserved+*input.ReservedVRAMMB > clusterVRAM {
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":             "total reserved_vram_mb would exceed cluster capacity",
				"cluster_vram_mb":   clusterVRAM,
				"current_reserved":  currentOrgReserved,
				"requested":         *input.ReservedVRAMMB,
			})
			return
		}
	}

	// Upsert
	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO project_reservations (project_id, reserved_vram_mb, reserved_cpu_cores, reserved_memory_mb, updated_at)
		VALUES ($1, COALESCE($2,0), COALESCE($3,0), COALESCE($4,0), NOW())
		ON CONFLICT (project_id) DO UPDATE SET
		  reserved_vram_mb   = COALESCE($2, project_reservations.reserved_vram_mb),
		  reserved_cpu_cores = COALESCE($3, project_reservations.reserved_cpu_cores),
		  reserved_memory_mb = COALESCE($4, project_reservations.reserved_memory_mb),
		  updated_at         = NOW()`,
		id, input.ReservedVRAMMB, input.ReservedCPUCores, input.ReservedMemoryMB,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "reservation updated", "project_id": id})
}

// ─── ChangePriority ───────────────────────────────────────────────────────────

// ChangePriority handles POST /admin/v1/projects/:id/priority
func (h *ProjectHandler) ChangePriority(c *gin.Context) {
	id := c.Param("id")

	var input struct {
		Priority string `json:"priority" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if !project.Priority(input.Priority).IsValid() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "priority must be one of: CRITICAL, HIGH, NORMAL, LOW, BEST_EFFORT"})
		return
	}

	var existing struct {
		OrganizationID string `db:"organization_id"`
		Priority       string `db:"priority"`
	}
	if err := h.db.GetContext(c.Request.Context(), &existing,
		`SELECT organization_id, priority FROM projects WHERE id=$1`, id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	// Idempotent: same priority → no-op
	if existing.Priority == input.Priority {
		c.JSON(http.StatusOK, gin.H{"message": "no change — priority is already " + input.Priority, "changed": false})
		return
	}

	_, err := h.db.ExecContext(c.Request.Context(),
		`UPDATE projects SET priority=$1, updated_at=NOW() WHERE id=$2`, input.Priority, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Audit log
	h.auditPriorityChange(c, id, existing.Priority, input.Priority)

	c.JSON(http.StatusOK, gin.H{
		"message":       "priority updated",
		"project_id":    id,
		"old_priority":  existing.Priority,
		"new_priority":  input.Priority,
		"changed":       true,
	})
}

// ─── SetProtection ────────────────────────────────────────────────────────────

// SetProtection handles PUT /admin/v1/projects/:id/protection
func (h *ProjectHandler) SetProtection(c *gin.Context) {
	id := c.Param("id")

	var exists int
	if err := h.db.GetContext(c.Request.Context(), &exists,
		`SELECT COUNT(*) FROM projects WHERE id=$1`, id); err != nil || exists == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	var input struct {
		AlwaysRunning   *bool   `json:"always_running"`
		Protected       *bool   `json:"protected"`
		MinimumReplicas *int    `json:"minimum_replicas"`
		AdmissionPolicy *string `json:"admission_policy"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if input.MinimumReplicas != nil && (*input.MinimumReplicas < 0 || *input.MinimumReplicas > 100) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "minimum_replicas must be between 0 and 100"})
		return
	}
	if input.AdmissionPolicy != nil {
		switch *input.AdmissionPolicy {
		case "queue", "preempt_then_queue", "reject":
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "admission_policy must be one of: queue, preempt_then_queue, reject"})
			return
		}
	}

	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO project_configurations (project_id, always_running, protected, minimum_replicas, admission_policy, updated_at)
		VALUES ($1,
		  COALESCE($2,FALSE), COALESCE($3,FALSE), COALESCE($4,0), COALESCE($5,'queue'), NOW())
		ON CONFLICT (project_id) DO UPDATE SET
		  always_running   = COALESCE($2, project_configurations.always_running),
		  protected        = COALESCE($3, project_configurations.protected),
		  minimum_replicas = COALESCE($4, project_configurations.minimum_replicas),
		  admission_policy = COALESCE($5, project_configurations.admission_policy),
		  updated_at       = NOW()`,
		id, input.AlwaysRunning, input.Protected, input.MinimumReplicas, input.AdmissionPolicy,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "protection settings updated", "project_id": id})
}

// ─── GetRuntimes ──────────────────────────────────────────────────────────────

// GetRuntimes handles GET /admin/v1/projects/:id/runtimes
func (h *ProjectHandler) GetRuntimes(c *gin.Context) {
	id := c.Param("id")
	var exists int
	if err := h.db.GetContext(c.Request.Context(), &exists,
		`SELECT COUNT(*) FROM projects WHERE id=$1`, id); err != nil || exists == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	type rtRow struct {
		ID            string     `db:"id"             json:"id"`
		ModelID       string     `db:"model_id"       json:"model_id"`
		State         string     `db:"state"          json:"state"`
		NodeID        string     `db:"node_id"        json:"node_id"`
		GPUIDs        []byte     `db:"gpu_ids"        json:"-"`
		GPUIDsStr     string     `json:"gpu_ids"`
		BindHost      string     `db:"bind_host"      json:"bind_host"`
		BindPort      int        `db:"bind_port"      json:"bind_port"`
		LastUsedAt    *time.Time `db:"last_used_at"   json:"last_used_at"`
		UpdatedAt     time.Time  `db:"updated_at"     json:"updated_at"`
	}
	var rows []rtRow
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT ar.id, COALESCE(ar.model_id::text,'') AS model_id,
		       ar.state, ar.node_id::text AS node_id,
		       ar.gpu_ids, ar.bind_host, ar.bind_port,
		       ar.last_used_at, ar.updated_at
		FROM agent_runtimes ar
		WHERE ar.project_id=$1
		  AND ar.state NOT IN ('deleted','archived')
		ORDER BY ar.updated_at DESC`, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if rows == nil {
		rows = []rtRow{}
	}
	for i := range rows {
		if rows[i].GPUIDs != nil {
			rows[i].GPUIDsStr = string(rows[i].GPUIDs)
		} else {
			rows[i].GPUIDsStr = "[]"
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": len(rows), "project_id": id})
}

// ─── GetUsage ─────────────────────────────────────────────────────────────────

// GetUsage handles GET /admin/v1/projects/:id/usage
func (h *ProjectHandler) GetUsage(c *gin.Context) {
	id := c.Param("id")
	var exists int
	if err := h.db.GetContext(c.Request.Context(), &exists,
		`SELECT COUNT(*) FROM projects WHERE id=$1`, id); err != nil || exists == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	from := c.Query("from")
	to := c.Query("to")
	if from == "" {
		from = time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339)
	}
	if to == "" {
		to = time.Now().UTC().Format(time.RFC3339)
	}

	breakdown := c.Query("breakdown")

	type usageSummary struct {
		TotalRequests    int64   `db:"total_requests"    json:"total_requests"`
		TotalTokens      int64   `db:"total_tokens"      json:"total_tokens"`
		PromptTokens     int64   `db:"prompt_tokens"     json:"prompt_tokens"`
		CompletionTokens int64   `db:"completion_tokens" json:"completion_tokens"`
		CostUSD          float64 `db:"cost_usd"          json:"cost_usd"`
		GPUTimeMs        int64   `db:"gpu_time_ms"       json:"gpu_time_ms"`
		AvgLatencyMs     float64 `db:"avg_latency_ms"    json:"avg_latency_ms"`
		ErrorCount       int64   `db:"error_count"       json:"error_count"`
	}

	var summary usageSummary
	_ = h.db.GetContext(c.Request.Context(), &summary, `
		SELECT COUNT(*)                                             AS total_requests,
		       COALESCE(SUM(total_tokens),0)                       AS total_tokens,
		       COALESCE(SUM(prompt_tokens),0)                      AS prompt_tokens,
		       COALESCE(SUM(completion_tokens),0)                  AS completion_tokens,
		       COALESCE(SUM(cost_usd),0)                           AS cost_usd,
		       COALESCE(SUM(gpu_time_ms),0)                        AS gpu_time_ms,
		       COALESCE(AVG(latency_ms),0)                         AS avg_latency_ms,
		       COUNT(*) FILTER (WHERE status != 'success')         AS error_count
		FROM usage_events
		WHERE project_id=$1 AND created_at BETWEEN $2::timestamptz AND $3::timestamptz`,
		id, from, to)

	// Point-in-time: current active runtime count
	var runtimeCount int
	_ = h.db.GetContext(c.Request.Context(), &runtimeCount,
		`SELECT COUNT(*) FROM agent_runtimes WHERE project_id=$1 AND state IN ('active','warm')`, id)

	// Preemption count in window
	var preemptionCount int
	_ = h.db.GetContext(c.Request.Context(), &preemptionCount, `
		SELECT COUNT(*) FROM preemption_events
		WHERE preempted_project_id=$1::text
		  AND preempted_runtime_id IS NOT NULL
		  AND created_at BETWEEN $2::timestamptz AND $3::timestamptz`,
		id, from, to)

	resp := gin.H{
		"project_id":        id,
		"from":              from,
		"to":                to,
		"total_requests":    summary.TotalRequests,
		"total_tokens":      summary.TotalTokens,
		"prompt_tokens":     summary.PromptTokens,
		"completion_tokens": summary.CompletionTokens,
		"cost_usd":          summary.CostUSD,
		"gpu_time_ms":       summary.GPUTimeMs,
		"avg_latency_ms":    summary.AvgLatencyMs,
		"error_count":       summary.ErrorCount,
		"runtime_count":     runtimeCount,
		"preemption_count":  preemptionCount,
	}

	// Per-model breakdown
	if breakdown == "model" {
		type modelRow struct {
			ModelName        string  `db:"model_name"        json:"model_name"`
			TotalRequests    int64   `db:"total_requests"    json:"total_requests"`
			TotalTokens      int64   `db:"total_tokens"      json:"total_tokens"`
			PromptTokens     int64   `db:"prompt_tokens"     json:"prompt_tokens"`
			CompletionTokens int64   `db:"completion_tokens" json:"completion_tokens"`
			CostUSD          float64 `db:"cost_usd"          json:"cost_usd"`
			GPUTimeMs        int64   `db:"gpu_time_ms"       json:"gpu_time_ms"`
			AvgLatencyMs     float64 `db:"avg_latency_ms"    json:"avg_latency_ms"`
			ErrorCount       int64   `db:"error_count"       json:"error_count"`
		}
		var breakdownRows []modelRow
		_ = h.db.SelectContext(c.Request.Context(), &breakdownRows, `
			SELECT model_name,
			       COUNT(*)                                     AS total_requests,
			       COALESCE(SUM(total_tokens),0)               AS total_tokens,
			       COALESCE(SUM(prompt_tokens),0)              AS prompt_tokens,
			       COALESCE(SUM(completion_tokens),0)          AS completion_tokens,
			       COALESCE(SUM(cost_usd),0)                   AS cost_usd,
			       COALESCE(SUM(gpu_time_ms),0)                AS gpu_time_ms,
			       COALESCE(AVG(latency_ms),0)                 AS avg_latency_ms,
			       COUNT(*) FILTER (WHERE status!='success')   AS error_count
			FROM usage_events
			WHERE project_id=$1
			  AND created_at BETWEEN $2::timestamptz AND $3::timestamptz
			GROUP BY model_name ORDER BY total_tokens DESC`, id, from, to)
		if breakdownRows == nil {
			breakdownRows = []modelRow{}
		}
		resp["breakdown"] = breakdownRows
	}

	c.JSON(http.StatusOK, resp)
}

// ─── GetPreemptions ──────────────────────────────────────────────────────────

// GetPreemptions handles GET /admin/v1/projects/:id/preemptions
func (h *ProjectHandler) GetPreemptions(c *gin.Context) {
	id := c.Param("id")
	var exists int
	if err := h.db.GetContext(c.Request.Context(), &exists,
		`SELECT COUNT(*) FROM projects WHERE id=$1`, id); err != nil || exists == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	limit := 50
	offset := 0
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			if n < 1 || n > 100 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be between 1 and 100"})
				return
			}
			limit = n
		}
	}
	if o := c.Query("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil && n >= 0 {
			offset = n
		}
	}

	type evtRow struct {
		ID                  string     `db:"id"                     json:"id"`
		NodeID              *string    `db:"node_id"                json:"node_id"`
		PreemptedRuntimeID  *string    `db:"preempted_runtime_id"   json:"preempted_runtime_id"`
		PreemptedProjectID  *string    `db:"preempted_project_id"   json:"preempted_project_id"`
		PreemptedPriority   *string    `db:"preempted_priority"     json:"preempted_priority"`
		RequestingRuntimeID *string    `db:"requesting_runtime_id"  json:"requesting_runtime_id"`
		RequestingProjectID *string    `db:"requesting_project_id"  json:"requesting_project_id"`
		RequestingPriority  *string    `db:"requesting_priority"    json:"requesting_priority"`
		Trigger             string     `db:"trigger"                json:"trigger"`
		CreatedAt           time.Time  `db:"created_at"             json:"created_at"`
	}
	var rows []evtRow
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT id,
		       node_id::text AS node_id,
		       preempted_runtime_id::text   AS preempted_runtime_id,
		       preempted_project_id::text   AS preempted_project_id,
		       preempted_priority,
		       requesting_runtime_id::text  AS requesting_runtime_id,
		       requesting_project_id::text  AS requesting_project_id,
		       requesting_priority,
		       trigger, created_at
		FROM preemption_events
		WHERE (preempted_project_id=$1 OR requesting_project_id=$1)
		  AND preempted_runtime_id IS NOT NULL
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3`, id, limit, offset); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if rows == nil {
		rows = []evtRow{}
	}
	c.JSON(http.StatusOK, gin.H{
		"data":       rows,
		"total":      len(rows),
		"limit":      limit,
		"offset":     offset,
		"project_id": id,
	})
}

// ─── GetQueue ─────────────────────────────────────────────────────────────────

// GetQueue handles GET /admin/v1/projects/:id/queue
func (h *ProjectHandler) GetQueue(c *gin.Context) {
	id := c.Param("id")
	var exists int
	if err := h.db.GetContext(c.Request.Context(), &exists,
		`SELECT COUNT(*) FROM projects WHERE id=$1`, id); err != nil || exists == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	limit := 50
	offset := 0
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	if o := c.Query("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil && n >= 0 {
			offset = n
		}
	}

	type qRow struct {
		ID              string     `db:"id"               json:"id"`
		PriorityScore   int        `db:"priority_score"   json:"priority_score"`
		AdmissionPolicy string     `db:"admission_policy" json:"admission_policy"`
		Status          string     `db:"status"           json:"status"`
		Attempts        int        `db:"attempts"         json:"attempts"`
		EnqueuedAt      time.Time  `db:"enqueued_at"      json:"enqueued_at"`
		ExpiresAt       *time.Time `db:"expires_at"       json:"expires_at"`
		ErrorMsg        string     `db:"error_msg"        json:"error_msg"`
	}
	var rows []qRow
	_ = h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT id, priority_score, admission_policy, status, attempts,
		       enqueued_at, expires_at, error_msg
		FROM deployment_queue
		WHERE project_id=$1 AND status IN ('pending','expired')
		ORDER BY priority_score DESC, enqueued_at ASC
		LIMIT $2 OFFSET $3`, id, limit, offset)
	if rows == nil {
		rows = []qRow{}
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": len(rows), "project_id": id})
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func (h *ProjectHandler) auditPriorityChange(c *gin.Context, projectID, oldPriority, newPriority string) {
	var orgID string
	_ = h.db.QueryRowContext(c.Request.Context(),
		`SELECT organization_id FROM projects WHERE id=$1`, projectID).Scan(&orgID)

	_, _ = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO audit_logs (org_id, action, resource, resource_id, metadata)
		VALUES ($1, 'project.priority_changed', 'project', $2::uuid,
		        jsonb_build_object('old_priority',$3,'new_priority',$4))`,
		orgID, projectID, oldPriority, newPriority,
	)
}

// isUniqueViolation detects PostgreSQL unique constraint errors.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return len(err.Error()) > 0 && (
		contains(err.Error(), "unique constraint") ||
		contains(err.Error(), "duplicate key") ||
		contains(err.Error(), "UNIQUE"))
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
