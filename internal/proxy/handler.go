// Package proxy handles OpenAI-compatible inference requests.
// Request pipeline:
//
//	Auth → Gateway Policy → Alias Resolution → Prompt Policy → Registry → Activator (on miss) → Backend
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
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
	activator     runtimemgr.Activator
	usageTracker  *usage.Tracker
	log           *zap.Logger
	mu            sync.RWMutex
	teamPolicies  map[string]*policy.TeamPolicy
	httpClient    *http.Client
	db            *sqlx.DB
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

// lookupProjectContext returns project_id, project_name, project_priority, project_priority_weight.
// When claims already have project context (API key scoped to a project), returns that directly
// without a DB round-trip. Falls back to querying by model name for legacy models.
func (h *Handler) lookupProjectContext(ctx context.Context, modelName string, claims *auth.TeamClaims) (projectID, projectName, projectPriority *string, projectPriorityWeight *int) {
	// Fast path: project context already in claims (API key scoped to project).
	if claims != nil && claims.ProjectID != "" {
		pid := claims.ProjectID
		pname := claims.ProjectName
		ppw := claims.ProjectPriorityWeight
		return &pid, &pname, nil, &ppw
	}
	// Slow path: look up by model→project relationship.
	if h.db == nil {
		return nil, nil, nil, nil
	}
	var row struct {
		ProjectID             *string `db:"project_id"`
		ProjectName           *string `db:"project_name"`
		ProjectPriority       *string `db:"project_priority"`
		ProjectPriorityWeight *int    `db:"project_priority_weight"`
	}
	err := h.db.GetContext(ctx, &row, `
		SELECT p.id::text       AS project_id,
		       p.name            AS project_name,
		       p.priority        AS project_priority,
		       p.priority_weight AS project_priority_weight
		FROM models m
		JOIN projects p ON p.id = m.project_id
		WHERE m.name = $1 AND m.enabled = TRUE
		LIMIT 1`, modelName)
	if err != nil {
		return nil, nil, nil, nil
	}
	return row.ProjectID, row.ProjectName, row.ProjectPriority, row.ProjectPriorityWeight
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

	// ── 1b. Project override via header or request body ────────────────────
	// X-Nexus-Project: <project-name-or-id> allows callers to specify which
	// project this request belongs to, overriding the API key's project scope.
	// This is useful when a team key is shared across projects.
	if projectHdr := c.GetHeader("X-Nexus-Project"); projectHdr != "" && h.db != nil {
		var projRow struct {
			ID             string `db:"id"`
			Name           string `db:"name"`
			PriorityWeight int    `db:"priority_weight"`
		}
		// Resolve by name (within team) or by UUID
		lookupErr := h.db.GetContext(c.Request.Context(), &projRow, `
			SELECT id::text, name, priority_weight
			FROM projects
			WHERE (name = $1 OR id::text = $1)
			  AND team_id = $2
			  AND status = 'active'
			LIMIT 1`, projectHdr, claims.TeamID)
		if lookupErr == nil {
			// Shallow-copy claims with project override so we don't mutate shared state.
			overriddenClaims := *claims
			overriddenClaims.ProjectID = projRow.ID
			overriddenClaims.ProjectName = projRow.Name
			overriddenClaims.ProjectPriorityWeight = projRow.PriorityWeight
			claims = &overriddenClaims
		}
	}

	// ── 2. Gateway policy (temperature cap, tool restrictions, etc.) ───────
	inputEst := estimateTokens(req.Messages)
	if v := h.gwPolicy.Enforce(c.Request.Context(), claims.OrgID, claims.TeamID, "", &req, inputEst); v != nil {
		abortErr(c, http.StatusForbidden, v.Code, v.Message)
		return
	}

	// ── 3. Infrastructure policy (rate limit, quota, ACL) ─────────────────
	// Use project priority weight when available; fall back to team priority.
	// This ensures high-priority projects are served before low-priority ones
	// when the concurrency limit is reached.
	effectivePriority := claims.TeamPriority
	if claims.ProjectPriorityWeight > 0 {
		effectivePriority = claims.ProjectPriorityWeight
	}
	tp := h.teamPolicy(claims.TeamID)
	decision := h.policy.Evaluate(c.Request.Context(), &policy.InferenceRequest{
		Model:                req.Model,
		EstimatedInputTokens: inputEst,
		TeamID:               claims.TeamID,
		ProjectID:            claims.ProjectID, // project-level limits when scoped
	}, effectivePriority, tp)

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
	_ = h.policy.IncrementProjectInflight(c.Request.Context(), claims.ProjectID)
	middleware.ActiveRequests.WithLabelValues(claims.TeamName, req.Model).Inc()
	atomic.AddInt64(&ep.ActiveConns, 1)
	h.lifecycleMgr.RecordActivity(c.Request.Context(), ep.ID)
	if h.activator != nil {
		h.activator.RecordActivity(c.Request.Context(), ep.ID)
	}
	start := time.Now()
	defer func() {
		_ = h.policy.DecrementInflight(context.Background(), claims.TeamID)
		_ = h.policy.DecrementProjectInflight(context.Background(), claims.ProjectID)
		middleware.ActiveRequests.WithLabelValues(claims.TeamName, req.Model).Dec()
		atomic.AddInt64(&ep.ActiveConns, -1)
	}()

	c.Header("X-Nexus-Request-ID", c.GetString(middleware.RequestIDKey))
	c.Header("X-Nexus-Team-ID", claims.TeamID)
	c.Header("X-Nexus-Model", req.Model)
	c.Header("X-Nexus-Endpoint", ep.ID)

	chatReq := runtime.ChatRequest{Req: &req, EndpointURL: ep.URL}

	if req.Stream {
		// For streaming, try up to maxFailoverAttempts endpoints.
		// If the first one is unreachable (connection refused), mark it down
		// and retry with the next healthy endpoint.
		for attempt := 0; attempt < maxFailoverAttempts; attempt++ {
			if attempt > 0 {
				// Previous endpoint failed — re-resolve to get a different one.
				ep2, b2, rerr := h.registry.ResolveWithFailover(req.Model, maxFailoverAttempts)
				if rerr != nil {
					break // no more healthy endpoints
				}
				ep = ep2
				backend = b2
				chatReq.EndpointURL = ep.URL
			}
			if !h.streamChat(c, claims, req, chatReq, backend, ep, start) {
				// Connection-level error — mark endpoint down and try again.
				h.registry.UpdateEndpointHealth(c.Request.Context(), ep.ID, req.Model, runtime.EndpointHealth{
					Status:    runtime.StatusDown,
					CheckedAt: time.Now(),
					Error:     "connection refused on inference request",
				})
				ep.SetStatus(runtime.StatusDown)
				continue
			}
			return
		}
		// All attempts failed or already written — nothing more to do.
	} else {
		// Sync path: try up to maxFailoverAttempts endpoints.
		for attempt := 0; attempt < maxFailoverAttempts; attempt++ {
			if attempt > 0 {
				ep2, b2, rerr := h.registry.ResolveWithFailover(req.Model, maxFailoverAttempts)
				if rerr != nil {
					abortErr(c, http.StatusServiceUnavailable, "no_healthy_endpoint",
						"all endpoints unreachable after upstream failures")
					return
				}
				ep = ep2
				backend = b2
				chatReq.EndpointURL = ep.URL
			}
			done := h.syncChat(c, claims, req, chatReq, backend, ep, start)
			if done {
				return
			}
			// Connection-level failure — mark this endpoint down and try next.
			h.registry.UpdateEndpointHealth(c.Request.Context(), ep.ID, req.Model, runtime.EndpointHealth{
				Status:    runtime.StatusDown,
				CheckedAt: time.Now(),
				Error:     "connection refused on inference request",
			})
			ep.SetStatus(runtime.StatusDown)
		}
		abortErr(c, http.StatusBadGateway, "upstream_error",
			"all available endpoints returned connection errors")
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
) (ok bool) {
	resp, err := backend.Chat(c.Request.Context(), chatReq)
	if err != nil {
		// Distinguish connection-level failures (retry-able) from upstream errors.
		if isConnectError(err) {
			return false // caller will mark endpoint down and retry
		}
		abortErr(c, http.StatusBadGateway, "upstream_error", err.Error())
		return true // wrote response, do not retry
	}

	var chatResp models.ChatCompletionResponse
	if err := json.Unmarshal(resp.Body, &chatResp); err != nil {
		abortErr(c, http.StatusBadGateway, "parse_error", "Failed to parse upstream response")
		return true
	}

	latencyMs := int(time.Since(start).Milliseconds())
	_ = h.policy.RecordTokenUsage(context.Background(), claims.TeamID,
		chatResp.Usage.PromptTokens, chatResp.Usage.CompletionTokens)
	_ = h.policy.RecordProjectTokenUsage(context.Background(), claims.ProjectID,
		chatResp.Usage.PromptTokens, chatResp.Usage.CompletionTokens)
	middleware.RecordTokens(claims.TeamName, req.Model,
		chatResp.Usage.PromptTokens, chatResp.Usage.CompletionTokens)
	projID, projName, projPriority, projPriorityWeight := h.lookupProjectContext(context.Background(), req.Model, claims)
	h.usageTracker.Record(context.Background(), usage.Event{
		OrgID: claims.OrgID, TeamID: claims.TeamID, ModelName: req.Model,
		EndpointID: ep.ID, PromptTokens: chatResp.Usage.PromptTokens,
		CompletionTokens: chatResp.Usage.CompletionTokens,
		TotalTokens:      chatResp.Usage.TotalTokens,
		LatencyMs:        latencyMs, Status: "success",
		ProjectID: projID, ProjectName: projName, ProjectPriority: projPriority,
		ProjectPriorityWeight: projPriorityWeight,
	})
	c.JSON(resp.StatusCode, chatResp)
	return true
}

func (h *Handler) streamChat(
	c *gin.Context,
	claims *auth.TeamClaims,
	req models.InferenceRequest,
	chatReq runtime.ChatRequest,
	backend runtime.Backend,
	ep *runtime.Endpoint,
	start time.Time,
) (ok bool) {
	resp, err := backend.Chat(c.Request.Context(), chatReq)
	if err != nil {
		if isConnectError(err) {
			return false // caller will mark endpoint down and retry
		}
		abortErr(c, http.StatusBadGateway, "upstream_error", err.Error())
		return true
	}
	if resp.Stream == nil {
		abortErr(c, http.StatusBadGateway, "no_stream", "Backend did not return a stream")
		return true
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
		_ = h.policy.RecordProjectTokenUsage(context.Background(), claims.ProjectID, promptTokens, completionTokens)
		middleware.RecordTokens(claims.TeamName, req.Model, promptTokens, completionTokens)
	}
	projID, projName, projPriority, projPriorityWeight := h.lookupProjectContext(context.Background(), req.Model, claims)
	h.usageTracker.Record(context.Background(), usage.Event{
		OrgID: claims.OrgID, TeamID: claims.TeamID, ModelName: req.Model,
		EndpointID: ep.ID, PromptTokens: promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
		LatencyMs:        latencyMs, Status: "success",
		ProjectID: projID, ProjectName: projName, ProjectPriority: projPriority,
		ProjectPriorityWeight: projPriorityWeight,
	})
	return true
}

// isConnectError returns true for connection-refused and similar network errors
// that indicate the upstream server is not reachable (as opposed to returning
// an HTTP error code). These errors are safe to retry on a different endpoint.
func isConnectError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no route to host") ||
		strings.Contains(msg, "connect: connection refused") ||
		strings.Contains(msg, "dial tcp") ||
		strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "context deadline exceeded")
}

// SwapTeamPolicies atomically replaces the in-memory team policy map.
// Called by the gateway's 60-second reload goroutine. Safe for concurrent use.
func (h *Handler) SwapTeamPolicies(fresh map[string]*policy.TeamPolicy) {
	h.mu.Lock()
	h.teamPolicies = fresh
	h.mu.Unlock()
}

func (h *Handler) teamPolicy(teamID string) *policy.TeamPolicy {
	h.mu.RLock()
	tp := h.teamPolicies[teamID]
	h.mu.RUnlock()
	if tp != nil {
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
