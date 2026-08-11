// Package handlers — portal.go
// Self-Service Developer Portal & Automatic Provisioning Engine for NexusLLM.
package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	internalauth "github.com/nexusllm/nexusllm/internal/auth"
	"github.com/nexusllm/nexusllm/internal/catalog"
	"github.com/nexusllm/nexusllm/internal/policy"
	"github.com/nexusllm/nexusllm/internal/project"
	"github.com/nexusllm/nexusllm/internal/runtime"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
)

// PortalHandler manages self-service developer portal APIs and provisioning.
type PortalHandler struct {
	db              *sqlx.DB
	rdb             *redis.Client
	engine          *policy.Engine
	registry        *runtime.Registry
	virtualResolver *catalog.VirtualModelResolver
	authSvc         *internalauth.Service
}

// NewPortalHandler constructs a PortalHandler.
func NewPortalHandler(
	db *sqlx.DB,
	rdb *redis.Client,
	engine *policy.Engine,
	registry *runtime.Registry,
	virtualResolver *catalog.VirtualModelResolver,
	authSvc *internalauth.Service,
) *PortalHandler {
	return &PortalHandler{
		db:              db,
		rdb:             rdb,
		engine:          engine,
		registry:        registry,
		virtualResolver: virtualResolver,
		authSvc:         authSvc,
	}
}

func (h *PortalHandler) getAuthOrgID(c *gin.Context) string {
	header := c.GetHeader("Authorization")
	if header != "" && strings.HasPrefix(header, "Bearer ") {
		tokenStr := strings.TrimPrefix(header, "Bearer ")
		if h.authSvc != nil {
			claims, err := h.authSvc.ValidateJWT(c.Request.Context(), tokenStr)
			if err == nil && claims != nil && claims.OrgID != "" {
				return claims.OrgID
			}
		}
	}
	if orgID := c.Query("org_id"); orgID != "" {
		return orgID
	}
	return c.GetHeader("X-Organization-ID")
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. Developer Project Management
// ─────────────────────────────────────────────────────────────────────────────

type CreatePortalProjectInput struct {
	OrganizationID          string `json:"organization_id"`
	TeamID                  string `json:"team_id"`
	Name                    string `json:"name"            binding:"required"`
	Description             string `json:"description"`
	Environment             string `json:"environment"` // development | staging | production
	ExpectedMonthlyRequests int64  `json:"expected_monthly_requests"`
	ExpectedMonthlyTokens   int64  `json:"expected_monthly_tokens"`
}

// CreatePortalProject handles POST /portal/v1/projects
func (h *PortalHandler) CreatePortalProject(c *gin.Context) {
	var in CreatePortalProjectInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if authOrg := h.getAuthOrgID(c); authOrg != "" {
		in.OrganizationID = authOrg
	}
	if in.OrganizationID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id is required"})
		return
	}
	if len(in.Name) > 200 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name must be <= 200 characters"})
		return
	}
	if in.Environment == "" {
		in.Environment = "development"
	}
	switch in.Environment {
	case "development", "staging", "production":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "environment must be development, staging, or production"})
		return
	}

	// If team_id is missing, auto-create or pick default team for org
	teamID := in.TeamID
	if teamID == "" {
		_ = h.db.GetContext(c.Request.Context(), &teamID,
			`SELECT id::text FROM teams WHERE org_id=$1 ORDER BY created_at ASC LIMIT 1`, in.OrganizationID)
		if teamID == "" {
			// Auto-create default team for developer
			teamID = uuid.New().String()
			_, err := h.db.ExecContext(c.Request.Context(), `
				INSERT INTO teams (id, org_id, name, slug, created_at, updated_at)
				VALUES ($1, $2, 'Default Developer Team', $3, NOW(), NOW())`,
				teamID, in.OrganizationID, "default-team-"+teamID[:8])
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to initialize team"})
				return
			}
		}
	}

	projID := uuid.New().String()
	weight := int(project.DefaultWeight)
	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO projects
		  (id, organization_id, team_id, name, description, priority_weight, status, preemptible,
		   environment, expected_monthly_requests, expected_monthly_tokens, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending', TRUE, $7, $8, $9, NOW(), NOW())`,
		projID, in.OrganizationID, teamID, in.Name, in.Description, weight,
		in.Environment, in.ExpectedMonthlyRequests, in.ExpectedMonthlyTokens)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error creating project: " + err.Error()})
		return
	}

	h.recordAudit(c, "PROJECT_CREATED", "Project created: "+in.Name, projID, in.OrganizationID, teamID)

	c.JSON(http.StatusCreated, gin.H{
		"id":                        projID,
		"organization_id":           in.OrganizationID,
		"team_id":                   teamID,
		"name":                      in.Name,
		"environment":               in.Environment,
		"expected_monthly_requests": in.ExpectedMonthlyRequests,
		"expected_monthly_tokens":   in.ExpectedMonthlyTokens,
		"status":                    "pending",
	})
}

// ListPortalProjects handles GET /portal/v1/projects
func (h *PortalHandler) ListPortalProjects(c *gin.Context) {
	orgID := h.getAuthOrgID(c)
	teamID := c.Query("team_id")

	query := `
		SELECT id::text, organization_id::text, COALESCE(team_id::text, '') AS team_id,
		       name, COALESCE(description, '') AS description, priority_weight, status,
		       COALESCE(environment, 'development') AS environment,
		       COALESCE(expected_monthly_requests, 0) AS expected_monthly_requests,
		       COALESCE(expected_monthly_tokens, 0) AS expected_monthly_tokens,
		       created_at, updated_at
		FROM projects
		WHERE ($1 = '' OR organization_id::text = $1)
		  AND ($2 = '' OR team_id::text = $2)
		ORDER BY created_at DESC`

	var rows []struct {
		ID                      string    `db:"id"                        json:"id"`
		OrganizationID          string    `db:"organization_id"           json:"organization_id"`
		TeamID                  string    `db:"team_id"                   json:"team_id"`
		Name                    string    `db:"name"                      json:"name"`
		Description             string    `db:"description"               json:"description"`
		PriorityWeight          int       `db:"priority_weight"           json:"priority_weight"`
		Status                  string    `db:"status"                    json:"status"`
		Environment             string    `db:"environment"               json:"environment"`
		ExpectedMonthlyRequests int64     `db:"expected_monthly_requests" json:"expected_monthly_requests"`
		ExpectedMonthlyTokens   int64     `db:"expected_monthly_tokens"   json:"expected_monthly_tokens"`
		CreatedAt               time.Time `db:"created_at"                json:"created_at"`
		UpdatedAt               time.Time `db:"updated_at"                json:"updated_at"`
	}

	if err := h.db.SelectContext(c.Request.Context(), &rows, query, orgID, teamID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "count": len(rows)})
}

// GetPortalProject handles GET /portal/v1/projects/:id
func (h *PortalHandler) GetPortalProject(c *gin.Context) {
	id := c.Param("id")
	authOrg := h.getAuthOrgID(c)
	var proj struct {
		ID                      string    `db:"id"                        json:"id"`
		OrganizationID          string    `db:"organization_id"           json:"organization_id"`
		TeamID                  string    `db:"team_id"                   json:"team_id"`
		Name                    string    `db:"name"                      json:"name"`
		Description             string    `db:"description"               json:"description"`
		PriorityWeight          int       `db:"priority_weight"           json:"priority_weight"`
		Status                  string    `db:"status"                    json:"status"`
		Environment             string    `db:"environment"               json:"environment"`
		ExpectedMonthlyRequests int64     `db:"expected_monthly_requests" json:"expected_monthly_requests"`
		ExpectedMonthlyTokens   int64     `db:"expected_monthly_tokens"   json:"expected_monthly_tokens"`
		CreatedAt               time.Time `db:"created_at"                json:"created_at"`
		UpdatedAt               time.Time `db:"updated_at"                json:"updated_at"`
	}
	err := h.db.GetContext(c.Request.Context(), &proj, `
		SELECT id::text, organization_id::text, COALESCE(team_id::text, '') AS team_id,
		       name, COALESCE(description, '') AS description, priority_weight, status,
		       COALESCE(environment, 'development') AS environment,
		       COALESCE(expected_monthly_requests, 0) AS expected_monthly_requests,
		       COALESCE(expected_monthly_tokens, 0) AS expected_monthly_tokens,
		       created_at, updated_at
		FROM projects WHERE id = $1 AND ($2 = '' OR organization_id::text = $2)`, id, authOrg)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}
	c.JSON(http.StatusOK, proj)
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Self-Service Access Requests
// ─────────────────────────────────────────────────────────────────────────────

type CreateAccessRequestInput struct {
	ProjectID           string   `json:"project_id"             binding:"required"`
	RequestedModels     []string `json:"requested_models"`    // e.g. ["gemma-2", "llama-3"]
	RequestedProviders  []string `json:"requested_providers"` // e.g. ["openrouter", "openai/gpt-5"]
	BusinessUseCase     string   `json:"business_use_case"`
	ExpectedRPM         int      `json:"expected_rpm"`
	ExpectedTPM         int      `json:"expected_tpm"`
	RequiredContextSize int      `json:"required_context_size"`
}

// CreateAccessRequest handles POST /portal/v1/requests
func (h *PortalHandler) CreateAccessRequest(c *gin.Context) {
	var in CreateAccessRequestInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var proj struct {
		OrgID  string `db:"organization_id"`
		TeamID string `db:"team_id"`
	}
	if err := h.db.GetContext(c.Request.Context(), &proj,
		`SELECT organization_id::text, COALESCE(team_id::text,'') AS team_id FROM projects WHERE id = $1`,
		in.ProjectID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	modelsJSON, _ := json.Marshal(in.RequestedModels)
	providersJSON, _ := json.Marshal(in.RequestedProviders)

	reqID := uuid.New().String()
	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO project_access_requests
		  (id, project_id, organization_id, team_id, status,
		   requested_models, requested_providers, business_use_case,
		   expected_rpm, expected_tpm, required_context_size, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'pending_review', $5, $6, $7, $8, $9, $10, NOW(), NOW())`,
		reqID, in.ProjectID, proj.OrgID, proj.TeamID,
		string(modelsJSON), string(providersJSON), in.BusinessUseCase,
		in.ExpectedRPM, in.ExpectedTPM, in.RequiredContextSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error: " + err.Error()})
		return
	}

	h.recordAudit(c, "ACCESS_REQUEST_SUBMITTED", "Submitted access request for project: "+in.ProjectID, in.ProjectID, proj.OrgID, proj.TeamID)

	c.JSON(http.StatusCreated, gin.H{
		"id":                    reqID,
		"project_id":            in.ProjectID,
		"status":                "pending_review",
		"requested_models":      in.RequestedModels,
		"requested_providers":   in.RequestedProviders,
		"expected_rpm":          in.ExpectedRPM,
		"expected_tpm":          in.ExpectedTPM,
		"required_context_size": in.RequiredContextSize,
	})
}

// ListAccessRequests handles GET /portal/v1/requests
func (h *PortalHandler) ListAccessRequests(c *gin.Context) {
	projID := c.Query("project_id")
	orgID := h.getAuthOrgID(c)

	var rows []struct {
		ID                  string    `db:"id"                    json:"id"`
		ProjectID           string    `db:"project_id"            json:"project_id"`
		OrganizationID      string    `db:"organization_id"       json:"organization_id"`
		TeamID              string    `db:"team_id"               json:"team_id"`
		Status              string    `db:"status"                json:"status"`
		RequestedModels     string    `db:"requested_models"      json:"requested_models"`
		RequestedProviders  string    `db:"requested_providers"   json:"requested_providers"`
		BusinessUseCase     string    `db:"business_use_case"     json:"business_use_case"`
		ExpectedRPM         int       `db:"expected_rpm"          json:"expected_rpm"`
		ExpectedTPM         int       `db:"expected_tpm"          json:"expected_tpm"`
		RequiredContextSize int       `db:"required_context_size" json:"required_context_size"`
		ReviewNotes         string    `db:"review_notes"          json:"review_notes"`
		CreatedAt           time.Time `db:"created_at"            json:"created_at"`
		UpdatedAt           time.Time `db:"updated_at"            json:"updated_at"`
	}

	err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT id::text, project_id::text, organization_id::text, COALESCE(team_id::text,'') AS team_id,
		       status, requested_models::text, requested_providers::text,
		       COALESCE(business_use_case, '') AS business_use_case,
		       expected_rpm, expected_tpm, required_context_size,
		       COALESCE(review_notes, '') AS review_notes,
		       created_at, updated_at
		FROM project_access_requests
		WHERE ($1 = '' OR project_id::text = $1)
		  AND ($2 = '' OR organization_id::text = $2)
		ORDER BY created_at DESC`, projID, orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "count": len(rows)})
}

// GetAccessRequest handles GET /portal/v1/requests/:id
func (h *PortalHandler) GetAccessRequest(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		ID                  string    `db:"id"                    json:"id"`
		ProjectID           string    `db:"project_id"            json:"project_id"`
		OrganizationID      string    `db:"organization_id"       json:"organization_id"`
		TeamID              string    `db:"team_id"               json:"team_id"`
		Status              string    `db:"status"                json:"status"`
		RequestedModels     string    `db:"requested_models"      json:"requested_models"`
		RequestedProviders  string    `db:"requested_providers"   json:"requested_providers"`
		BusinessUseCase     string    `db:"business_use_case"     json:"business_use_case"`
		ExpectedRPM         int       `db:"expected_rpm"          json:"expected_rpm"`
		ExpectedTPM         int       `db:"expected_tpm"          json:"expected_tpm"`
		RequiredContextSize int       `db:"required_context_size" json:"required_context_size"`
		ReviewNotes         string    `db:"review_notes"          json:"review_notes"`
		CreatedAt           time.Time `db:"created_at"            json:"created_at"`
		UpdatedAt           time.Time `db:"updated_at"            json:"updated_at"`
	}
	err := h.db.GetContext(c.Request.Context(), &req, `
		SELECT id::text, project_id::text, organization_id::text, COALESCE(team_id::text,'') AS team_id,
		       status, requested_models::text, requested_providers::text,
		       COALESCE(business_use_case, '') AS business_use_case,
		       expected_rpm, expected_tpm, required_context_size,
		       COALESCE(review_notes, '') AS review_notes,
		       created_at, updated_at
		FROM project_access_requests WHERE id = $1`, id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "access request not found"})
		return
	}
	c.JSON(http.StatusOK, req)
}

// CancelAccessRequest handles POST /portal/v1/requests/:id/cancel
func (h *PortalHandler) CancelAccessRequest(c *gin.Context) {
	id := c.Param("id")
	_, err := h.db.ExecContext(c.Request.Context(), `
		UPDATE project_access_requests
		SET status = 'cancelled', updated_at = NOW()
		WHERE id = $1 AND status IN ('draft','submitted','pending_review')`, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "access request cancelled"})
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Admin Review & Automatic Provisioning Engine
// ─────────────────────────────────────────────────────────────────────────────

type ReviewRequestInput struct {
	Action      string `json:"action" binding:"required"` // approve | reject | modify
	ReviewNotes string `json:"review_notes"`
	OverrideRPM *int   `json:"override_rpm"`
	OverrideTPM *int   `json:"override_tpm"`
}

// ListPendingPortalRequests handles GET /admin/v1/portal/requests/pending
func (h *PortalHandler) ListPendingPortalRequests(c *gin.Context) {
	var rows []struct {
		ID                  string    `db:"id"                    json:"id"`
		ProjectID           string    `db:"project_id"            json:"project_id"`
		ProjectName         string    `db:"project_name"          json:"project_name"`
		OrganizationID      string    `db:"organization_id"       json:"organization_id"`
		TeamID              string    `db:"team_id"               json:"team_id"`
		Status              string    `db:"status"                json:"status"`
		RequestedModels     string    `db:"requested_models"      json:"requested_models"`
		RequestedProviders  string    `db:"requested_providers"   json:"requested_providers"`
		BusinessUseCase     string    `db:"business_use_case"     json:"business_use_case"`
		ExpectedRPM         int       `db:"expected_rpm"          json:"expected_rpm"`
		ExpectedTPM         int       `db:"expected_tpm"          json:"expected_tpm"`
		RequiredContextSize int       `db:"required_context_size" json:"required_context_size"`
		CreatedAt           time.Time `db:"created_at"            json:"created_at"`
	}

	err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT par.id::text, par.project_id::text, p.name AS project_name,
		       par.organization_id::text, COALESCE(par.team_id::text,'') AS team_id,
		       par.status, par.requested_models::text, par.requested_providers::text,
		       COALESCE(par.business_use_case, '') AS business_use_case,
		       par.expected_rpm, par.expected_tpm, par.required_context_size,
		       par.created_at
		FROM project_access_requests par
		JOIN projects p ON p.id = par.project_id
		WHERE par.status IN ('pending_review', 'submitted')
		ORDER BY par.created_at ASC`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "count": len(rows)})
}

// ReviewPortalAccessRequest handles POST /admin/v1/portal/requests/:id/review
// Executes automatic provisioning on approval:
//  1. Grants team_model_permissions for public models
//  2. Grants project_provider_access for cloud providers
//  3. Applies rate limits & quotas in project_policies
//  4. Generates project API key (nxs_...)
//  5. Invalidates Redis caches
//  6. Emits developer notification and audit log
func (h *PortalHandler) ReviewPortalAccessRequest(c *gin.Context) {
	reqID := c.Param("id")
	var in ReviewRequestInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	tx, err := h.db.BeginTxx(c.Request.Context(), nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start transaction"})
		return
	}
	defer tx.Rollback()

	var req struct {
		ID                 string `db:"id"`
		ProjectID          string `db:"project_id"`
		OrgID              string `db:"organization_id"`
		TeamID             string `db:"team_id"`
		Status             string `db:"status"`
		RequestedModels    string `db:"requested_models"`
		RequestedProviders string `db:"requested_providers"`
		ExpectedRPM        int    `db:"expected_rpm"`
		ExpectedTPM        int    `db:"expected_tpm"`
	}
	if err := tx.GetContext(c.Request.Context(), &req, `
		SELECT id::text, project_id::text, organization_id::text, COALESCE(team_id::text,'') AS team_id,
		       status, requested_models::text, requested_providers::text, expected_rpm, expected_tpm
		FROM project_access_requests WHERE id = $1 FOR UPDATE`, reqID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "request not found"})
		return
	}

	if req.Status != "pending_review" && req.Status != "submitted" {
		c.JSON(http.StatusConflict, gin.H{"error": "request has already been reviewed"})
		return
	}

	if in.Action == "reject" {
		_, _ = tx.ExecContext(c.Request.Context(), `
			UPDATE project_access_requests
			SET status = 'rejected', review_notes = $1, reviewed_at = NOW(), updated_at = NOW()
			WHERE id = $2`, in.ReviewNotes, reqID)
		_ = tx.Commit()

		h.notifyDeveloper(c.Request.Context(), req.ProjectID, "Access Request Rejected", "Your access request was rejected: "+in.ReviewNotes, "request_rejected")
		h.recordAudit(c, "ACCESS_REQUEST_REJECTED", "Rejected request: "+reqID, req.ProjectID, req.OrgID, req.TeamID)

		c.JSON(http.StatusOK, gin.H{"message": "access request rejected"})
		return
	}

	// ── ACTION: APPROVE / MODIFY ─────────────────────────────────────────────
	rpm := req.ExpectedRPM
	if in.OverrideRPM != nil && *in.OverrideRPM > 0 {
		rpm = *in.OverrideRPM
	}
	tpm := req.ExpectedTPM
	if in.OverrideTPM != nil && *in.OverrideTPM > 0 {
		tpm = *in.OverrideTPM
	}

	// 1. Activate Project
	_, err = tx.ExecContext(c.Request.Context(), `UPDATE projects SET status = 'active', updated_at = NOW() WHERE id = $1`, req.ProjectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to activate project"})
		return
	}

	// 2. Provision Public Models (team_model_permissions)
	var modelsList []string
	_ = json.Unmarshal([]byte(req.RequestedModels), &modelsList)
	for _, modelName := range modelsList {
		var modelID string
		if err := tx.GetContext(c.Request.Context(), &modelID, `SELECT id FROM models WHERE name = $1`, modelName); err == nil && modelID != "" {
			_, _ = tx.ExecContext(c.Request.Context(),
				`INSERT INTO team_model_permissions (team_id, model_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
				req.TeamID, modelID)
			_ = h.engine.SetModelAllowed(c.Request.Context(), req.TeamID, modelName)
			_ = h.engine.SetOrgModelAllowed(c.Request.Context(), req.OrgID, modelName)
		}
	}

	// 3. Provision Cloud Providers (project_provider_access)
	var providersList []string
	_ = json.Unmarshal([]byte(req.RequestedProviders), &providersList)
	for _, provName := range providersList {
		var provID string
		if err := tx.GetContext(c.Request.Context(), &provID, `SELECT id FROM providers WHERE name = $1 OR id::text = $1`, provName); err == nil && provID != "" {
			_, _ = tx.ExecContext(c.Request.Context(), `
				INSERT INTO project_provider_access (project_id, provider_id, enabled, created_at, updated_at)
				VALUES ($1, $2, TRUE, NOW(), NOW())
				ON CONFLICT (project_id, provider_id) DO UPDATE SET enabled = TRUE, updated_at = NOW()`,
				req.ProjectID, provID)
		}
	}

	// 4. Rate Limits & Quotas (project_policies)
	_, _ = tx.ExecContext(c.Request.Context(), `
		INSERT INTO project_policies (project_id, rpm, tpm, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (project_id) DO UPDATE SET rpm = $2, tpm = $3, updated_at = NOW()`,
		req.ProjectID, rpm, tpm)

	// 5. Automatic API Key Generation
	rawKey, hashKey, err := internalauth.GenerateAPIKey()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate API key"})
		return
	}
	keyID := uuid.New().String()
	prefix := rawKey[:12]
	keyName := "Default Auto-Provisioned Key"

	var projName string
	var projWeight int
	_ = tx.QueryRowContext(c.Request.Context(), `SELECT name, priority_weight FROM projects WHERE id = $1`, req.ProjectID).Scan(&projName, &projWeight)

	_, err = tx.ExecContext(c.Request.Context(), `
		INSERT INTO api_keys
		  (id, team_id, name, key_hash, key_prefix, active, created_at, updated_at,
		   project_id, project_name, project_priority_weight)
		VALUES ($1, $2, $3, $4, $5, TRUE, NOW(), NOW(), $6, $7, $8)`,
		keyID, req.TeamID, keyName, hashKey, prefix, req.ProjectID, projName, projWeight)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store API key"})
		return
	}

	// 6. Complete request status
	_, _ = tx.ExecContext(c.Request.Context(), `
		UPDATE project_access_requests
		SET status = 'approved', review_notes = $1, provisioned_api_key_id = $2,
		    reviewed_at = NOW(), updated_at = NOW()
		WHERE id = $3`, in.ReviewNotes, keyID, reqID)

	if err := tx.Commit(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "transaction commit failed"})
		return
	}

	// 7. Clear Redis caches
	_ = h.rdb.Del(c.Request.Context(), "nexus:org:"+req.OrgID+":models").Err()
	_ = h.rdb.Del(c.Request.Context(), "nexus:team:"+req.TeamID+":models").Err()
	if h.virtualResolver != nil {
		h.virtualResolver.Invalidate()
	}

	h.notifyDeveloper(c.Request.Context(), req.ProjectID, "Access Request Approved", "Your access request was approved! API Key generated.", "request_approved")
	h.recordAudit(c, "ACCESS_REQUEST_APPROVED", "Approved request: "+reqID, req.ProjectID, req.OrgID, req.TeamID)

	c.JSON(http.StatusOK, gin.H{
		"message":                "access request approved & provisioned automatically",
		"status":                 "approved",
		"provisioned_api_key_id": keyID,
		"api_key_prefix":         prefix,
		"api_key_secret":         rawKey, // Revealed ONCE
		"rpm_limit":              rpm,
		"tpm_limit":              tpm,
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Developer API Key Management & Zero-Downtime Rotation
// ─────────────────────────────────────────────────────────────────────────────

// CreatePortalAPIKey handles POST /portal/v1/projects/:id/api-keys
func (h *PortalHandler) CreatePortalAPIKey(c *gin.Context) {
	projID := c.Param("id")
	var input struct {
		Name      string     `json:"name" binding:"required"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	authOrg := h.getAuthOrgID(c)
	var proj struct {
		TeamID         string `db:"team_id"`
		OrgID          string `db:"organization_id"`
		Name           string `db:"name"`
		PriorityWeight int    `db:"priority_weight"`
	}
	if err := h.db.GetContext(c.Request.Context(), &proj,
		`SELECT team_id::text, organization_id::text, name, priority_weight FROM projects WHERE id = $1 AND ($2 = '' OR organization_id::text = $2)`,
		projID, authOrg); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	rawKey, hashKey, err := internalauth.GenerateAPIKey()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate key"})
		return
	}
	keyID := uuid.New().String()
	prefix := rawKey[:12]

	_, err = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO api_keys
		  (id, team_id, name, key_hash, key_prefix, active, expires_at, created_at, updated_at,
		   project_id, project_name, project_priority_weight)
		VALUES ($1, $2, $3, $4, $5, TRUE, $6, NOW(), NOW(), $7, $8, $9)`,
		keyID, proj.TeamID, input.Name, hashKey, prefix, input.ExpiresAt, projID, proj.Name, proj.PriorityWeight)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}

	h.recordAudit(c, "API_KEY_CREATED", "Created API key: "+input.Name, projID, proj.OrgID, proj.TeamID)

	c.JSON(http.StatusCreated, gin.H{
		"id":             keyID,
		"name":           input.Name,
		"key_prefix":     prefix,
		"api_key_secret": rawKey, // Revealed ONCE
		"expires_at":     input.ExpiresAt,
	})
}

// RotatePortalAPIKey handles POST /portal/v1/api-keys/:key_id/rotate
// Implements zero-downtime rotation: creates a new key while keeping old key active for 24h grace period.
func (h *PortalHandler) RotatePortalAPIKey(c *gin.Context) {
	keyID := c.Param("key_id")

	var oldKey struct {
		TeamID         string `db:"team_id"`
		ProjectID      string `db:"project_id"`
		Name           string `db:"name"`
		ProjectName    string `db:"project_name"`
		PriorityWeight int    `db:"project_priority_weight"`
		OrgID          string `db:"org_id"`
	}
	err := h.db.GetContext(c.Request.Context(), &oldKey, `
		SELECT ak.team_id::text, COALESCE(ak.project_id::text,'') AS project_id,
		       ak.name, COALESCE(ak.project_name,'') AS project_name,
		       COALESCE(ak.project_priority_weight, 500) AS project_priority_weight,
		       t.org_id::text
		FROM api_keys ak
		JOIN teams t ON t.id = ak.team_id
		WHERE ak.id = $1 AND ak.active = TRUE`, keyID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "active key not found"})
		return
	}

	// 1. Set old key expiration to 24h from now (grace period)
	graceExpiry := time.Now().Add(24 * time.Hour)
	_, _ = h.db.ExecContext(c.Request.Context(),
		`UPDATE api_keys SET expires_at = $1, updated_at = NOW() WHERE id = $2`, graceExpiry, keyID)

	// 2. Generate new replacement key
	rawKey, hashKey, err := internalauth.GenerateAPIKey()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate new key"})
		return
	}
	newKeyID := uuid.New().String()
	prefix := rawKey[:12]
	newName := oldKey.Name + " (Rotated)"

	_, err = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO api_keys
		  (id, team_id, name, key_hash, key_prefix, active, created_at, updated_at,
		   project_id, project_name, project_priority_weight)
		VALUES ($1, $2, $3, $4, $5, TRUE, NOW(), NOW(), $6, $7, $8)`,
		newKeyID, oldKey.TeamID, newName, hashKey, prefix, oldKey.ProjectID, oldKey.ProjectName, oldKey.PriorityWeight)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store rotated key"})
		return
	}

	h.recordAudit(c, "API_KEY_ROTATED", "Rotated key "+keyID+" into new key "+newKeyID, oldKey.ProjectID, oldKey.OrgID, oldKey.TeamID)

	c.JSON(http.StatusOK, gin.H{
		"message":               "key rotated successfully (old key remains active for 24h grace period)",
		"old_key_id":            keyID,
		"old_key_grace_expires": graceExpiry,
		"new_key_id":            newKeyID,
		"new_key_prefix":        prefix,
		"new_api_key_secret":    rawKey, // Revealed ONCE
	})
}

// DeletePortalAPIKey handles DELETE /portal/v1/api-keys/:key_id
func (h *PortalHandler) DeletePortalAPIKey(c *gin.Context) {
	keyID := c.Param("key_id")
	var keyInfo struct {
		KeyHash string `db:"key_hash"`
	}
	_ = h.db.GetContext(c.Request.Context(), &keyInfo, `SELECT key_hash FROM api_keys WHERE id = $1`, keyID)

	_, err := h.db.ExecContext(c.Request.Context(), `UPDATE api_keys SET active = FALSE, updated_at = NOW() WHERE id = $1`, keyID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if keyInfo.KeyHash != "" {
		_ = h.rdb.Del(c.Request.Context(), "nexus:apikey:"+keyInfo.KeyHash).Err()
	}
	c.JSON(http.StatusOK, gin.H{"message": "API key revoked successfully"})
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. Scoped Visibility & Metrics
// ─────────────────────────────────────────────────────────────────────────────

// GetPortalModels handles GET /portal/v1/models
// Returns ONLY models granted to the developer's project/team. No catalog leakage.
func (h *PortalHandler) GetPortalModels(c *gin.Context) {
	projID := c.Query("project_id")
	if projID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "project_id query param is required"})
		return
	}

	var proj struct {
		TeamID string `db:"team_id"`
	}
	if err := h.db.GetContext(c.Request.Context(), &proj,
		`SELECT COALESCE(team_id::text,'') AS team_id FROM projects WHERE id = $1`, projID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	type modelItem struct {
		Name        string `db:"name"        json:"name"`
		DisplayName string `db:"display_name" json:"display_name"`
		BackendType string `db:"backend_type" json:"backend_type"`
		Source      string `db:"source"      json:"source"`
	}

	var publicModels []modelItem
	_ = h.db.SelectContext(c.Request.Context(), &publicModels, `
		SELECT m.name, m.display_name, m.backend_type, 'public' AS source
		FROM models m
		JOIN team_model_permissions tmp ON tmp.model_id = m.id
		WHERE tmp.team_id = $1 AND m.enabled = TRUE`, proj.TeamID)

	c.JSON(http.StatusOK, gin.H{
		"data":  publicModels,
		"count": len(publicModels),
	})
}

// GetPortalUsage handles GET /portal/v1/usage
func (h *PortalHandler) GetPortalUsage(c *gin.Context) {
	projID := c.Query("project_id")
	if projID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "project_id is required"})
		return
	}

	// Fetch rate limit policy
	var pol struct {
		RPMLimit int `db:"rpm"`
		TPMLimit int `db:"tpm"`
	}
	_ = h.db.GetContext(c.Request.Context(), &pol,
		`SELECT rpm, tpm FROM project_policies WHERE project_id = $1`, projID)

	c.JSON(http.StatusOK, gin.H{
		"project_id":        projID,
		"rpm_limit":         pol.RPMLimit,
		"tpm_limit":         pol.TPMLimit,
		"current_rpm":       0,
		"current_tpm":       0,
		"daily_token_usage": 0,
		"status":            "healthy",
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. Developer Notifications
// ─────────────────────────────────────────────────────────────────────────────

// GetPortalNotifications handles GET /portal/v1/notifications
func (h *PortalHandler) GetPortalNotifications(c *gin.Context) {
	projID := c.Query("project_id")
	var rows []struct {
		ID        string    `db:"id"         json:"id"`
		ProjectID string    `db:"project_id" json:"project_id"`
		Title     string    `db:"title"      json:"title"`
		Message   string    `db:"message"    json:"message"`
		Type      string    `db:"type"       json:"type"`
		Read      bool      `db:"read"       json:"read"`
		CreatedAt time.Time `db:"created_at" json:"created_at"`
	}
	err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT id::text, COALESCE(project_id::text,'') AS project_id,
		       title, message, type, read, created_at
		FROM developer_notifications
		WHERE ($1 = '' OR project_id::text = $1)
		ORDER BY created_at DESC LIMIT 50`, projID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "count": len(rows)})
}

// MarkNotificationRead handles POST /portal/v1/notifications/:id/read
func (h *PortalHandler) MarkNotificationRead(c *gin.Context) {
	id := c.Param("id")
	_, err := h.db.ExecContext(c.Request.Context(), `UPDATE developer_notifications SET read = TRUE WHERE id = $1`, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "notification marked as read"})
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal Audit & Notification Helpers
// ─────────────────────────────────────────────────────────────────────────────

func (h *PortalHandler) recordAudit(c *gin.Context, action, details, projID, orgID, teamID string) {
	_, _ = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO audit_logs (id, org_id, team_id, project_id, action, details, created_at)
		VALUES ($1, NULLIF($2,'')::uuid, NULLIF($3,'')::uuid, NULLIF($4,'')::uuid, $5, $6, NOW())`,
		uuid.New().String(), orgID, teamID, projID, action, details)
}

func (h *PortalHandler) notifyDeveloper(ctx context.Context, projID, title, msg, notifType string) {
	_, _ = h.db.ExecContext(ctx, `
		INSERT INTO developer_notifications (id, project_id, title, message, type, read, created_at)
		VALUES ($1, $2, $3, $4, $5, FALSE, NOW())`,
		uuid.New().String(), projID, title, msg, notifType)
}

func hashPassword(password string) string {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		sum := sha256.Sum256([]byte("nexus_salt_" + password))
		return hex.EncodeToString(sum[:])
	}
	return string(bytes)
}

func checkPassword(password, storedHash string) bool {
	if strings.HasPrefix(storedHash, "$2a$") || strings.HasPrefix(storedHash, "$2b$") {
		return bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(password)) == nil
	}
	sum := sha256.Sum256([]byte("nexus_salt_" + password))
	return hex.EncodeToString(sum[:]) == storedHash
}

// ─────────────────────────────────────────────────────────────────────────────
// 7. Developer & Admin Authentication Handlers
// ─────────────────────────────────────────────────────────────────────────────

type RegisterInput struct {
	Email    string `json:"email" binding:"required"`
	Password string `json:"password" binding:"required"`
	Name     string `json:"name"`
	OrgName  string `json:"org_name"`
}

// RegisterUser handles POST /portal/v1/auth/register
func (h *PortalHandler) RegisterUser(c *gin.Context) {
	var in RegisterInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	pwdHash := hashPassword(in.Password)

	orgName := in.OrgName
	if orgName == "" {
		orgName = in.Email + "'s Org"
	}
	orgID := uuid.New().String()
	orgSlug := "org-" + orgID[:8]

	_, err := h.db.ExecContext(c.Request.Context(), `
		INSERT INTO organizations (id, name, slug, created_at, updated_at)
		VALUES ($1, $2, $3, NOW(), NOW())`, orgID, orgName, orgSlug)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create organization"})
		return
	}

	userID := uuid.New().String()
	_, err = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO users (id, org_id, email, name, role, password_hash, active, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'member', $5, TRUE, NOW(), NOW())`,
		userID, orgID, in.Email, in.Name, pwdHash)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "user email already registered"})
		return
	}

	tokenStr := "dev-jwt-token-" + userID
	if h.authSvc != nil {
		t, tErr := h.authSvc.IssueJWT(&internalauth.RequestClaims{
			UserID: userID,
			OrgID:  orgID,
			Role:   "member",
			Email:  in.Email,
		}, 24*time.Hour)
		if tErr == nil {
			tokenStr = t
		}
	}

	c.JSON(http.StatusCreated, gin.H{
		"message": "registration successful",
		"user_id": userID,
		"org_id":  orgID,
		"email":   in.Email,
		"role":    "member",
		"token":   tokenStr,
	})
}

type LoginInput struct {
	Email    string `json:"email" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// LoginUser handles POST /portal/v1/auth/login
func (h *PortalHandler) LoginUser(c *gin.Context) {
	var in LoginInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var user struct {
		ID           string `db:"id"`
		OrgID        string `db:"org_id"`
		Email        string `db:"email"`
		Role         string `db:"role"`
		PasswordHash string `db:"password_hash"`
	}

	err := h.db.GetContext(c.Request.Context(), &user, `
		SELECT id::text, org_id::text, email, role, COALESCE(password_hash,'') AS password_hash
		FROM users WHERE email = $1 AND active = TRUE`, in.Email)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	if user.PasswordHash != "" && !checkPassword(in.Password, user.PasswordHash) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	tokenStr := "dev-jwt-token-" + user.ID
	if h.authSvc != nil {
		t, tErr := h.authSvc.IssueJWT(&internalauth.RequestClaims{
			UserID: user.ID,
			OrgID:  user.OrgID,
			Role:   user.Role,
			Email:  user.Email,
		}, 24*time.Hour)
		if tErr == nil {
			tokenStr = t
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "login successful",
		"user_id": user.ID,
		"org_id":  user.OrgID,
		"email":   user.Email,
		"role":    user.Role,
		"token":   tokenStr,
	})
}

// AdminLogin handles POST /admin/v1/auth/login
func (h *PortalHandler) AdminLogin(c *gin.Context) {
	var in LoginInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var user struct {
		ID           string `db:"id"`
		OrgID        string `db:"org_id"`
		Email        string `db:"email"`
		Role         string `db:"role"`
		PasswordHash string `db:"password_hash"`
	}

	err := h.db.GetContext(c.Request.Context(), &user, `
		SELECT id::text, org_id::text, email, role, COALESCE(password_hash,'') AS password_hash
		FROM users WHERE email = $1 AND active = TRUE`, in.Email)
	if err != nil || user.Role != "admin" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "admin authentication failed"})
		return
	}

	if user.PasswordHash != "" && !checkPassword(in.Password, user.PasswordHash) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid admin credentials"})
		return
	}

	tokenStr := "admin-jwt-token-" + user.ID
	if h.authSvc != nil {
		t, tErr := h.authSvc.IssueJWT(&internalauth.RequestClaims{
			UserID: user.ID,
			OrgID:  user.OrgID,
			Role:   "admin",
			Email:  user.Email,
		}, 24*time.Hour)
		if tErr == nil {
			tokenStr = t
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "admin login successful",
		"user_id": user.ID,
		"org_id":  user.OrgID,
		"email":   user.Email,
		"role":    "admin",
		"token":   tokenStr,
	})
}
