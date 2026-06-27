package handlers

// project_policy.go — Project-level policy, quota, and usage endpoints.
//
// Routes (registered in cmd/admin/main.go):
//
//	GET    /admin/v1/projects/:id/policy       — get project policy limits
//	PUT    /admin/v1/projects/:id/policy       — update project policy limits
//	GET    /admin/v1/projects/:id/quota        — live quota status (Redis counters)
//	GET    /admin/v1/projects/:id/usage/daily  — per-day usage breakdown
//	GET    /admin/v1/projects/:id/usage/summary — totals for a date range

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/policy"
	"github.com/nexusllm/nexusllm/internal/usage"
	"github.com/redis/go-redis/v9"
)

// ProjectPolicyHandler handles project-level policy and usage admin routes.
type ProjectPolicyHandler struct {
	db      *sqlx.DB
	rdb     *redis.Client
	pEngine *policy.Engine
	tracker *usage.Tracker
}

// NewProjectPolicyHandler constructs a ProjectPolicyHandler.
func NewProjectPolicyHandler(
	db *sqlx.DB,
	rdb *redis.Client,
	pEngine *policy.Engine,
	tracker *usage.Tracker,
) *ProjectPolicyHandler {
	return &ProjectPolicyHandler{db: db, rdb: rdb, pEngine: pEngine, tracker: tracker}
}

// ─── GET /admin/v1/projects/:id/policy ───────────────────────────────────────

func (h *ProjectPolicyHandler) GetPolicy(c *gin.Context) {
	id := c.Param("id")
	type row struct {
		ID                 string    `db:"id"                   json:"id"`
		ProjectID          string    `db:"project_id"           json:"project_id"`
		RPM                int       `db:"rpm"                  json:"rpm"`
		TPM                int       `db:"tpm"                  json:"tpm"`
		MaxConcurrent      int       `db:"max_concurrent"       json:"max_concurrent"`
		MaxContextTokens   int       `db:"max_context_tokens"   json:"max_context_tokens"`
		DailyTokenBudget   int64     `db:"daily_token_budget"   json:"daily_token_budget"`
		MonthlyTokenBudget int64     `db:"monthly_token_budget" json:"monthly_token_budget"`
		DailyCostBudget    float64   `db:"daily_cost_budget"    json:"daily_cost_budget"`
		MonthlyCostBudget  float64   `db:"monthly_cost_budget"  json:"monthly_cost_budget"`
		UpdatedAt          time.Time `db:"updated_at"           json:"updated_at"`
	}
	var r row
	err := h.db.GetContext(c.Request.Context(), &r, `
		SELECT id, project_id::text, rpm, tpm, max_concurrent, max_context_tokens,
		       daily_token_budget, monthly_token_budget,
		       daily_cost_budget::float8, monthly_cost_budget::float8, updated_at
		FROM project_policies WHERE project_id = $1`, id)
	if err != nil {
		// Auto-seed if missing (migration 023 may not have seeded this project)
		_, _ = h.db.ExecContext(c.Request.Context(),
			`INSERT INTO project_policies (project_id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
		_ = h.db.GetContext(c.Request.Context(), &r, `
			SELECT id, project_id::text, rpm, tpm, max_concurrent, max_context_tokens,
			       daily_token_budget, monthly_token_budget,
			       daily_cost_budget::float8, monthly_cost_budget::float8, updated_at
			FROM project_policies WHERE project_id = $1`, id)
	}
	c.JSON(200, r)
}

// ─── PUT /admin/v1/projects/:id/policy ───────────────────────────────────────

func (h *ProjectPolicyHandler) UpdatePolicy(c *gin.Context) {
	id := c.Param("id")
	var input struct {
		RPM                *int     `json:"rpm"`
		TPM                *int     `json:"tpm"`
		MaxConcurrent      *int     `json:"max_concurrent"`
		MaxContextTokens   *int     `json:"max_context_tokens"`
		DailyTokenBudget   *int64   `json:"daily_token_budget"`
		MonthlyTokenBudget *int64   `json:"monthly_token_budget"`
		DailyCostBudget    *float64 `json:"daily_cost_budget"`
		MonthlyCostBudget  *float64 `json:"monthly_cost_budget"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Upsert the policy row
	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO project_policies (project_id) VALUES ($1)
		ON CONFLICT (project_id) DO NOTHING`, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	_, err = h.db.ExecContext(c.Request.Context(), `
		UPDATE project_policies SET
		    rpm                 = COALESCE($2, rpm),
		    tpm                 = COALESCE($3, tpm),
		    max_concurrent      = COALESCE($4, max_concurrent),
		    max_context_tokens  = COALESCE($5, max_context_tokens),
		    daily_token_budget  = COALESCE($6, daily_token_budget),
		    monthly_token_budget = COALESCE($7, monthly_token_budget),
		    daily_cost_budget   = COALESCE($8, daily_cost_budget),
		    monthly_cost_budget = COALESCE($9, monthly_cost_budget),
		    updated_at          = NOW()
		WHERE project_id = $1`,
		id,
		input.RPM, input.TPM, input.MaxConcurrent, input.MaxContextTokens,
		input.DailyTokenBudget, input.MonthlyTokenBudget,
		input.DailyCostBudget, input.MonthlyCostBudget,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Push updated policy to Redis so the gateway hot path picks it up immediately.
	type pRow struct {
		RPM                int   `db:"rpm"`
		TPM                int   `db:"tpm"`
		MaxConcurrent      int   `db:"max_concurrent"`
		MaxContextTokens   int   `db:"max_context_tokens"`
		DailyTokenBudget   int64 `db:"daily_token_budget"`
		MonthlyTokenBudget int64 `db:"monthly_token_budget"`
	}
	var pr pRow
	if err2 := h.db.GetContext(c.Request.Context(), &pr,
		`SELECT rpm, tpm, max_concurrent, max_context_tokens,
		        daily_token_budget, monthly_token_budget
		 FROM project_policies WHERE project_id = $1`, id); err2 == nil {
		_ = h.pEngine.SetProjectPolicy(c.Request.Context(), id, policy.ProjectPolicy{
			RPMLimit:           pr.RPM,
			TPMLimit:           pr.TPM,
			MaxConcurrent:      pr.MaxConcurrent,
			MaxContextTokens:   pr.MaxContextTokens,
			DailyTokenBudget:   pr.DailyTokenBudget,
			MonthlyTokenBudget: pr.MonthlyTokenBudget,
		})
	}

	c.JSON(200, gin.H{"message": "project policy updated", "project_id": id})
}

// ─── GET /admin/v1/projects/:id/quota ────────────────────────────────────────
// Returns live Redis counters — current RPM usage, inflight, daily tokens used.

func (h *ProjectPolicyHandler) GetQuotaStatus(c *gin.Context) {
	id := c.Param("id")

	// Live counters from Redis
	live := h.pEngine.GetProjectQuotaStatus(c.Request.Context(), id)

	// DB policy limits
	type pRow struct {
		RPM                int     `db:"rpm"                  json:"rpm_limit"`
		TPM                int     `db:"tpm"                  json:"tpm_limit"`
		MaxConcurrent      int     `db:"max_concurrent"       json:"max_concurrent_limit"`
		DailyTokenBudget   int64   `db:"daily_token_budget"   json:"daily_token_budget"`
		MonthlyTokenBudget int64   `db:"monthly_token_budget" json:"monthly_token_budget"`
		DailyCostBudget    float64 `db:"daily_cost_budget"    json:"daily_cost_budget"`
		MonthlyCostBudget  float64 `db:"monthly_cost_budget"  json:"monthly_cost_budget"`
	}
	var pr pRow
	_ = h.db.GetContext(c.Request.Context(), &pr, `
		SELECT rpm, tpm, max_concurrent,
		       daily_token_budget, monthly_token_budget,
		       daily_cost_budget::float8, monthly_cost_budget::float8
		FROM project_policies WHERE project_id = $1`, id)

	c.JSON(200, gin.H{
		"project_id":           id,
		"rpm_limit":            pr.RPM,
		"tpm_limit":            pr.TPM,
		"max_concurrent_limit": pr.MaxConcurrent,
		"daily_token_budget":   pr.DailyTokenBudget,
		"monthly_token_budget": pr.MonthlyTokenBudget,
		"daily_cost_budget":    pr.DailyCostBudget,
		"monthly_cost_budget":  pr.MonthlyCostBudget,
		// Live counters
		"daily_tokens_used": live["daily_tokens_used"],
		"tpm_current":       live["tpm_current"],
		"inflight":          live["inflight"],
		// Computed remaining (0 limit = unlimited → nil)
		"daily_tokens_remaining": func() interface{} {
			if pr.DailyTokenBudget <= 0 {
				return nil
			}
			rem := pr.DailyTokenBudget - live["daily_tokens_used"]
			if rem < 0 {
				rem = 0
			}
			return rem
		}(),
	})
}

// ─── GET /admin/v1/projects/:id/usage/daily ──────────────────────────────────

func (h *ProjectPolicyHandler) GetDailyUsage(c *gin.Context) {
	id := c.Param("id")
	from := c.Query("from")
	to := c.Query("to")
	if from == "" {
		from = time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339)
	}
	if to == "" {
		to = time.Now().UTC().Format(time.RFC3339)
	}
	rows, err := h.tracker.GetProjectDailyUsage(c.Request.Context(), id, from, to)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if rows == nil {
		rows = []usage.ProjectDailySummary{}
	}
	c.JSON(200, gin.H{"data": rows, "total": len(rows), "project_id": id, "from": from, "to": to})
}

// ─── GET /admin/v1/projects/:id/usage/summary ─────────────────────────────────

func (h *ProjectPolicyHandler) GetUsageSummary(c *gin.Context) {
	id := c.Param("id")
	from := c.Query("from")
	to := c.Query("to")
	if from == "" {
		from = time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339)
	}
	if to == "" {
		to = time.Now().UTC().Format(time.RFC3339)
	}
	summary, err := h.tracker.GetProjectSummary(c.Request.Context(), id, from, to)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{
		"project_id":        id,
		"from":              from,
		"to":                to,
		"request_count":     summary.RequestCount,
		"error_count":       summary.ErrorCount,
		"prompt_tokens":     summary.PromptTokens,
		"completion_tokens": summary.CompletionTokens,
		"total_tokens":      summary.TotalTokens,
		"cost_usd":          summary.CostUSD,
		"avg_latency_ms":    summary.AvgLatencyMs,
	})
}
