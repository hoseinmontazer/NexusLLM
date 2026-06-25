// Package handlers — project.go
// All priority fields use numeric priority_weight [0–1000].
// Legacy enum strings (CRITICAL/HIGH/NORMAL/LOW/BEST_EFFORT) are never accepted.
package handlers

import (
	"net/http"
	"strconv"
	"strings"
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
		PriorityWeight *int   `json:"priority_weight"` // 0–1000; default 500
		Status         string `json:"status"`
		Preemptible    *bool  `json:"preemptible"` // default true
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(input.Name) > 200 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name must be ≤ 200 characters"})
		return
	}
	if len(input.Description) > 1000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "description must be ≤ 1000 characters"})
		return
	}

	weight := int(project.DefaultWeight)
	if input.PriorityWeight != nil {
		weight = *input.PriorityWeight
		if !project.PriorityWeight(weight).IsValid() {
			c.JSON(http.StatusBadRequest, gin.H{"error": "priority_weight must be between 0 and 1000"})
			return
		}
	}

	preemptible := true
	if input.Preemptible != nil {
		preemptible = *input.Preemptible
	}

	if input.Status == "" {
		input.Status = "active"
	}
	switch input.Status {
	case "active", "inactive", "archived":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "status must be one of: active, inactive, archived"})
		return
	}

	var teamCount int
	if err := h.db.GetContext(c.Request.Context(), &teamCount,
		`SELECT COUNT(*) FROM teams WHERE id=$1 AND org_id=$2 AND active=TRUE`,
		input.TeamID, input.OrganizationID); err != nil || teamCount == 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "team not found or not active in the specified organization"})
		return
	}

	id := uuid.New().String()
	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO projects (id, organization_id, team_id, name, description, priority_weight, preemptible, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		id, input.OrganizationID, input.TeamID, input.Name, input.Description,
		weight, preemptible, input.Status,
	)
	if err != nil {
		if isMigration018Missing(err) {
			// Fall back: insert without migration-018 columns
			_, err = h.db.ExecContext(c.Request.Context(), `
				INSERT INTO projects (id, organization_id, team_id, name, description, priority, status)
				VALUES ($1,$2,$3,$4,$5,$6,$7)`,
				id, input.OrganizationID, input.TeamID, input.Name, input.Description,
				enumToWeight_reverse(weight), input.Status,
			)
		}
		if err != nil {
			if isUniqueViolation(err) {
				c.JSON(http.StatusConflict, gin.H{"error": "a project with this name already exists in the team"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	_, _ = h.db.ExecContext(c.Request.Context(),
		`INSERT INTO project_configurations (project_id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
	_, _ = h.db.ExecContext(c.Request.Context(),
		`INSERT INTO project_reservations (project_id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
	_, _ = h.db.ExecContext(c.Request.Context(),
		`INSERT INTO project_effective_priority (project_id, base_weight, effective_priority) VALUES ($1,$2,$2) ON CONFLICT DO NOTHING`,
		id, weight)

	c.JSON(http.StatusCreated, gin.H{
		"id":              id,
		"name":            input.Name,
		"priority_weight": weight,
		"priority_label":  project.PriorityWeight(weight).Label(),
		"status":          input.Status,
	})
}

// ─── List ─────────────────────────────────────────────────────────────────────

// ListProjects handles GET /admin/v1/projects
// Filter params: org_id, team_id, min_weight, max_weight, status
func (h *ProjectHandler) ListProjects(c *gin.Context) {
	type projectRow struct {
		ID                string    `db:"id"                json:"id"`
		OrganizationID    string    `db:"organization_id"   json:"organization_id"`
		TeamID            string    `db:"team_id"           json:"team_id"`
		Name              string    `db:"name"              json:"name"`
		Description       string    `db:"description"       json:"description"`
		PriorityWeight    int       `db:"priority_weight"   json:"priority_weight"`
		PriorityLabel     string    `db:"priority_label"    json:"priority_label"`
		EffectivePriority int       `db:"effective_priority" json:"effective_priority"`
		Preemptible       bool      `db:"preemptible"       json:"preemptible"`
		Status            string    `db:"status"            json:"status"`
		RuntimeCount      int       `db:"runtime_count"     json:"runtime_count"`
		ReservedVRAMMB    int64     `db:"reserved_vram_mb"  json:"reserved_vram_mb"`
		ReservedCPUCores  int       `db:"reserved_cpu_cores" json:"reserved_cpu_cores"`
		ReservedMemoryMB  int64     `db:"reserved_memory_mb" json:"reserved_memory_mb"`
		Protected         bool      `db:"protected"         json:"protected"`
		AlwaysRunning     bool      `db:"always_running"    json:"always_running"`
		CreatedAt         time.Time `db:"created_at"        json:"created_at"`
		UpdatedAt         time.Time `db:"updated_at"        json:"updated_at"`
	}

	q := `SELECT p.id, p.organization_id, p.team_id, p.name, p.description,
	             p.priority_weight,
	             CASE
	               WHEN p.priority_weight >= 950 THEN 'Emergency'
	               WHEN p.priority_weight >= 900 THEN 'Production Critical'
	               WHEN p.priority_weight >= 800 THEN 'Revenue Critical'
	               WHEN p.priority_weight >= 700 THEN 'Core Internal'
	               WHEN p.priority_weight >= 500 THEN 'Standard'
	               WHEN p.priority_weight >= 300 THEN 'Batch'
	               WHEN p.priority_weight >= 100 THEN 'Development'
	               WHEN p.priority_weight >= 50  THEN 'Playground'
	               ELSE 'Best Effort'
	             END AS priority_label,
	             COALESCE(ep.effective_priority, p.priority_weight) AS effective_priority,
	             p.preemptible, p.status, p.created_at, p.updated_at,
	             COUNT(ar.id) FILTER (WHERE ar.state IN ('active','warm')) AS runtime_count,
	             COALESCE(pr.reserved_vram_mb,0)   AS reserved_vram_mb,
	             COALESCE(pr.reserved_cpu_cores,0) AS reserved_cpu_cores,
	             COALESCE(pr.reserved_memory_mb,0) AS reserved_memory_mb,
	             COALESCE(pc.protected,FALSE)       AS protected,
	             COALESCE(pc.always_running,FALSE)  AS always_running
	      FROM projects p
	      LEFT JOIN agent_runtimes ar ON ar.project_id = p.id
	      LEFT JOIN project_reservations pr ON pr.project_id = p.id
	      LEFT JOIN project_configurations pc ON pc.project_id = p.id
	      LEFT JOIN project_effective_priority ep ON ep.project_id = p.id
	      WHERE 1=1`
	args := []interface{}{}
	idx := 1

	if v := c.Query("org_id"); v != "" {
		q += " AND p.organization_id = $" + strconv.Itoa(idx)
		args = append(args, v)
		idx++
	}
	if v := c.Query("team_id"); v != "" {
		q += " AND p.team_id = $" + strconv.Itoa(idx)
		args = append(args, v)
		idx++
	}
	if v := c.Query("min_weight"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			q += " AND p.priority_weight >= $" + strconv.Itoa(idx)
			args = append(args, n)
			idx++
		}
	}
	if v := c.Query("max_weight"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			q += " AND p.priority_weight <= $" + strconv.Itoa(idx)
			args = append(args, n)
			idx++
		}
	}
	if v := c.Query("status"); v != "" {
		q += " AND p.status = $" + strconv.Itoa(idx)
		args = append(args, v)
		idx++
	}

	q += " GROUP BY p.id, pr.reserved_vram_mb, pr.reserved_cpu_cores, pr.reserved_memory_mb," +
		" pc.protected, pc.always_running, ep.effective_priority" +
		" ORDER BY COALESCE(ep.effective_priority, p.priority_weight) DESC, p.name"

	var rows []projectRow
	if err := h.db.SelectContext(c.Request.Context(), &rows, q, args...); err != nil {
		// Migration 018 may not have been applied yet — fall back to the pre-weight schema.
		if isMigration018Missing(err) {
			legacy := h.listProjectsLegacy(c)
			c.JSON(http.StatusOK, gin.H{"data": legacy, "total": len(legacy)})
			return
		}
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
		ID                string    `db:"id"                  json:"id"`
		OrganizationID    string    `db:"organization_id"     json:"organization_id"`
		TeamID            string    `db:"team_id"             json:"team_id"`
		Name              string    `db:"name"                json:"name"`
		Description       string    `db:"description"         json:"description"`
		PriorityWeight    int       `db:"priority_weight"     json:"priority_weight"`
		PriorityLabel     string    `db:"priority_label"      json:"priority_label"`
		EffectivePriority int       `db:"effective_priority"  json:"effective_priority"`
		WaitingBonus      int       `db:"waiting_bonus"       json:"waiting_bonus"`
		ReservationBonus  int       `db:"reservation_bonus"   json:"reservation_bonus"`
		ResourcePenalty   int       `db:"resource_penalty"    json:"resource_penalty"`
		Preemptible       bool      `db:"preemptible"         json:"preemptible"`
		Status            string    `db:"status"              json:"status"`
		CreatedAt         time.Time `db:"created_at"          json:"created_at"`
		UpdatedAt         time.Time `db:"updated_at"          json:"updated_at"`
		ReservedVRAMMB    int64     `db:"reserved_vram_mb"    json:"reserved_vram_mb"`
		ReservedCPUCores  int       `db:"reserved_cpu_cores"  json:"reserved_cpu_cores"`
		ReservedMemoryMB  int64     `db:"reserved_memory_mb"  json:"reserved_memory_mb"`
		MaxGPUVRAMMB      int64     `db:"max_gpu_vram_mb"     json:"max_gpu_vram_mb"`
		MaxCPU            int       `db:"max_cpu"             json:"max_cpu"`
		MaxMemoryMB       int64     `db:"max_memory_mb"       json:"max_memory_mb"`
		AlwaysRunning     bool      `db:"always_running"      json:"always_running"`
		Protected         bool      `db:"protected"           json:"protected"`
		MinReplicas       int       `db:"minimum_replicas"    json:"minimum_replicas"`
		AdmissionPolicy   string    `db:"admission_policy"    json:"admission_policy"`
	}
	var row fullRow
	err := h.db.GetContext(c.Request.Context(), &row, `
		SELECT p.id, p.organization_id, p.team_id, p.name, p.description,
		       p.priority_weight,
		       CASE
		         WHEN p.priority_weight >= 950 THEN 'Emergency'
		         WHEN p.priority_weight >= 900 THEN 'Production Critical'
		         WHEN p.priority_weight >= 800 THEN 'Revenue Critical'
		         WHEN p.priority_weight >= 700 THEN 'Core Internal'
		         WHEN p.priority_weight >= 500 THEN 'Standard'
		         WHEN p.priority_weight >= 300 THEN 'Batch'
		         WHEN p.priority_weight >= 100 THEN 'Development'
		         WHEN p.priority_weight >= 50  THEN 'Playground'
		         ELSE 'Best Effort'
		       END AS priority_label,
		       COALESCE(ep.effective_priority, p.priority_weight)  AS effective_priority,
		       COALESCE(ep.waiting_bonus, 0)      AS waiting_bonus,
		       COALESCE(ep.reservation_bonus, 0)  AS reservation_bonus,
		       COALESCE(ep.resource_penalty, 0)   AS resource_penalty,
		       p.preemptible, p.status, p.created_at, p.updated_at,
		       COALESCE(pr.reserved_vram_mb,0)    AS reserved_vram_mb,
		       COALESCE(pr.reserved_cpu_cores,0)  AS reserved_cpu_cores,
		       COALESCE(pr.reserved_memory_mb,0)  AS reserved_memory_mb,
		       COALESCE(pr.max_gpu_vram_mb,0)     AS max_gpu_vram_mb,
		       COALESCE(pr.max_cpu,0)             AS max_cpu,
		       COALESCE(pr.max_memory_mb,0)       AS max_memory_mb,
		       COALESCE(pc.always_running,FALSE)  AS always_running,
		       COALESCE(pc.protected,FALSE)       AS protected,
		       COALESCE(pc.minimum_replicas,0)    AS minimum_replicas,
		       COALESCE(pc.admission_policy,'queue') AS admission_policy
		FROM projects p
		LEFT JOIN project_reservations pr ON pr.project_id = p.id
		LEFT JOIN project_configurations pc ON pc.project_id = p.id
		LEFT JOIN project_effective_priority ep ON ep.project_id = p.id
		WHERE p.id = $1`, id)
	if err != nil {
		if isMigration018Missing(err) {
			// Fall back to pre-018 schema
			h.getProjectLegacy(c, id)
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}
	c.JSON(http.StatusOK, row)
}

// ─── Update ───────────────────────────────────────────────────────────────────

func (h *ProjectHandler) UpdateProject(c *gin.Context) {
	id := c.Param("id")
	var existing struct {
		Name           string `db:"name"`
		PriorityWeight int    `db:"priority_weight"`
	}
	// Migration-018-aware: try new column, fall back to enum
	err := h.db.GetContext(c.Request.Context(), &existing,
		`SELECT name, priority_weight FROM projects WHERE id=$1`, id)
	if err != nil {
		if isMigration018Missing(err) {
			var legRow struct {
				Name     string `db:"name"`
				Priority string `db:"priority"`
			}
			if err2 := h.db.GetContext(c.Request.Context(), &legRow,
				`SELECT name, priority FROM projects WHERE id=$1`, id); err2 != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
				return
			}
			existing.Name = legRow.Name
			existing.PriorityWeight = enumToWeight(legRow.Priority)
		} else {
			c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
			return
		}
	}
	var input struct {
		Name           *string `json:"name"`
		Description    *string `json:"description"`
		PriorityWeight *int    `json:"priority_weight"`
		Preemptible    *bool   `json:"preemptible"`
		Status         *string `json:"status"`
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
	if input.PriorityWeight != nil && !project.PriorityWeight(*input.PriorityWeight).IsValid() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "priority_weight must be between 0 and 1000"})
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
	_, err = h.db.ExecContext(c.Request.Context(), `
		UPDATE projects SET
		  name            = COALESCE($2, name),
		  description     = COALESCE($3, description),
		  priority_weight = COALESCE($4, priority_weight),
		  preemptible     = COALESCE($5, preemptible),
		  status          = COALESCE($6, status),
		  updated_at      = NOW()
		WHERE id = $1`,
		id, input.Name, input.Description, input.PriorityWeight, input.Preemptible, input.Status)
	if err != nil {
		if isMigration018Missing(err) {
			// Fall back: update only the columns that exist before migration 018
			var legacyPriority interface{}
			if input.PriorityWeight != nil {
				legacyPriority = enumToWeight_reverse(*input.PriorityWeight)
			}
			_, err = h.db.ExecContext(c.Request.Context(), `
				UPDATE projects SET
				  name        = COALESCE($2, name),
				  description = COALESCE($3, description),
				  priority    = COALESCE($4, priority),
				  status      = COALESCE($5, status),
				  updated_at  = NOW()
				WHERE id = $1`,
				id, input.Name, input.Description, legacyPriority, input.Status)
		}
		if err != nil {
			if isUniqueViolation(err) {
				c.JSON(http.StatusConflict, gin.H{"error": "a project with this name already exists in the team"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	if input.PriorityWeight != nil && *input.PriorityWeight != existing.PriorityWeight {
		h.auditPriorityChange(c, id, existing.PriorityWeight, *input.PriorityWeight)
		// Reset effective priority cache
		_, _ = h.db.ExecContext(c.Request.Context(), `
			UPDATE project_effective_priority
			SET base_weight=GREATEST(0,LEAST(1000,$1)),
			    effective_priority=GREATEST(0,LEAST(1000,$1+waiting_bonus+reservation_bonus-resource_penalty)),
			    last_computed_at=NOW()
			WHERE project_id=$2`, *input.PriorityWeight, id)
	}
	c.JSON(http.StatusOK, gin.H{"message": "project updated", "id": id})
}

// ─── Delete ───────────────────────────────────────────────────────────────────

func (h *ProjectHandler) DeleteProject(c *gin.Context) {
	id := c.Param("id")
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
	n, _ := res.RowsAffected()
	if n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "project deleted", "id": id})
}

// ─── ChangePriority ───────────────────────────────────────────────────────────

// ChangePriority handles POST /admin/v1/projects/:id/priority
// Body: {"priority_weight": 800}
func (h *ProjectHandler) ChangePriority(c *gin.Context) {
	id := c.Param("id")
	var input struct {
		PriorityWeight int `json:"priority_weight" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !project.PriorityWeight(input.PriorityWeight).IsValid() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "priority_weight must be between 0 and 1000"})
		return
	}

	// ── Read current priority (migration-018-aware) ────────────────────────
	// Try the new column first; fall back to the old enum if it doesn't exist.
	var existing struct {
		OrganizationID string `db:"organization_id"`
		PriorityWeight int    `db:"priority_weight"`
	}
	err := h.db.GetContext(c.Request.Context(), &existing,
		`SELECT organization_id, priority_weight FROM projects WHERE id=$1`, id)
	if err != nil {
		if isMigration018Missing(err) {
			// Migration 018 not applied — read old enum and map to weight
			var legacyRow struct {
				OrganizationID string `db:"organization_id"`
				Priority       string `db:"priority"`
			}
			if err2 := h.db.GetContext(c.Request.Context(), &legacyRow,
				`SELECT organization_id, priority FROM projects WHERE id=$1`, id); err2 != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
				return
			}
			existing.OrganizationID = legacyRow.OrganizationID
			existing.PriorityWeight = enumToWeight(legacyRow.Priority)
		} else {
			c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
			return
		}
	}

	if existing.PriorityWeight == input.PriorityWeight {
		c.JSON(http.StatusOK, gin.H{
			"message":         "no change — priority_weight is already " + strconv.Itoa(input.PriorityWeight),
			"changed":         false,
			"priority_weight": input.PriorityWeight,
			"priority_label":  project.PriorityWeight(input.PriorityWeight).Label(),
		})
		return
	}

	// ── Update priority (migration-018-aware) ──────────────────────────────
	// Try updating priority_weight; if the column doesn't exist yet, update the
	// legacy priority string column instead so the change is still persisted.
	legacyStr := enumToWeight_reverse(input.PriorityWeight)
	_, updateErr := h.db.ExecContext(c.Request.Context(),
		`UPDATE projects SET priority_weight=$1, updated_at=NOW() WHERE id=$2`,
		input.PriorityWeight, id)
	if updateErr != nil && isMigration018Missing(updateErr) {
		// Fall back: write the closest legacy enum value
		_, _ = h.db.ExecContext(c.Request.Context(),
			`UPDATE projects SET priority=$1, updated_at=NOW() WHERE id=$2`,
			legacyStr, id)
	} else if updateErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": updateErr.Error()})
		return
	}

	// Recalculate effective priority (only if table exists)
	_, _ = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO project_effective_priority (project_id, base_weight, effective_priority, last_computed_at)
		VALUES ($1,$2,$2,NOW())
		ON CONFLICT (project_id) DO UPDATE SET
		  base_weight        = $2,
		  effective_priority = LEAST(1000, GREATEST(0,
		      $2 + project_effective_priority.waiting_bonus
		         + project_effective_priority.reservation_bonus
		         - project_effective_priority.resource_penalty
		  )),
		  last_computed_at   = NOW()`, id, input.PriorityWeight)

	h.auditPriorityChange(c, id, existing.PriorityWeight, input.PriorityWeight)

	c.JSON(http.StatusOK, gin.H{
		"message":             "priority updated",
		"project_id":          id,
		"old_priority_weight": existing.PriorityWeight,
		"new_priority_weight": input.PriorityWeight,
		"new_priority_label":  project.PriorityWeight(input.PriorityWeight).Label(),
		"changed":             true,
	})
}

// ─── Reserve ──────────────────────────────────────────────────────────────────

func (h *ProjectHandler) Reserve(c *gin.Context) {
	id := c.Param("id")
	var exists int
	if err := h.db.GetContext(c.Request.Context(), &exists, `SELECT COUNT(*) FROM projects WHERE id=$1`, id); err != nil || exists == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}
	var input struct {
		ReservedVRAMMB   *int64 `json:"reserved_vram_mb"`
		ReservedCPUCores *int   `json:"reserved_cpu_cores"`
		ReservedMemoryMB *int64 `json:"reserved_memory_mb"`
		MaxGPUVRAMMB     *int64 `json:"max_gpu_vram_mb"`
		MaxCPU           *int   `json:"max_cpu"`
		MaxMemoryMB      *int64 `json:"max_memory_mb"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
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
	if input.MaxGPUVRAMMB != nil && *input.MaxGPUVRAMMB < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "max_gpu_vram_mb must be ≥ 0"})
		return
	}

	if input.ReservedVRAMMB != nil {
		var clusterVRAM int64
		_ = h.db.GetContext(c.Request.Context(), &clusterVRAM,
			`SELECT COALESCE(SUM(total_vram_mb),0) FROM nodes WHERE status IN ('online','degraded')`)
		var orgID string
		_ = h.db.GetContext(c.Request.Context(), &orgID, `SELECT organization_id FROM projects WHERE id=$1`, id)
		var currentOrgReserved int64
		_ = h.db.GetContext(c.Request.Context(), &currentOrgReserved, `
			SELECT COALESCE(SUM(pr.reserved_vram_mb),0)
			FROM project_reservations pr
			JOIN projects p ON p.id = pr.project_id
			WHERE p.organization_id=$1 AND p.id != $2`, orgID, id)
		if currentOrgReserved+*input.ReservedVRAMMB > clusterVRAM {
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":            "total reserved_vram_mb would exceed cluster capacity",
				"cluster_vram_mb":  clusterVRAM,
				"current_reserved": currentOrgReserved,
				"requested":        *input.ReservedVRAMMB,
			})
			return
		}
	}

	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO project_reservations
		  (project_id, reserved_vram_mb, reserved_cpu_cores, reserved_memory_mb,
		   max_gpu_vram_mb, max_cpu, max_memory_mb, updated_at)
		VALUES ($1,COALESCE($2,0),COALESCE($3,0),COALESCE($4,0),COALESCE($5,0),COALESCE($6,0),COALESCE($7,0),NOW())
		ON CONFLICT (project_id) DO UPDATE SET
		  reserved_vram_mb   = COALESCE($2, project_reservations.reserved_vram_mb),
		  reserved_cpu_cores = COALESCE($3, project_reservations.reserved_cpu_cores),
		  reserved_memory_mb = COALESCE($4, project_reservations.reserved_memory_mb),
		  max_gpu_vram_mb    = COALESCE($5, project_reservations.max_gpu_vram_mb),
		  max_cpu            = COALESCE($6, project_reservations.max_cpu),
		  max_memory_mb      = COALESCE($7, project_reservations.max_memory_mb),
		  updated_at         = NOW()`,
		id, input.ReservedVRAMMB, input.ReservedCPUCores, input.ReservedMemoryMB,
		input.MaxGPUVRAMMB, input.MaxCPU, input.MaxMemoryMB,
	)
	if err != nil {
		if isMigration018Missing(err) {
			// Fall back: upsert without max_* quota columns (migration 018 not applied)
			_, err = h.db.ExecContext(c.Request.Context(), `
				INSERT INTO project_reservations
				  (project_id, reserved_vram_mb, reserved_cpu_cores, reserved_memory_mb, updated_at)
				VALUES ($1,COALESCE($2,0),COALESCE($3,0),COALESCE($4,0),NOW())
				ON CONFLICT (project_id) DO UPDATE SET
				  reserved_vram_mb   = COALESCE($2, project_reservations.reserved_vram_mb),
				  reserved_cpu_cores = COALESCE($3, project_reservations.reserved_cpu_cores),
				  reserved_memory_mb = COALESCE($4, project_reservations.reserved_memory_mb),
				  updated_at         = NOW()`,
				id, input.ReservedVRAMMB, input.ReservedCPUCores, input.ReservedMemoryMB,
			)
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"message": "reservation updated", "project_id": id})
}

// ─── SetProtection ────────────────────────────────────────────────────────────

func (h *ProjectHandler) SetProtection(c *gin.Context) {
	id := c.Param("id")
	var exists int
	if err := h.db.GetContext(c.Request.Context(), &exists, `SELECT COUNT(*) FROM projects WHERE id=$1`, id); err != nil || exists == 0 {
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
		INSERT INTO project_configurations
		  (project_id, always_running, protected, minimum_replicas, admission_policy, updated_at)
		VALUES ($1,COALESCE($2,FALSE),COALESCE($3,FALSE),COALESCE($4,0),COALESCE($5,'queue'),NOW())
		ON CONFLICT (project_id) DO UPDATE SET
		  always_running   = COALESCE($2, project_configurations.always_running),
		  protected        = COALESCE($3, project_configurations.protected),
		  minimum_replicas = COALESCE($4, project_configurations.minimum_replicas),
		  admission_policy = COALESCE($5, project_configurations.admission_policy),
		  updated_at       = NOW()`,
		id, input.AlwaysRunning, input.Protected, input.MinimumReplicas, input.AdmissionPolicy)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "protection settings updated", "project_id": id})
}

// ─── GetPriorityPresets ───────────────────────────────────────────────────────

// GetPriorityPresets handles GET /admin/v1/scheduler/priority-presets
// Returns the standard preset table for the UI priority picker.
func (h *ProjectHandler) GetPriorityPresets(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"presets": project.StandardPresets})
}

// ─── GetRuntimes ──────────────────────────────────────────────────────────────

func (h *ProjectHandler) GetRuntimes(c *gin.Context) {
	id := c.Param("id")
	var exists int
	if err := h.db.GetContext(c.Request.Context(), &exists, `SELECT COUNT(*) FROM projects WHERE id=$1`, id); err != nil || exists == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}
	type rtRow struct {
		ID         string     `db:"id"          json:"id"`
		ModelID    string     `db:"model_id"    json:"model_id"`
		State      string     `db:"state"       json:"state"`
		NodeID     string     `db:"node_id"     json:"node_id"`
		GPUIDs     string     `json:"gpu_ids"`
		GPUIDsRaw  []byte     `db:"gpu_ids"     json:"-"`
		BindHost   string     `db:"bind_host"   json:"bind_host"`
		BindPort   int        `db:"bind_port"   json:"bind_port"`
		LastUsedAt *time.Time `db:"last_used_at" json:"last_used_at"`
		UpdatedAt  time.Time  `db:"updated_at"  json:"updated_at"`
	}
	var rows []rtRow
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT ar.id, COALESCE(ar.model_id::text,'') AS model_id,
		       ar.state, ar.node_id::text AS node_id,
		       ar.gpu_ids, ar.bind_host, ar.bind_port,
		       ar.last_used_at, ar.updated_at
		FROM agent_runtimes ar
		WHERE ar.project_id=$1 AND ar.state NOT IN ('deleted','archived')
		ORDER BY ar.updated_at DESC`, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if rows == nil {
		rows = []rtRow{}
	}
	for i := range rows {
		if rows[i].GPUIDsRaw != nil {
			rows[i].GPUIDs = string(rows[i].GPUIDsRaw)
		} else {
			rows[i].GPUIDs = "[]"
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": len(rows), "project_id": id})
}

// ─── GetQueue ─────────────────────────────────────────────────────────────────

// GetQueue handles GET /admin/v1/projects/:id/queue
// Returns pending deployments with effective_priority and wait time.
func (h *ProjectHandler) GetQueue(c *gin.Context) {
	id := c.Param("id")
	var exists int
	if err := h.db.GetContext(c.Request.Context(), &exists, `SELECT COUNT(*) FROM projects WHERE id=$1`, id); err != nil || exists == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}
	limit, offset := 50, 0
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
		ID                string     `db:"id"                  json:"id"`
		ModelName         string     `db:"model_name"          json:"model_name"`
		PriorityWeight    int        `db:"priority_weight"     json:"priority_weight"`
		EffectivePriority int        `db:"effective_priority"  json:"effective_priority"`
		AdmissionPolicy   string     `db:"admission_policy"    json:"admission_policy"`
		Status            string     `db:"status"              json:"status"`
		Attempts          int        `db:"attempts"            json:"attempts"`
		WaitingSince      time.Time  `db:"waiting_since"       json:"waiting_since"`
		RequiredVRAMMB    int64      `db:"required_vram_mb"    json:"required_vram_mb"`
		RequiredRAMMB     int64      `db:"required_ram_mb"     json:"required_ram_mb"`
		RequiredCPU       int        `db:"required_cpu"        json:"required_cpu"`
		PreemptionReason  string     `db:"preemption_reason"   json:"preemption_reason"`
		ErrorMsg          string     `db:"error_msg"           json:"error_msg"`
		EnqueuedAt        time.Time  `db:"enqueued_at"         json:"enqueued_at"`
		ExpiresAt         *time.Time `db:"expires_at"          json:"expires_at"`
	}
	var rows []qRow
	_ = h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT id, COALESCE(model_name,'') AS model_name,
		       COALESCE(priority_weight, priority_score) AS priority_weight,
		       COALESCE(effective_priority, priority_score) AS effective_priority,
		       admission_policy, status, attempts,
		       COALESCE(waiting_since, enqueued_at) AS waiting_since,
		       COALESCE(required_vram_mb, 0) AS required_vram_mb,
		       COALESCE(required_ram_mb, 0) AS required_ram_mb,
		       COALESCE(required_cpu, 0) AS required_cpu,
		       COALESCE(preemption_reason,'') AS preemption_reason,
		       error_msg, enqueued_at, expires_at
		FROM deployment_queue
		WHERE project_id=$1 AND status IN ('pending','expired')
		ORDER BY effective_priority DESC, waiting_since ASC
		LIMIT $2 OFFSET $3`, id, limit, offset)
	if rows == nil {
		rows = []qRow{}
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": len(rows), "project_id": id})
}

// ─── GetUsage ─────────────────────────────────────────────────────────────────

func (h *ProjectHandler) GetUsage(c *gin.Context) {
	id := c.Param("id")
	var exists int
	if err := h.db.GetContext(c.Request.Context(), &exists, `SELECT COUNT(*) FROM projects WHERE id=$1`, id); err != nil || exists == 0 {
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
		SELECT COUNT(*)                                          AS total_requests,
		       COALESCE(SUM(total_tokens),0)                    AS total_tokens,
		       COALESCE(SUM(prompt_tokens),0)                   AS prompt_tokens,
		       COALESCE(SUM(completion_tokens),0)               AS completion_tokens,
		       COALESCE(SUM(cost_usd),0)                        AS cost_usd,
		       COALESCE(SUM(gpu_time_ms),0)                     AS gpu_time_ms,
		       COALESCE(AVG(latency_ms),0)                      AS avg_latency_ms,
		       COUNT(*) FILTER (WHERE status != 'success')      AS error_count
		FROM usage_events
		WHERE project_id=$1 AND created_at BETWEEN $2::timestamptz AND $3::timestamptz`,
		id, from, to)
	var runtimeCount, preemptionCount int
	_ = h.db.GetContext(c.Request.Context(), &runtimeCount,
		`SELECT COUNT(*) FROM agent_runtimes WHERE project_id=$1 AND state IN ('active','warm')`, id)
	_ = h.db.GetContext(c.Request.Context(), &preemptionCount, `
		SELECT COUNT(*) FROM preemption_events
		WHERE preempted_project_id=$1::text AND preempted_runtime_id IS NOT NULL
		  AND created_at BETWEEN $2::timestamptz AND $3::timestamptz`, id, from, to)
	c.JSON(http.StatusOK, gin.H{
		"project_id": id, "from": from, "to": to,
		"total_requests": summary.TotalRequests, "total_tokens": summary.TotalTokens,
		"prompt_tokens": summary.PromptTokens, "completion_tokens": summary.CompletionTokens,
		"cost_usd": summary.CostUSD, "gpu_time_ms": summary.GPUTimeMs,
		"avg_latency_ms": summary.AvgLatencyMs, "error_count": summary.ErrorCount,
		"runtime_count": runtimeCount, "preemption_count": preemptionCount,
	})
}

// ─── GetPreemptions ──────────────────────────────────────────────────────────

func (h *ProjectHandler) GetPreemptions(c *gin.Context) {
	id := c.Param("id")
	var exists int
	if err := h.db.GetContext(c.Request.Context(), &exists, `SELECT COUNT(*) FROM projects WHERE id=$1`, id); err != nil || exists == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}
	limit, offset := 50, 0
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n >= 1 && n <= 100 {
			limit = n
		}
	}
	if o := c.Query("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil && n >= 0 {
			offset = n
		}
	}
	type evtRow struct {
		ID                  string    `db:"id"                     json:"id"`
		NodeID              *string   `db:"node_id"                json:"node_id"`
		PreemptedRuntimeID  *string   `db:"preempted_runtime_id"   json:"preempted_runtime_id"`
		PreemptedProjectID  *string   `db:"preempted_project_id"   json:"preempted_project_id"`
		PreemptedWeight     *int      `db:"preempted_weight"       json:"preempted_weight"`
		RequestingRuntimeID *string   `db:"requesting_runtime_id"  json:"requesting_runtime_id"`
		RequestingProjectID *string   `db:"requesting_project_id"  json:"requesting_project_id"`
		RequestingWeight    *int      `db:"requesting_weight"      json:"requesting_weight"`
		Trigger             string    `db:"trigger"                json:"trigger"`
		CreatedAt           time.Time `db:"created_at"             json:"created_at"`
	}
	var rows []evtRow
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT id, node_id::text AS node_id,
		       preempted_runtime_id::text  AS preempted_runtime_id,
		       preempted_project_id::text  AS preempted_project_id,
		       preempted_weight,
		       requesting_runtime_id::text AS requesting_runtime_id,
		       requesting_project_id::text AS requesting_project_id,
		       requesting_weight,
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
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": len(rows), "limit": limit, "offset": offset, "project_id": id})
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func (h *ProjectHandler) auditPriorityChange(c *gin.Context, projectID string, oldWeight, newWeight int) {
	var orgID string
	_ = h.db.QueryRowContext(c.Request.Context(),
		`SELECT organization_id FROM projects WHERE id=$1`, projectID).Scan(&orgID)
	_, _ = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO audit_logs (org_id, action, resource, resource_id, metadata)
		VALUES ($1,'project.priority_changed','project',$2::uuid,
		        jsonb_build_object('old_weight',$3,'new_weight',$4,
		                          'old_label',$5,'new_label',$6))`,
		orgID, projectID, oldWeight, newWeight,
		project.PriorityWeight(oldWeight).Label(),
		project.PriorityWeight(newWeight).Label(),
	)
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "unique constraint") ||
		strings.Contains(s, "duplicate key") ||
		strings.Contains(s, "UNIQUE")
}

// ─── SchedulerQueue ──────────────────────────────────────────────────────────

// SchedulerQueue handles GET /admin/v1/scheduler/queue
// Returns the full cluster deployment queue ordered by effective_priority DESC.
func (h *ProjectHandler) SchedulerQueue(c *gin.Context) {
	limit, offset := 100, 0
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	if o := c.Query("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil && n >= 0 {
			offset = n
		}
	}

	type qRow struct {
		ID                string    `db:"id"                  json:"id"`
		ProjectID         *string   `db:"project_id"          json:"project_id"`
		ModelName         string    `db:"model_name"          json:"model_name"`
		PriorityWeight    int       `db:"priority_weight"     json:"priority_weight"`
		EffectivePriority int       `db:"effective_priority"  json:"effective_priority"`
		AdmissionPolicy   string    `db:"admission_policy"    json:"admission_policy"`
		Status            string    `db:"status"              json:"status"`
		Attempts          int       `db:"attempts"            json:"attempts"`
		WaitingSince      time.Time `db:"waiting_since"       json:"waiting_since"`
		RequiredVRAMMB    int64     `db:"required_vram_mb"    json:"required_vram_mb"`
		RequiredRAMMB     int64     `db:"required_ram_mb"     json:"required_ram_mb"`
		RequiredCPU       int       `db:"required_cpu"        json:"required_cpu"`
		PreemptionReason  string    `db:"preemption_reason"   json:"preemption_reason"`
		ErrorMsg          string    `db:"error_msg"           json:"error_msg"`
		EnqueuedAt        time.Time `db:"enqueued_at"         json:"enqueued_at"`
	}
	var rows []qRow
	_ = h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT dq.id,
		       dq.project_id::text AS project_id,
		       COALESCE(dq.model_name,'') AS model_name,
		       COALESCE(dq.priority_weight, dq.priority_score) AS priority_weight,
		       COALESCE(dq.effective_priority, dq.priority_score) AS effective_priority,
		       dq.admission_policy, dq.status, dq.attempts,
		       COALESCE(dq.waiting_since, dq.enqueued_at) AS waiting_since,
		       COALESCE(dq.required_vram_mb,0) AS required_vram_mb,
		       COALESCE(dq.required_ram_mb,0) AS required_ram_mb,
		       COALESCE(dq.required_cpu,0) AS required_cpu,
		       COALESCE(dq.preemption_reason,'') AS preemption_reason,
		       dq.error_msg, dq.enqueued_at
		FROM deployment_queue dq
		WHERE dq.status = 'pending'
		ORDER BY dq.effective_priority DESC, dq.waiting_since ASC
		LIMIT $1 OFFSET $2`, limit, offset)
	if rows == nil {
		rows = []qRow{}
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": len(rows)})
}

// ─── SchedulerDecisions ───────────────────────────────────────────────────────

// SchedulerDecisions handles GET /admin/v1/scheduler/decisions
func (h *ProjectHandler) SchedulerDecisions(c *gin.Context) {
	limit := 50
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	type decRow struct {
		ID                string     `db:"id"                  json:"id"`
		ModelID           string     `db:"model_id"            json:"model_id"`
		ModelName         string     `db:"model_name"          json:"model_name"`
		ProjectID         *string    `db:"project_id"          json:"project_id"`
		NodeID            *string    `db:"node_id"             json:"node_id"`
		DecisionType      string     `db:"decision_type"       json:"decision_type"`
		PriorityWeight    int        `db:"priority_weight"     json:"priority_weight"`
		EffectivePriority int        `db:"effective_priority"  json:"effective_priority"`
		WaitingBonus      int        `db:"waiting_bonus"       json:"waiting_bonus"`
		ReservationBonus  int        `db:"reservation_bonus"   json:"reservation_bonus"`
		ResourcePenalty   int        `db:"resource_penalty"    json:"resource_penalty"`
		NodeScore         float64    `db:"node_score"          json:"node_score"`
		Reason            string     `db:"reason"              json:"reason"`
		Outcome           string     `db:"outcome"             json:"outcome"`
		ErrorMsg          string     `db:"error_msg"           json:"error_msg"`
		DecidedAt         time.Time  `db:"decided_at"          json:"decided_at"`
		CompletedAt       *time.Time `db:"completed_at"        json:"completed_at"`
	}

	q := `SELECT id, model_id::text AS model_id, model_name,
	             project_id::text AS project_id, node_id::text AS node_id,
	             decision_type, priority_weight, effective_priority,
	             waiting_bonus, reservation_bonus, resource_penalty,
	             node_score, reason, outcome, error_msg, decided_at, completed_at
	      FROM scheduler_decisions
	      WHERE 1=1`
	args := []interface{}{}
	idx := 1
	if v := c.Query("model_id"); v != "" {
		q += " AND model_id = $" + strconv.Itoa(idx)
		args = append(args, v)
		idx++
	}
	if v := c.Query("project_id"); v != "" {
		q += " AND project_id = $" + strconv.Itoa(idx)
		args = append(args, v)
		idx++
	}
	q += " ORDER BY decided_at DESC LIMIT $" + strconv.Itoa(idx)
	args = append(args, limit)

	var rows []decRow
	_ = h.db.SelectContext(c.Request.Context(), &rows, q, args...)
	if rows == nil {
		rows = []decRow{}
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "total": len(rows)})
}

// ─── Migration-018 fallback ───────────────────────────────────────────────────

// isMigration018Missing returns true when the error indicates that the
// priority_weight, preemptible, or project_effective_priority objects are missing
// (i.e. migration 018 has not yet been applied to this database).
func isMigration018Missing(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "priority_weight") ||
		strings.Contains(s, "preemptible") ||
		strings.Contains(s, "project_effective_priority") ||
		(strings.Contains(s, "column") && strings.Contains(s, "does not exist")) ||
		strings.Contains(s, "42703") || // PostgreSQL: undefined_column
		strings.Contains(s, "42P01") // PostgreSQL: undefined_table
}

// listProjectsLegacy is used when migration 018 has not yet been applied.
// It queries only the columns that exist in migration 011, then maps the old
// enum priority to a numeric weight so the response shape is consistent.
func (h *ProjectHandler) listProjectsLegacy(c *gin.Context) []projectRowLegacy {
	type legacyRow struct {
		ID             string    `db:"id"             json:"id"`
		OrganizationID string    `db:"organization_id" json:"organization_id"`
		TeamID         string    `db:"team_id"        json:"team_id"`
		Name           string    `db:"name"           json:"name"`
		Description    string    `db:"description"    json:"description"`
		Priority       string    `db:"priority"       json:"-"`
		Status         string    `db:"status"         json:"status"`
		RuntimeCount   int       `db:"runtime_count"  json:"runtime_count"`
		ReservedVRAMMB int64     `db:"reserved_vram_mb" json:"reserved_vram_mb"`
		CreatedAt      time.Time `db:"created_at"     json:"created_at"`
		UpdatedAt      time.Time `db:"updated_at"     json:"updated_at"`
	}
	var legacyRows []legacyRow
	_ = h.db.SelectContext(c.Request.Context(), &legacyRows, `
		SELECT p.id, p.organization_id, p.team_id, p.name, p.description,
		       p.priority, p.status, p.created_at, p.updated_at,
		       COUNT(ar.id) FILTER (WHERE ar.state IN ('active','warm')) AS runtime_count,
		       COALESCE(pr.reserved_vram_mb,0) AS reserved_vram_mb
		FROM projects p
		LEFT JOIN agent_runtimes ar ON ar.project_id = p.id
		LEFT JOIN project_reservations pr ON pr.project_id = p.id
		WHERE 1=1
		  AND ($1 = '' OR p.status = $1)
		GROUP BY p.id, pr.reserved_vram_mb
		ORDER BY p.name`,
		c.Query("status"),
	)

	// Convert to the same output shape as the new handler
	out := make([]projectRowLegacy, 0, len(legacyRows))
	for _, r := range legacyRows {
		w := enumToWeight(r.Priority)
		out = append(out, projectRowLegacy{
			ID:                r.ID,
			OrganizationID:    r.OrganizationID,
			TeamID:            r.TeamID,
			Name:              r.Name,
			Description:       r.Description,
			PriorityWeight:    w,
			PriorityLabel:     project.PriorityWeight(w).Label(),
			EffectivePriority: w,
			Preemptible:       true,
			Status:            r.Status,
			RuntimeCount:      r.RuntimeCount,
			ReservedVRAMMB:    r.ReservedVRAMMB,
			CreatedAt:         r.CreatedAt,
			UpdatedAt:         r.UpdatedAt,
		})
	}
	return out
}

type projectRowLegacy struct {
	ID                string    `json:"id"`
	OrganizationID    string    `json:"organization_id"`
	TeamID            string    `json:"team_id"`
	Name              string    `json:"name"`
	Description       string    `json:"description"`
	PriorityWeight    int       `json:"priority_weight"`
	PriorityLabel     string    `json:"priority_label"`
	EffectivePriority int       `json:"effective_priority"`
	Preemptible       bool      `json:"preemptible"`
	Status            string    `json:"status"`
	RuntimeCount      int       `json:"runtime_count"`
	ReservedVRAMMB    int64     `json:"reserved_vram_mb"`
	ReservedCPUCores  int       `json:"reserved_cpu_cores"`
	ReservedMemoryMB  int64     `json:"reserved_memory_mb"`
	Protected         bool      `json:"protected"`
	AlwaysRunning     bool      `json:"always_running"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// enumToWeight converts the legacy 5-tier enum to the canonical numeric weight.
func enumToWeight(p string) int {
	switch p {
	case "CRITICAL":
		return 900
	case "HIGH":
		return 700
	case "NORMAL":
		return 500
	case "LOW":
		return 300
	case "BEST_EFFORT":
		return 0
	default:
		return 500
	}
}

// enumToWeight_reverse maps a numeric priority_weight back to the closest
// legacy enum string. Used when migration 018 has not been applied and we
// need to persist the change in the old priority VARCHAR column.
func enumToWeight_reverse(w int) string {
	switch {
	case w >= 900:
		return "CRITICAL"
	case w >= 700:
		return "HIGH"
	case w >= 300:
		return "NORMAL"
	case w >= 100:
		return "LOW"
	default:
		return "BEST_EFFORT"
	}
}

// getProjectLegacy is the fallback GET handler used when migration 018 has not
// been applied. It queries only the columns from migration 011 and maps the
// old enum priority to a numeric weight so the response shape is consistent.
func (h *ProjectHandler) getProjectLegacy(c *gin.Context, id string) {
	type legacyFull struct {
		ID               string    `db:"id"`
		OrganizationID   string    `db:"organization_id"`
		TeamID           string    `db:"team_id"`
		Name             string    `db:"name"`
		Description      string    `db:"description"`
		Priority         string    `db:"priority"`
		Status           string    `db:"status"`
		CreatedAt        time.Time `db:"created_at"`
		UpdatedAt        time.Time `db:"updated_at"`
		ReservedVRAMMB   int64     `db:"reserved_vram_mb"`
		ReservedCPUCores int       `db:"reserved_cpu_cores"`
		ReservedMemoryMB int64     `db:"reserved_memory_mb"`
		AlwaysRunning    bool      `db:"always_running"`
		Protected        bool      `db:"protected"`
		MinReplicas      int       `db:"minimum_replicas"`
		AdmissionPolicy  string    `db:"admission_policy"`
	}
	var row legacyFull
	err := h.db.GetContext(c.Request.Context(), &row, `
		SELECT p.id, p.organization_id, p.team_id, p.name, p.description,
		       p.priority, p.status, p.created_at, p.updated_at,
		       COALESCE(pr.reserved_vram_mb,0)    AS reserved_vram_mb,
		       COALESCE(pr.reserved_cpu_cores,0)  AS reserved_cpu_cores,
		       COALESCE(pr.reserved_memory_mb,0)  AS reserved_memory_mb,
		       COALESCE(pc.always_running,FALSE)   AS always_running,
		       COALESCE(pc.protected,FALSE)        AS protected,
		       COALESCE(pc.minimum_replicas,0)     AS minimum_replicas,
		       COALESCE(pc.admission_policy,'queue') AS admission_policy
		FROM projects p
		LEFT JOIN project_reservations pr ON pr.project_id = p.id
		LEFT JOIN project_configurations pc ON pc.project_id = p.id
		WHERE p.id = $1`, id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}
	w := enumToWeight(row.Priority)
	c.JSON(http.StatusOK, gin.H{
		"id":                 row.ID,
		"organization_id":    row.OrganizationID,
		"team_id":            row.TeamID,
		"name":               row.Name,
		"description":        row.Description,
		"priority_weight":    w,
		"priority_label":     project.PriorityWeight(w).Label(),
		"effective_priority": w,
		"waiting_bonus":      0,
		"reservation_bonus":  0,
		"resource_penalty":   0,
		"preemptible":        true,
		"status":             row.Status,
		"reserved_vram_mb":   row.ReservedVRAMMB,
		"reserved_cpu_cores": row.ReservedCPUCores,
		"reserved_memory_mb": row.ReservedMemoryMB,
		"max_gpu_vram_mb":    0,
		"max_cpu":            0,
		"max_memory_mb":      0,
		"always_running":     row.AlwaysRunning,
		"protected":          row.Protected,
		"minimum_replicas":   row.MinReplicas,
		"admission_policy":   row.AdmissionPolicy,
		"runtime_count":      0,
		"created_at":         row.CreatedAt,
		"updated_at":         row.UpdatedAt,
	})
}
