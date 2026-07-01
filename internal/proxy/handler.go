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
	"io"
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
	"github.com/nexusllm/nexusllm/internal/thinking"
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
	thinkingRes   *thinking.Resolver
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
			// No Timeout here — streaming responses can take arbitrarily long.
			// Per-request deadlines are managed via context (from Gin's request context).
			// A global Timeout would kill long-running inference streams mid-response.
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
				// ResponseHeaderTimeout guards against a backend that accepts
				// the connection but never sends response headers (stuck server).
				ResponseHeaderTimeout: 2 * time.Minute,
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
	h.thinkingRes = thinking.NewResolver(db)
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

	// Read body once so we can both log it and parse it.
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		abortErr(c, http.StatusBadRequest, "read_error", "Failed to read request body")
		return
	}
	// Log the raw request at DEBUG level so we can diagnose client compatibility issues.
	h.log.Debug("incoming chat request",
		zap.String("body", string(rawBody)),
		zap.String("content_type", c.GetHeader("Content-Type")),
		zap.String("user_agent", c.GetHeader("User-Agent")),
	)
	// Restore body for binding.
	c.Request.Body = io.NopCloser(bytes.NewReader(rawBody))

	var req models.InferenceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.log.Warn("request binding failed",
			zap.Error(err),
			zap.String("body", string(rawBody)),
		)
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
			// Check if model is already starting up (in-flight warm-up).
			// If it is, return 503 + Retry-After immediately so the client
			// retries rather than holding a long HTTP connection open.
			// Holding the connection open causes the HTTP write timeout to
			// fire (default 30s), which the client receives as a 400/EOF.
			//
			// We use a short probe context (3s) to detect if the model is
			// already healthy right now. If it responds immediately it was
			// just slow to register in the registry — proceed. Otherwise
			// kick off the warm-up in the background and tell the client
			// to retry in 10 seconds.
			probeCtx, probeCancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
			_, probeErr := h.activator.EnsureRunning(probeCtx, req.Model)
			probeCancel()

			if probeErr != nil {
				// Model not yet ready — start it in background and tell client to retry.
				h.log.Info("cold-start triggered — returning 503 Retry-After",
					zap.String("model", req.Model),
				)
				go func() {
					bgCtx, bgCancel := context.WithTimeout(context.Background(), h.coldStartTimeout())
					defer bgCancel()
					if _, startErr := h.activator.EnsureRunning(bgCtx, req.Model); startErr != nil {
						h.log.Warn("background cold-start failed",
							zap.String("model", req.Model),
							zap.Error(startErr),
						)
					}
				}()
				c.Header("Retry-After", "10")
				abortErr(c, http.StatusServiceUnavailable, "model_starting",
					fmt.Sprintf("model %q is starting up, please retry in ~10 seconds", req.Model))
				return
			}
			// Probe succeeded immediately — re-resolve and continue.
			c.Header("X-Nexus-Warmup-Ms", "0")
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

	// ── Backend compatibility sanitization + Thinking mode resolution ─────
	// Order matters:
	//   1. Sanitize first (strips OpenAI-only fields that cause 400s on local backends)
	//   2. Thinking injection second (may add/modify chat_template_kwargs)
	// Both work on copies of req so the original is preserved for logging/retry.
	sanitized := sanitizeForBackend(req, backend.Type())
	chatReq.Req = &sanitized

	var thinkingOn bool
	var thinkingCaps thinking.ModelCaps
	if h.thinkingRes != nil {
		thinkingCaps = h.thinkingRes.LoadCaps(c.Request.Context(), req.Model)
		if thinkingCaps.SupportsThinking {
			thinkingOn = thinking.ResolveMode(&req, thinkingCaps)
			// Inject into the sanitized copy, not the original req.
			injected := thinking.InjectThinkingControl(sanitized, thinkingOn, thinkingCaps)
			chatReq.Req = &injected
			mode := "fast"
			if thinkingOn {
				mode = "thinking"
			}
			middleware.ThinkingRequestsTotal.WithLabelValues(
				claims.TeamName, req.Model, mode).Inc()
		} else {
			thinkingOn = thinking.ResolveMode(&req, thinkingCaps) // always false when !SupportsThinking
			_ = thinkingOn
		}
	}

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
			done, emptyContent := h.syncChat(c, claims, req, chatReq, backend, ep, start, thinkingCaps)
			if done && emptyContent && thinkingOn && h.thinkingRes != nil {
				// Thinking consumed all tokens — retry once with thinking disabled.
				h.log.Info("thinking produced empty content — retrying with thinking disabled",
					zap.String("model", req.Model),
				)
				disabledReq := thinking.InjectThinkingControl(req, false, thinkingCaps)
				retryReq := runtime.ChatRequest{Req: &disabledReq, EndpointURL: ep.URL}
				c.Header("X-Nexus-Thinking-Retry", "1")
				middleware.ThinkingRequestsTotal.WithLabelValues(
					claims.TeamName, req.Model, "fast_retry").Inc()
				h.syncChat(c, claims, req, retryReq, backend, ep, start, thinkingCaps)
				return
			}
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
	// Load stable created_at timestamps from the DB so the value is
	// deterministic across calls. VS Code extensions (Cline, Continue) cache
	// the model list and treat a changing `created` as a model replacement.
	modelCreatedAt := make(map[string]int64)
	if h.db != nil {
		type row struct {
			Name      string    `db:"name"`
			CreatedAt time.Time `db:"created_at"`
		}
		var rows []row
		_ = h.db.SelectContext(c.Request.Context(), &rows,
			`SELECT name, created_at FROM models WHERE enabled = TRUE`)
		for _, r := range rows {
			modelCreatedAt[r.Name] = r.CreatedAt.Unix()
		}
	}
	fallbackCreated := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Unix()

	var data []models.ModelObject
	for _, modelName := range claims.Permissions {
		if registered[modelName] {
			created, ok := modelCreatedAt[modelName]
			if !ok {
				created = fallbackCreated
			}
			data = append(data, models.ModelObject{
				ID: modelName, Object: "model", Created: created, OwnedBy: "nexusllm",
			})
		}
	}
	c.JSON(http.StatusOK, models.ModelListResponse{Object: "list", Data: data})
}

// LegacyCompletions handles POST /v1/completions (legacy text completions API).
// Several VS Code extensions (Roo Code, Continue fill-in-middle mode) still
// call this endpoint. We translate it to a chat completions request so it
// works transparently with all registered models.
func (h *Handler) LegacyCompletions(c *gin.Context) {
	var req struct {
		Model       string      `json:"model"`
		Prompt      interface{} `json:"prompt"` // string or []string
		MaxTokens   *int        `json:"max_tokens,omitempty"`
		Temperature *float64    `json:"temperature,omitempty"`
		Stream      bool        `json:"stream"`
		Stop        interface{} `json:"stop,omitempty"`
		Suffix      string      `json:"suffix,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		abortErr(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// Extract the prompt string.
	var promptStr string
	switch v := req.Prompt.(type) {
	case string:
		promptStr = v
	case []interface{}:
		if len(v) > 0 {
			if s, ok := v[0].(string); ok {
				promptStr = s
			}
		}
	}

	// Build a chat-completions request with the prompt as a user message.
	chatReq := models.InferenceRequest{
		Model:       req.Model,
		Messages:    []models.Message{{Role: "user", Content: promptStr}},
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stream:      req.Stream,
		Stop:        req.Stop,
	}
	// Swap out the parsed body so ChatCompletions can process it.
	body, _ := json.Marshal(chatReq)
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	h.ChatCompletions(c)
}

// ModelByID handles GET /v1/models/:model_id.
// Cline, Continue, and Kilo Code all call this endpoint to verify a model
// exists before submitting a request. Without it they receive 404 and fall
// back to disabled mode or show a configuration error.
func (h *Handler) ModelByID(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		abortErr(c, http.StatusUnauthorized, "unauthorized", "Missing authentication")
		return
	}
	modelID := c.Param("model_id")

	// Check the model is registered and the caller has permission.
	registered := make(map[string]bool)
	for _, name := range h.registry.ListModels() {
		registered[name] = true
	}
	allowed := false
	for _, p := range claims.Permissions {
		if p == modelID {
			allowed = true
			break
		}
	}
	if !allowed || !registered[modelID] {
		c.JSON(http.StatusNotFound, models.ErrorResponse{
			Error: models.ErrorDetail{
				Message: "The model '" + modelID + "' does not exist",
				Type:    "invalid_request_error",
				Code:    "model_not_found",
			},
		})
		return
	}

	// Look up stable created_at from DB.
	var createdAt int64
	if h.db != nil {
		_ = h.db.QueryRowContext(c.Request.Context(),
			`SELECT EXTRACT(EPOCH FROM created_at)::bigint FROM models WHERE name=$1 AND enabled=TRUE`,
			modelID,
		).Scan(&createdAt)
	}
	if createdAt == 0 {
		createdAt = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	}

	c.JSON(http.StatusOK, models.ModelObject{
		ID: modelID, Object: "model", Created: createdAt, OwnedBy: "nexusllm",
	})
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
	thinkingCaps thinking.ModelCaps,
) (ok bool, emptyContent bool) {
	resp, err := backend.Chat(c.Request.Context(), chatReq)
	if err != nil {
		if isConnectError(err) {
			return false, false // caller will mark endpoint down and retry
		}
		abortErr(c, http.StatusBadGateway, "upstream_error", err.Error())
		return true, false
	}

	var chatResp models.ChatCompletionResponse
	if err := json.Unmarshal(resp.Body, &chatResp); err != nil {
		abortErr(c, http.StatusBadGateway, "parse_error", "Failed to parse upstream response")
		return true, false
	}

	// Normalize the model field: always echo back the model name the client
	// sent. llama.cpp may report an internal path or a different identifier.
	// Cline validates response.model === request.model.
	if chatResp.Model == "" || chatResp.Model != req.Model {
		chatResp.Model = req.Model
	}

	// Inject a stable system_fingerprint so clients that require it don't
	// receive an empty/missing field. Some OpenAI SDK versions treat an absent
	// system_fingerprint as a protocol error.
	if chatResp.SystemFingerprint == "" {
		chatResp.SystemFingerprint = "nexusllm-v1"
	}

	// llama.cpp returns tool_calls[].function.arguments as a JSON object,
	// but the OpenAI spec requires it to be a JSON-encoded string.
	// Normalise it here so the client always gets a spec-compliant response.
	normalizeToolCallArguments(&chatResp)

	latencyMs := int(time.Since(start).Milliseconds())

	// ── Thinking token accounting ────────────────────────────────────────
	// Detect and record thinking tokens. Also check for empty visible content
	// so the caller can trigger a retry with thinking disabled.
	contentEmpty := false
	if thinkingCaps.SupportsThinking && len(chatResp.Choices) > 0 {
		msg := chatResp.Choices[0].Message
		if msg != nil {
			if s, ok := msg.Content.(string); ok {
				thinkTok, _ := thinking.EstimateThinkingTokens(s)
				if thinkTok > 0 {
					middleware.ThinkingTokensTotal.WithLabelValues(
						claims.TeamName, req.Model).Add(float64(thinkTok))
					chatResp.Usage.ThinkingTokens = thinkTok
				}
				contentEmpty = thinking.IsEmptyVisible(s)
			}
		}
	}

	_ = h.policy.RecordTokenUsage(context.Background(), claims.TeamID,
		chatResp.Usage.PromptTokens, chatResp.Usage.CompletionTokens)
	_ = h.policy.RecordProjectTokenUsage(context.Background(), claims.ProjectID,
		chatResp.Usage.PromptTokens, chatResp.Usage.CompletionTokens)
	middleware.RecordTokens(claims.TeamName, req.Model,
		chatResp.Usage.PromptTokens, chatResp.Usage.CompletionTokens)

	visibleTok := chatResp.Usage.CompletionTokens - chatResp.Usage.ThinkingTokens
	if visibleTok < 0 {
		visibleTok = 0
	}
	if thinkingCaps.SupportsThinking {
		middleware.VisibleCompletionTokensTotal.WithLabelValues(
			claims.TeamName, req.Model).Add(float64(visibleTok))
	}

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
	return true, contentEmpty
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

	// Check for non-2xx from the upstream BEFORE setting SSE headers.
	// If the upstream returns 400/500, the stream body contains an error
	// message, not SSE events. Forward the status and body directly.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.Stream != nil {
			// Read the error body from the stream and forward it as JSON.
			var errBody strings.Builder
			for {
				line, readErr := resp.Stream.ReadLine()
				if readErr != nil {
					break
				}
				errBody.WriteString(line)
			}
			resp.Stream.Close()
			bodyStr := strings.TrimSpace(errBody.String())
			if bodyStr == "" {
				abortErr(c, resp.StatusCode, "upstream_error",
					fmt.Sprintf("upstream returned HTTP %d with no body", resp.StatusCode))
			} else {
				c.Data(resp.StatusCode, "application/json", []byte(bodyStr))
			}
		} else {
			abortErr(c, resp.StatusCode, "upstream_error",
				fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode))
		}
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

	flusher, canFlush := c.Writer.(http.Flusher)

	// Track whether the client requested usage in the final chunk.
	// Kilo Code (via OpenAI SDK) always sends stream_options.include_usage=true.
	// If the upstream doesn't emit a usage chunk, we synthesize one.
	wantsUsage := req.StreamOptions != nil
	var (
		firstToken       bool
		promptTokens     int
		completionTokens int
		seenUsageChunk   bool   // true if upstream already sent a usage chunk
		streamID         string // captured from first chunk for synthesized usage chunk
		streamModel      string
		streamCreated    int64
		done             bool
	)

	writeSSE := func(data string) {
		fmt.Fprintf(c.Writer, "data: %s\n\n", data)
		if canFlush {
			flusher.Flush()
		}
	}

	for {
		line, readErr := resp.Stream.ReadLine()
		if readErr != nil {
			// Stream ended — may be normal EOF or a mid-stream error.
			// If we haven't sent [DONE] yet, emit a mid-stream error event
			// per the Kilo Code spec: finish_reason="error".
			if !done {
				errChunk := fmt.Sprintf(
					`{"id":%q,"object":"chat.completion.chunk","created":%d,"model":%q,`+
						`"choices":[{"index":0,"delta":{"content":""},"finish_reason":"error"}]}`,
					streamID, streamCreated, streamModel,
				)
				writeSSE(errChunk)
				fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
				if canFlush {
					flusher.Flush()
				}
			}
			break
		}

		if !strings.HasPrefix(line, "data:") {
			// SSE comment, field line, or blank separator — forward as-is.
			// Blank lines between events are already handled by our \n\n framing,
			// so just skip them to avoid double-spacing.
			if line != "" {
				fmt.Fprintf(c.Writer, "%s\n", line)
				if canFlush {
					flusher.Flush()
				}
			}
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))

		// Robust [DONE] detection — handles "data: [DONE]", "data:[DONE]", etc.
		if payload == "[DONE]" {
			done = true
			// Synthesize a usage chunk before [DONE] if the client requested it
			// and the upstream never sent one. This satisfies the Kilo Code / OpenAI
			// SDK expectation that usage is always present in the final stream chunk.
			if wantsUsage && !seenUsageChunk && (promptTokens+completionTokens) > 0 {
				usageChunk := fmt.Sprintf(
					`{"id":%q,"object":"chat.completion.chunk","created":%d,"model":%q,`+
						`"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
					streamID, streamCreated, streamModel,
					promptTokens, completionTokens, promptTokens+completionTokens,
				)
				writeSSE(usageChunk)
			}
			fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
			if canFlush {
				flusher.Flush()
			}
			break
		}

		if !firstToken {
			middleware.ObserveTTFT(claims.TeamName, req.Model, time.Since(start))
			firstToken = true
		}

		// Parse chunk to accumulate token counts and capture stream metadata.
		var chunk models.ChatCompletionResponse
		if json.Unmarshal([]byte(payload), &chunk) == nil {
			if streamID == "" && chunk.ID != "" {
				streamID = chunk.ID
				// Always use the request model name — upstreams (llama.cpp) may
				// emit their own internal identifier which won't match what the
				// client sent. Cline/Continue validate that chunk.model === request.model.
				if chunk.Model != "" {
					streamModel = req.Model
				} else if streamModel == "" {
					streamModel = req.Model
				}
				streamCreated = chunk.Created
			}
			if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
				promptTokens += chunk.Usage.PromptTokens
				completionTokens += chunk.Usage.CompletionTokens
			}
			// Detect if upstream already sent a usage chunk (choices=[]).
			if len(chunk.Choices) == 0 && chunk.Usage.TotalTokens > 0 {
				seenUsageChunk = true
				promptTokens = chunk.Usage.PromptTokens
				completionTokens = chunk.Usage.CompletionTokens
			}
		}

		// Strip non-standard reasoning_content fields before forwarding.
		normalized, forward := models.NormalizeStreamChunk(payload)
		if !forward {
			continue // pure reasoning chunk — drop silently
		}
		writeSSE(normalized)
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

// normalizeToolCallArguments ensures tool_calls[].function.arguments is always
// a JSON-encoded string, as required by the OpenAI spec.
//
// llama.cpp (and some other local servers) return arguments as a raw JSON object:
//
//	{"location": "Tokyo"}
//
// The OpenAI spec requires it as a JSON-encoded string:
//
//	"{\"location\": \"Tokyo\"}"
//
// The OpenAI SDK and Kilo Code both expect the string form and will fail to
// parse tool calls if they receive an object.
func normalizeToolCallArguments(resp *models.ChatCompletionResponse) {
	for i := range resp.Choices {
		msg := resp.Choices[i].Message
		if msg == nil {
			continue
		}
		toolCalls, ok := msg.ToolCalls.([]interface{})
		if !ok {
			continue
		}
		for j := range toolCalls {
			tc, ok := toolCalls[j].(map[string]interface{})
			if !ok {
				continue
			}
			fn, ok := tc["function"].(map[string]interface{})
			if !ok {
				continue
			}
			args := fn["arguments"]
			if args == nil {
				continue
			}
			// If arguments is already a string, it's fine.
			if _, isStr := args.(string); isStr {
				continue
			}
			// It's an object/map — marshal it to a JSON string.
			encoded, err := json.Marshal(args)
			if err == nil {
				fn["arguments"] = string(encoded)
			}
		}
	}
}

func (h *Handler) coldStartTimeout() time.Duration {
	// Default to 20 minutes — large models (235B) take 10-15 min to load.
	// Can be overridden via NEXUS_RUNTIMEMGR_COLDSTARTTIMEOUT env var.
	return 20 * time.Minute
}

// sanitizeForBackend removes fields that are valid in the OpenAI API spec but
// cause local backends (llama.cpp, Ollama, TGI) to return 400 errors because
// they don't recognise them. Called on a copy of the request so the original
// is untouched for logging and retry purposes.
//
// Confirmed fields that cause llama.cpp to return 400:
//   - stream_options        (OpenAI SDK adds this automatically; llama.cpp rejects it)
//   - parallel_tool_calls   (OpenAI-only extension)
//   - service_tier          (OpenAI routing hint, meaningless for local servers)
//   - store                 (OpenAI storage API)
//
// Fields that are safe to forward even if not used:
//   - user, seed, logit_bias, response_format, stop — all handled or ignored
func sanitizeForBackend(req models.InferenceRequest, backendType runtime.BackendType) models.InferenceRequest {
	switch backendType {
	case runtime.BackendOpenAICompat:
		// True OpenAI-compatible remote provider — forward everything as-is.
		return req
	default:
		// Local backends: llama.cpp, vllm, ollama, tgi, cpu_native.
		// Strip fields that cause 400 "unknown parameter" errors.
		req.StreamOptions = nil
		req.ParallelToolCalls = nil
		req.ServiceTier = nil
		req.Store = nil
		return req
	}
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
