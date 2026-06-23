// Package proxy handles OpenAI-compatible inference requests.
// Request pipeline:
//   Auth → Gateway Policy → Alias Resolution → Prompt Policy → Registry → Activator (on miss) → Backend
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/nexusllm/nexusllm/internal/alias"
	"github.com/nexusllm/nexusllm/internal/auth"
	"github.com/nexusllm/nexusllm/internal/gatewaypolicy"
	"github.com/nexusllm/nexusllm/internal/lifecycle"
	"github.com/nexusllm/nexusllm/internal/middleware"
	"github.com/nexusllm/nexusllm/internal/models"
	"github.com/nexusllm/nexusllm/internal/policy"
	"github.com/nexusllm/nexusllm/internal/promptpolicy"
	"github.com/nexusllm/nexusllm/internal/runtime"
	"github.com/nexusllm/nexusllm/internal/runtimemgr"
	"github.com/nexusllm/nexusllm/internal/usage"
	"go.uber.org/zap"
)

const maxFailoverAttempts = 3

// Handler proxies OpenAI-compatible requests through the full enterprise pipeline.
type Handler struct {
	policy        *policy.Engine
	gwPolicy      *gatewaypolicy.Engine
	promptPolicy  *promptpolicy.Engine
	aliasResolver *alias.Resolver
	lifecycleMgr  *lifecycle.Manager
	registry      *runtime.Registry
	activator     runtimemgr.Activator // lazy-load: starts model on demand
	usageTracker  *usage.Tracker
	log           *zap.Logger
	teamPolicies  map[string]*policy.TeamPolicy
	httpClient    *http.Client
	db            *sqlx.DB // for project context lookup; may be nil
}

// NewHandler constructs the proxy Handler.
func NewHandler(
	pe *policy.Engine,
	gwp *gatewaypolicy.Engine,
	pp *promptpolicy.Engine,
	ar *alias.Resolver,
	lm *lifecycle.Manager,
	registry *runtime.Registry,
	tracker *usage.Tracker,
	teamPolicies map[string]*policy.TeamPolicy,
	log *zap.Logger,
) *Handler {
	return &Handler{
		policy:        pe,
		gwPolicy:      gwp,
		promptPolicy:  pp,
		aliasResolver: ar,
		lifecycleMgr:  lm,
		registry:      registry,
		usageTracker:  tracker,
		log:           log,
		teamPolicies:  teamPolicies,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// WithActivator attaches a RuntimeActivator for lazy-loading models on demand.
// When set, a registry miss triggers EnsureRunning() instead of 503.
func (h *Handler) WithActivator(a runtimemgr.Activator) *Handler {
	h.activator = a
	return h
}

// WithDB attaches a database connection for project context enrichment.
func (h *Handler) WithDB(db *sqlx.DB) *Handler {
	h.db = db
	return h
}

// lookupProjectContext returns project_id, project_name, project_priority for a model name.
// Returns nil values if project is not set (legacy models).
func (h *Handler) lookupProjectContext(ctx context.Context, modelName string) (projectID, projectName, projectPriority *string) {
	if h.db == nil {
		return nil, nil, nil
	}
	var row struct {
		ProjectID       *string `db:"project_id"`
		ProjectName     *string `db:"project_name"`
		ProjectPriority *string `db:"project_priority"`
	}
	err := h.db.GetContext(ctx, &row, `
		SELECT p.id::text AS project_id, p.name AS project_name, p.priority AS project_priority
		FROM models m
		JOIN projects p ON p.id = m.project_id
		WHERE m.name = $1 AND m.enabled = TRUE
		LIMIT 1`, modelName)
	if err != nil {
		return nil, nil, nil
	}
	return row.ProjectID, row.ProjectName, row.ProjectPriority
}

// ─── public handlers ──────────────────────────────────────────────────────────

// ChatCompletions handles POST /v1/chat/completions
func (h *Handler) ChatCompletions(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		abortErr(c, http.StatusUnauthorized, "unauthorized", "Missing authentication")
		return
	}

	var req models.InferenceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abortErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Model == "" {
		abortErr(c, http.StatusBadRequest, "missing_model", "Field 'model' is required")
		return
	}

	// ── 1. Alias resolution ────────────────────────────────────────────────
	realModel, err := h.aliasResolver.Resolve(c.Request.Context(), req.Model, claims.TeamID, claims.OrgID)
	if err != nil {
		realModel = req.Model
	}
	req.Model = realModel
	c.Set("model", realModel)

	// ── 2. Gateway policy (temperature cap, tool restrictions, etc.) ───────
	inputEst := estimateTokens(req.Messages)
	if v := h.gwPolicy.Enforce(c.Request.Context(), claims.OrgID, claims.TeamID, "", &req, inputEst); v != nil {
		abortErr(c, http.StatusForbidden, v.Code, v.Message)
		return
	}

	// ── 3. Infrastructure policy (rate limit, quota, ACL) ─────────────────
	tp := h.teamPolicy(claims.TeamID)
	decision := h.policy.Evaluate(c.Request.Context(), &policy.InferenceRequest{
		Model:                req.Model,
		EstimatedInputTokens: inputEst,
		TeamID:               claims.TeamID,
	}, claims.TeamPriority, tp)

	if !decision.Allowed {
		middleware.RecordRejection(claims.TeamName, decision.RejectReason)
		if decision.QueueInstead {
			c.Header("Retry-After", "5")
			abortErr(c, http.StatusTooManyRequests, decision.RejectReason, "Request queued — retry shortly")
			return
		}
		status := http.StatusForbidden
		if decision.RejectReason == "rate_limit_exceeded" || decision.RejectReason == "daily_quota_exceeded" {
			status = http.StatusTooManyRequests
		}
		abortErr(c, status, decision.RejectReason, "Request rejected by policy engine")
		return
	}

	// ── 4. Prompt policy (system prompt injection, PII, content filter) ────
	ppDecision := h.promptPolicy.Evaluate(c.Request.Context(), claims.OrgID, claims.TeamID, req.Model, &req)
	if !ppDecision.Allowed {
		abortErr(c, http.StatusForbidden, "prompt_policy_blocked", ppDecision.BlockReason)
		return
	}
	if ppDecision.ModifiedRequest != nil {
		req = *ppDecision.ModifiedRequest
	}

	// ── 5. Resolve endpoint ────────────────────────────────────────────────
	ep, backend, err := h.registry.ResolveWithFailover(req.Model, maxFailoverAttempts)
	if err != nil {
		// Registry miss — try lazy-loading via the runtime activator.
		if h.activator != nil {
			h.log.Info("registry miss — triggering lazy-load",
				zap.String("model", req.Model),
			)
			// On the very first cold-start the request can take minutes.
			// Return 503 with Retry-After so the client retries; don't hold
			// the HTTP connection for the full cold-start duration.
			warmup, warmErr := h.activator.EnsureRunning(c.Request.Context(), req.Model)
			if warmErr != nil {
				h.log.Warn("activator failed",
					zap.String("model", req.Model),
					zap.Error(warmErr),
				)
				abortErr(c, http.StatusServiceUnavailable, "model_starting",
					fmt.Sprintf("model %q is starting up, please retry in a moment: %s", req.Model, warmErr.Error()))
				return
			}
			// Record how long the cold start took.
			c.Header("X-Nexus-Warmup-Ms", fmt.Sprintf("%d", warmup.WarmupMs))
			// Re-resolve now that the model is warm.
			ep, backend, err = h.registry.ResolveWithFailover(req.Model, maxFailoverAttempts)
			if err != nil {
				abortErr(c, http.StatusServiceUnavailable, "no_healthy_endpoint",
					fmt.Sprintf("model started but endpoint not yet routable: %s", err.Error()))
				return
			}
		} else {
			abortErr(c, http.StatusServiceUnavailable, "no_healthy_endpoint", err.Error())
			return
		}
	}

	// ── 6. Track inflight ─────────────────────────────────────────────────
	_ = h.policy.IncrementInflight(c.Request.Context(), claims.TeamID)
	middleware.ActiveRequests.WithLabelValues(claims.TeamName, req.Model).Inc()
	atomic.AddInt64(&ep.ActiveConns, 1)
	h.lifecycleMgr.RecordActivity(c.Request.Context(), ep.ID)
	if h.activator != nil {
		h.activator.RecordActivity(c.Request.Context(), ep.ID)
	}
	start := time.Now()
	defer func() {
		_ = h.policy.DecrementInflight(context.Background(), claims.TeamID)
		middleware.ActiveRequests.WithLabelValues(claims.TeamName, req.Model).Dec()
		atomic.AddInt64(&ep.ActiveConns, -1)
	}()

	c.Header("X-Nexus-Request-ID", c.GetString(middleware.RequestIDKey))
	c.Header("X-Nexus-Team-ID", claims.TeamID)
	c.Header("X-Nexus-Model", req.Model)
	c.Header("X-Nexus-Endpoint", ep.ID)

	chatReq := runtime.ChatRequest{Req: &req, EndpointURL: ep.URL}

	if req.Stream {
		h.streamChat(c, claims, req, chatReq, backend, ep, start)
	} else {
		h.syncChat(c, claims, req, chatReq, backend, ep, start)
	}
}

// Embeddings handles POST /v1/embeddings
func (h *Handler) Embeddings(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		abortErr(c, http.StatusUnauthorized, "unauthorized", "Missing authentication")
		return
	}
	var req models.EmbeddingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abortErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Model == "" {
		abortErr(c, http.StatusBadRequest, "missing_model", "Field 'model' is required")
		return
	}

	realModel, _ := h.aliasResolver.Resolve(c.Request.Context(), req.Model, claims.TeamID, claims.OrgID)
	req.Model = realModel
	c.Set("model", req.Model)

	tp := h.teamPolicy(claims.TeamID)
	decision := h.policy.Evaluate(c.Request.Context(), &policy.InferenceRequest{
		Model:  req.Model,
		TeamID: claims.TeamID,
	}, claims.TeamPriority, tp)
	if !decision.Allowed {
		middleware.RecordRejection(claims.TeamName, decision.RejectReason)
		abortErr(c, http.StatusForbidden, decision.RejectReason, "Request rejected by policy engine")
		return
	}

	ep, backend, err := h.registry.ResolveWithFailover(req.Model, maxFailoverAttempts)
	if err != nil {
		abortErr(c, http.StatusServiceUnavailable, "no_healthy_endpoint", err.Error())
		return
	}

	atomic.AddInt64(&ep.ActiveConns, 1)
	defer atomic.AddInt64(&ep.ActiveConns, -1)
	h.lifecycleMgr.RecordActivity(c.Request.Context(), ep.ID)

	start := time.Now()
	resp, err := backend.Embeddings(c.Request.Context(), runtime.EmbedRequest{Req: &req, EndpointURL: ep.URL})
	if err != nil {
		abortErr(c, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	latencyMs := int(time.Since(start).Milliseconds())
	_ = h.policy.RecordTokenUsage(context.Background(), claims.TeamID, resp.Usage.TotalTokens, 0)
	middleware.RecordTokens(claims.TeamName, req.Model, resp.Usage.TotalTokens, 0)
	h.usageTracker.Record(context.Background(), usage.Event{
		OrgID: claims.OrgID, TeamID: claims.TeamID, ModelName: req.Model,
		EndpointID: ep.ID, PromptTokens: resp.Usage.TotalTokens,
		LatencyMs: latencyMs, Status: "success",
	})
	c.JSON(http.StatusOK, resp)
}

// Models handles GET /v1/models
func (h *Handler) Models(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		abortErr(c, http.StatusUnauthorized, "unauthorized", "Missing authentication")
		return
	}
	registered := make(map[string]bool)
	for _, name := range h.registry.ListModels() {
		registered[name] = true
	}
	var data []models.ModelObject
	now := time.Now().Unix()
	for _, modelName := range claims.Permissions {
		if registered[modelName] {
			data = append(data, models.ModelObject{
				ID: modelName, Object: "model", Created: now, OwnedBy: "nexusllm",
			})
		}
	}
	c.JSON(http.StatusOK, models.ModelListResponse{Object: "list", Data: data})
}

// ─── private ──────────────────────────────────────────────────────────────────

func (h *Handler) syncChat(
	c *gin.Context,
	claims *auth.TeamClaims,
	req models.InferenceRequest,
	chatReq runtime.ChatRequest,
	backend runtime.Backend,
	ep *runtime.Endpoint,
	start time.Time,
) {
	resp, err := backend.Chat(c.Request.Context(), chatReq)
	if err != nil {
		abortErr(c, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	var chatResp models.ChatCompletionResponse
	if err := json.Unmarshal(resp.Body, &chatResp); err != nil {
		abortErr(c, http.StatusBadGateway, "parse_error", "Failed to parse upstream response")
		return
	}

	latencyMs := int(time.Since(start).Milliseconds())
	_ = h.policy.RecordTokenUsage(context.Background(), claims.TeamID,
		chatResp.Usage.PromptTokens, chatResp.Usage.CompletionTokens)
	middleware.RecordTokens(claims.TeamName, req.Model,
		chatResp.Usage.PromptTokens, chatResp.Usage.CompletionTokens)
	projID, projName, projPriority := h.lookupProjectContext(context.Background(), req.Model)
	h.usageTracker.Record(context.Background(), usage.Event{
		OrgID: claims.OrgID, TeamID: claims.TeamID, ModelName: req.Model,
		EndpointID: ep.ID, PromptTokens: chatResp.Usage.PromptTokens,
		CompletionTokens: chatResp.Usage.CompletionTokens,
		TotalTokens:      chatResp.Usage.TotalTokens,
		LatencyMs:        latencyMs, Status: "success",
		ProjectID: projID, ProjectName: projName, ProjectPriority: projPriority,
	})
	c.JSON(resp.StatusCode, chatResp)
}

func (h *Handler) streamChat(
	c *gin.Context,
	claims *auth.TeamClaims,
	req models.InferenceRequest,
	chatReq runtime.ChatRequest,
	backend runtime.Backend,
	ep *runtime.Endpoint,
	start time.Time,
) {
	resp, err := backend.Chat(c.Request.Context(), chatReq)
	if err != nil {
		abortErr(c, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	if resp.Stream == nil {
		abortErr(c, http.StatusBadGateway, "no_stream", "Backend did not return a stream")
		return
	}
	defer resp.Stream.Close()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	firstToken := false
	promptTokens, completionTokens := 0, 0
	flusher, canFlush := c.Writer.(http.Flusher)

	for {
		line, err := resp.Stream.ReadLine()
		if err != nil {
			break
		}
		if !firstToken && strings.HasPrefix(line, "data:") {
			middleware.ObserveTTFT(claims.TeamName, req.Model, time.Since(start))
			firstToken = true
		}
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload != "[DONE]" {
				var chunk models.ChatCompletionResponse
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					promptTokens += chunk.Usage.PromptTokens
					completionTokens += chunk.Usage.CompletionTokens
				}
			}
		}
		fmt.Fprintf(c.Writer, "%s\n", line)
		if canFlush {
			flusher.Flush()
		}
		if line == "data: [DONE]" {
			break
		}
	}

	latencyMs := int(time.Since(start).Milliseconds())
	if promptTokens+completionTokens > 0 {
		_ = h.policy.RecordTokenUsage(context.Background(), claims.TeamID, promptTokens, completionTokens)
		middleware.RecordTokens(claims.TeamName, req.Model, promptTokens, completionTokens)
	}
	projID, projName, projPriority := h.lookupProjectContext(context.Background(), req.Model)
	h.usageTracker.Record(context.Background(), usage.Event{
		OrgID: claims.OrgID, TeamID: claims.TeamID, ModelName: req.Model,
		EndpointID: ep.ID, PromptTokens: promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
		LatencyMs:        latencyMs, Status: "success",
		ProjectID: projID, ProjectName: projName, ProjectPriority: projPriority,
	})
}

func (h *Handler) teamPolicy(teamID string) *policy.TeamPolicy {
	if tp, ok := h.teamPolicies[teamID]; ok {
		return tp
	}
	return &policy.TeamPolicy{RPMLimit: 100, TPDLimit: 1_000_000, MaxConcurrent: 10, MaxContextTokens: 8192}
}

func estimateTokens(messages []models.Message) int {
	total := 0
	for _, m := range messages {
		if s, ok := m.Content.(string); ok {
			total += len(s) / 4
		}
		total += 4
	}
	return total
}

func abortErr(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, models.ErrorResponse{
		Error: models.ErrorDetail{Message: msg, Type: "gateway_error", Code: code},
	})
}

// keep compiler happy — transitively used
var _ = bytes.NewReader
var _ = bufio.NewReader
