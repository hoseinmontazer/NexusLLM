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
	"github.com/nexusllm/nexusllm/internal/catalog"
	"github.com/nexusllm/nexusllm/internal/gatewaypolicy"
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
	registry      *runtime.Registry
	activator     runtimemgr.Activator
	usageTracker  *usage.Tracker
	capValidator  *CapabilityValidator // nil-safe: skipped when not set
	log           *zap.Logger
	mu            sync.RWMutex
	teamPolicies  map[string]*policy.TeamPolicy
	httpClient    *http.Client     // direct client (local endpoints + fallback)
	factory       *runtime.Factory // nil-safe; provides proxy-aware clients
	db            *sqlx.DB
	thinkingRes   *thinking.Resolver
	coldStartDur  time.Duration // 0 = use default (20 min)
	// startTracker deduplicates background cold-start goroutines across
	// concurrent HTTP retries. At most one background EnsureRunning goroutine
	// runs per model at a time; its lifetime is independent of any HTTP request.
	// Nil-safe: when not set the old probe-then-background path is used.
	startTracker *runtimemgr.StartTracker
	// virtualResolver resolves Mode-B catalog model names when the registry
	// has no matching pool entry. Nil-safe — skipped when not set.
	virtualResolver *catalog.VirtualModelResolver
}

// NewHandler constructs the proxy Handler.
func NewHandler(
	pe *policy.Engine,
	gwp *gatewaypolicy.Engine,
	pp *promptpolicy.Engine,
	ar *alias.Resolver,
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

// WithColdStartTimeout overrides the default 20-minute cold-start timeout.
// Should be set to match the RuntimeManager's configured ColdStartTimeout.
func (h *Handler) WithColdStartTimeout(d time.Duration) *Handler {
	h.coldStartDur = d
	return h
}

// WithDB attaches a database connection for project context enrichment.
func (h *Handler) WithDB(db *sqlx.DB) *Handler {
	h.db = db
	h.thinkingRes = thinking.NewResolver(db)
	return h
}

// WithCapabilityValidator attaches a CapabilityValidator to the handler.
// When set, every inference request is checked for model-endpoint compatibility
// before routing. Requests to endpoints the model does not support receive
// HTTP 400 with a structured error response before any backend interaction.
func (h *Handler) WithCapabilityValidator(cv *CapabilityValidator) *Handler {
	h.capValidator = cv
	return h
}

// WithFactory attaches a runtime.Factory so the handler can use proxy-aware
// HTTP clients for cloud endpoints. Call this when NEXUS_UPSTREAM_PROXY or
// per-model upstream_proxy may be set.
func (h *Handler) WithFactory(f *runtime.Factory) *Handler {
	h.factory = f
	return h
}

// WithVirtualResolver attaches a catalog.VirtualModelResolver for Mode-B
// catalog model resolution. When set, registry misses fall through to the
// resolver before the activator/503 path. Virtual models never trigger
// EnsureRunning — they are always considered routable.
func (h *Handler) WithVirtualResolver(r *catalog.VirtualModelResolver) *Handler {
	h.virtualResolver = r
	return h
}

// WithStartTracker attaches a StartTracker that prevents goroutine explosions
// during cold-start retry storms. Must be the same instance attached to all
// proxy handler methods (ChatCompletions, multiservice pipelineSetup, etc.).
// When set, at most one background EnsureRunning goroutine runs per model;
// HTTP retries during startup skip spawning and return 503 immediately.
// Call this in main() after constructing the activator.
func (h *Handler) WithStartTracker(t *runtimemgr.StartTracker) *Handler {
	h.startTracker = t
	return h
}

// clientFor returns the *http.Client to use for an upstream call.
//
//  1. ep.UpstreamProxy set           → proxy-aware client from factory
//  2. factory has a GlobalProxy set   → global proxy client (NEXUS_UPSTREAM_PROXY)
//  3. no proxy configured             → direct httpClient
func (h *Handler) clientFor(ep *runtime.Endpoint) *http.Client {
	if h.factory == nil {
		return h.httpClient
	}
	proxyURL := ep.UpstreamProxy
	if proxyURL == "" {
		proxyURL = h.factory.GlobalProxy()
	}
	return h.factory.ClientFor(proxyURL)
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

// resolveModelWithProvider applies the X-Nexus-Provider header shorthand.
//
// When a caller sends X-Nexus-Provider: openrouter together with
// model: "openai/gpt-chat-latest", the two are combined into the virtual
// model name "openrouter/openai/gpt-chat-latest" that the gateway's virtual
// resolver understands.
//
// Rules:
//  1. Header absent or empty → model is returned unchanged.
//  2. Model already starts with "<provider>/" → model is returned unchanged
//     (idempotent — prevents double-prefixing when the caller already uses
//     the full virtual name).
//  3. Otherwise → "<provider>/<model>" is returned.
func resolveModelWithProvider(c *gin.Context, model string) string {
	provider := strings.TrimSpace(c.GetHeader("X-Nexus-Provider"))
	if provider == "" {
		return model
	}
	prefix := provider + "/"
	if strings.HasPrefix(model, prefix) {
		return model // already prefixed — idempotent
	}
	return prefix + model
}

// ChatCompletions handles POST /v1/chat/completions
func (h *Handler) ChatCompletions(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		abortErr(c, http.StatusUnauthorized, "unauthorized", "Missing authentication")
		return
	}

	// Read body once so we can both parse it and have it available for logging.
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		abortErr(c, http.StatusBadRequest, "read_error", "Failed to read request body")
		return
	}
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

	// Apply X-Nexus-Provider shorthand before alias resolution.
	// Allows callers to send model: "openai/gpt-4o" + X-Nexus-Provider: openrouter
	// instead of the full virtual name "openrouter/openai/gpt-4o".
	req.Model = resolveModelWithProvider(c, req.Model)

	// ── 1. Alias resolution ────────────────────────────────────────────────
	realModel, err := h.aliasResolver.Resolve(c.Request.Context(), req.Model, claims.TeamID, claims.OrgID)
	if err != nil {
		realModel = req.Model
	}
	req.Model = realModel
	c.Set("model", realModel)

	// ── 1c. Capability validation ─────────────────────────────────────────
	// Ensure the model supports the requested endpoint before any policy,
	// quota, or backend interaction. The validator is engine-independent and
	// uses the registry's DB-backed capability metadata as the single source
	// of truth. This prevents e.g. a Whisper model from receiving a chat request.
	if h.capValidator != nil {
		if !h.capValidator.CheckAndAbort(c, realModel, c.FullPath()) {
			return
		}
	}

	// ── 1b. Project override via X-Nexus-Project header ──────────────────
	claims = h.applyProjectHeaderOverride(c, claims)

	// ── 2. Gateway policy (temperature cap, tool restrictions, etc.) ───────
	inputEst := estimateTokens(req.Messages)
	if v := h.gwPolicy.Enforce(c.Request.Context(), claims.OrgID, claims.ProjectID, claims.TeamID, claims.APIKeyID, &req, inputEst); v != nil {
		abortErr(c, http.StatusForbidden, v.Code, v.Message)
		return
	}

	// ── 3. Infrastructure policy (rate limit, quota, ACL) ─────────────────
	// Priority is derived from the project (org → project → priority_weight).
	// If no project context, default to standard weight (500).
	effectivePriority := 500 // default: standard business workload
	if claims.ProjectPriorityWeight > 0 {
		effectivePriority = claims.ProjectPriorityWeight
	}
	tp := h.teamPolicy(claims.TeamID)
	decision := h.policy.Evaluate(c.Request.Context(), &policy.InferenceRequest{
		Model:                req.Model,
		EstimatedInputTokens: inputEst,
		OrgID:                claims.OrgID,
		TeamID:               claims.TeamID,
		ProjectID:            claims.ProjectID,
	}, effectivePriority, tp)

	if !decision.Allowed {
		middleware.RecordRejection(claims.TeamID, claims.ProjectID, decision.RejectReason)
		if decision.QueueInstead {
			c.Header("Retry-After", "5")
			abortErr(c, http.StatusTooManyRequests, decision.RejectReason, "Request queued — retry shortly")
			return
		}
		status := http.StatusForbidden
		switch decision.RejectReason {
		case "project_rate_limit_exceeded", "project_token_rate_exceeded",
			"project_daily_budget_exceeded", "project_monthly_budget_exceeded",
			"rate_limit_exceeded", "daily_quota_exceeded",
			"org_monthly_budget_exceeded":
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
		// ── 5a. Try the virtual catalog resolver (Mode-B) ─────────────────
		// Provider catalog models are never cold-started. If the name exists
		// in the exposed catalog we dispatch immediately, bypassing the
		// activator entirely. This is the correct behaviour — virtual models
		// are always "running" because they are backed by a remote API.
		if h.virtualResolver != nil {
			vep, verr := h.virtualResolver.Resolve(c.Request.Context(), req.Model)
			if verr != nil {
				h.log.Warn("virtual resolver error", zap.String("model", req.Model), zap.Error(verr))
			}
			if vep != nil {
				// Synthesize a real Endpoint from the virtual entry and obtain
				// the backend implementation from the factory/registry.
				ep = vep.AsEndpoint()
				backend = h.registry.BackendForType(string(ep.BackendType))
				if backend == nil {
					backend = h.registry.BackendForType(string(runtime.BackendOpenAICompat))
				}
				if backend == nil {
					// BackendForType should never return nil for openai_compat, but
					// be defensive — fall back to 503 rather than panic.
					abortErr(c, http.StatusServiceUnavailable, "no_backend",
						"no backend implementation found for provider type: "+string(ep.BackendType))
					return
				}
				// Build the per-provider HTTP client from the virtual endpoint's
				// transport config (already carries provider proxy, TLS, etc.).
				virtualClient, clientErr := runtime.BuildProviderClient(vep.Transport)
				if clientErr != nil {
					virtualClient = h.httpClient
				}
				// Store client on the endpoint so ClientForEndpoint picks it up.
				// We store it in the Registry's epClients cache keyed by ep.ID.
				h.registry.CacheVirtualClient(ep.ID, virtualClient)
				// Proceed with the virtual endpoint — skip activator entirely.
				goto virtualDispatch
			}
		}

		// Registry miss — decide whether to cold-start or return 404.
		//
		// The decision MUST be based on BackendType metadata from the Model
		// Registry, never on the model name string. A model named "openai/gpt-x"
		// is not necessarily remote, and a model named "production-ai" is not
		// necessarily local. Only the stored BackendType is authoritative.
		//
		// IsRemoteModel queries the registry pool first (fast path) then falls
		// back to a DB lookup (slow path). Remote/provider models have no
		// container to cold-start — they are always reachable via their
		// external API. If the registry has no pool entry and the DB says it
		// is remote (or the model doesn't exist at all as a remote), we return
		// 404 rather than attempting EnsureRunning.
		if h.registry.IsRemoteModel(c.Request.Context(), req.Model) {
			abortErr(c, http.StatusNotFound, "model_not_found",
				fmt.Sprintf("model %q is not available — check that it is enabled in the provider catalog", req.Model))
			return
		}
		if h.activator != nil {
			// ── Cold-start path ────────────────────────────────────────────────
			// handleColdStart either writes 503 (startup in progress) or sets
			// X-Nexus-Warmup-Ms and returns without writing for the rare probe
			// fast-path (model ready but registry stale).
			h.handleColdStart(c, req.Model)
			if c.IsAborted() || c.Writer.Written() {
				// 503 was written — return to caller.
				return
			}
			// Probe fast-path: re-resolve and continue.
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

	// virtualDispatch: jump target when a Mode-B virtual model was resolved.
	// The ep and backend are already set; skip the activator and proceed directly
	// to inflight tracking and dispatch.
virtualDispatch:

	// ── 6. Track inflight ─────────────────────────────────────────────────
	// Project-scoped requests: only increment project inflight counter (Layer-1).
	// Legacy team-only requests: increment team inflight counter.
	// This ensures Team policy never throttles project-scoped requests.
	if claims.ProjectID != "" {
		_ = h.policy.IncrementProjectInflight(c.Request.Context(), claims.ProjectID)
	} else {
		_ = h.policy.IncrementInflight(c.Request.Context(), claims.TeamID)
	}
	middleware.ActiveRequests.WithLabelValues(claims.TeamID, claims.ProjectID, req.Model).Inc()
	atomic.AddInt64(&ep.ActiveConns, 1)
	// Pre-inference activity: resets idle clock at request start.
	if h.activator != nil {
		h.activator.RecordActivity(c.Request.Context(), ep.ID)
	}
	start := time.Now()
	defer func() {
		if claims.ProjectID != "" {
			_ = h.policy.DecrementProjectInflight(context.Background(), claims.ProjectID)
		} else {
			_ = h.policy.DecrementInflight(context.Background(), claims.TeamID)
		}
		middleware.ActiveRequests.WithLabelValues(claims.TeamID, claims.ProjectID, req.Model).Dec()
		atomic.AddInt64(&ep.ActiveConns, -1)
		// Post-inference activity: resets idle clock at request completion so
		// last_used_at reflects when the last response byte was sent, not when
		// the request arrived. Critical for long streaming responses where the
		// start-of-request timestamp would otherwise expire the idle timeout
		// while the client is still receiving tokens.
		if h.activator != nil {
			h.activator.RecordActivity(context.Background(), ep.ID)
		}
	}()

	c.Header("X-Nexus-Request-ID", c.GetString(middleware.RequestIDKey))
	c.Header("X-Nexus-Org-ID", claims.OrgID)
	c.Header("X-Nexus-Model", req.Model)
	c.Header("X-Nexus-Endpoint", ep.ID)

	// Resolve effective URL: UpstreamBaseURL overrides host:port for cloud endpoints.
	endpointURL := ep.URL
	if ep.UpstreamBaseURL != "" {
		endpointURL = ep.UpstreamBaseURL
	}

	// Substitute upstream model name when configured.
	// For virtual (Mode B) catalog models ep.UpstreamModelName is the provider's
	// own model ID (e.g. "openai/gpt-oss-20b"), while req.Model is the NexusLLM
	// virtual name (e.g. "openrouter/openai/gpt-oss-20b"). Without this the
	// provider receives the full virtual name and rejects it as unknown.
	// The same substitution applies for Mode A aliases and legacy external models.
	//
	// FIX C-2: capture the registry name BEFORE mutating req.Model so failover
	// re-resolution uses the correct pool key, not the upstream provider model ID.
	registryModelName := req.Model
	if ep.UpstreamModelName != "" {
		req.Model = ep.UpstreamModelName
	}

	chatReq := runtime.ChatRequest{Req: &req, EndpointURL: endpointURL, UpstreamAPIKey: ep.UpstreamAPIKey, Client: h.registry.ClientForEndpoint(ep)}

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
				claims.TeamID, claims.ProjectID, req.Model, mode).Inc()
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
				// FIX C-2/H-1: re-resolve using registryModelName (the NexusLLM pool
				// key), not req.Model which has already been mutated to the upstream
				// provider model ID. Also rebuild the correct endpointURL for the new
				// endpoint — provider endpoints must use UpstreamBaseURL, not ep.URL.
				ep2, b2, rerr := h.registry.ResolveWithFailover(registryModelName, maxFailoverAttempts)
				if rerr != nil {
					break // no more healthy endpoints
				}
				ep = ep2
				backend = b2
				newURL := ep.URL
				if ep.UpstreamBaseURL != "" {
					newURL = ep.UpstreamBaseURL
				}
				chatReq.EndpointURL = newURL
				chatReq.UpstreamAPIKey = ep.UpstreamAPIKey
				chatReq.Client = h.registry.ClientForEndpoint(ep)
				if ep.UpstreamModelName != "" {
					req.Model = ep.UpstreamModelName
				}
			}
			if !h.streamChat(c, claims, req, chatReq, backend, ep, start) {
				// Connection-level error — mark endpoint down and try again.
				// FIX C-2: use registryModelName for pool lookup, not req.Model
				// which has been mutated to the upstream provider model ID.
				h.registry.UpdateEndpointHealth(c.Request.Context(), ep.ID, registryModelName, runtime.EndpointHealth{
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
				// FIX C-2/H-1: same fix as streaming — use registryModelName and
				// rebuild correct provider URL for the replacement endpoint.
				ep2, b2, rerr := h.registry.ResolveWithFailover(registryModelName, maxFailoverAttempts)
				if rerr != nil {
					abortErr(c, http.StatusServiceUnavailable, "no_healthy_endpoint",
						"all endpoints unreachable after upstream failures")
					return
				}
				ep = ep2
				backend = b2
				newURL := ep.URL
				if ep.UpstreamBaseURL != "" {
					newURL = ep.UpstreamBaseURL
				}
				chatReq.EndpointURL = newURL
				chatReq.UpstreamAPIKey = ep.UpstreamAPIKey
				chatReq.Client = h.registry.ClientForEndpoint(ep)
				if ep.UpstreamModelName != "" {
					req.Model = ep.UpstreamModelName
				}
			}
			// A retry is possible whenever thinking is on and enabled for this
			// model — skipBillingIfEmpty=true so this attempt isn't billed if it
			// turns out to be the discarded one (see syncChat's billing gate).
			mayRetry := thinkingOn && h.thinkingRes != nil
			done, emptyContent := h.syncChat(c, claims, req, chatReq, backend, ep, start, thinkingCaps, mayRetry)
			if done && emptyContent && mayRetry {
				// Thinking consumed all tokens — retry once with thinking disabled.
				h.log.Info("thinking produced empty content — retrying with thinking disabled",
					zap.String("model", req.Model),
				)
				disabledReq := thinking.InjectThinkingControl(req, false, thinkingCaps)
				retryReq := runtime.ChatRequest{Req: &disabledReq, EndpointURL: endpointURL, UpstreamAPIKey: ep.UpstreamAPIKey, Client: h.registry.ClientForEndpoint(ep)}
				c.Header("X-Nexus-Thinking-Retry", "1")
				middleware.ThinkingRequestsTotal.WithLabelValues(
					claims.TeamID, claims.ProjectID, req.Model, "fast_retry").Inc()
				h.syncChat(c, claims, req, retryReq, backend, ep, start, thinkingCaps, false)
				return
			}
			if done {
				return
			}
			// Connection-level failure — mark this endpoint down and try next.
			// FIX C-2: use registryModelName for pool lookup.
			h.registry.UpdateEndpointHealth(c.Request.Context(), ep.ID, registryModelName, runtime.EndpointHealth{
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
	var req models.EmbeddingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abortErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Model == "" {
		abortErr(c, http.StatusBadRequest, "missing_model", "Field 'model' is required")
		return
	}

	// Apply X-Nexus-Provider shorthand before pipeline setup.
	req.Model = resolveModelWithProvider(c, req.Model)

	// Run the full shared pipeline — identical to ChatCompletions.
	res, ok := h.pipelineSetup(c, req.Model, 0)
	if !ok {
		return
	}
	defer h.decrementInflight(context.Background(), res.teamID, res.projectID, res.realModel)
	req.Model = res.realModel

	ep := res.ep
	backend, berr := h.registry.BackendForEndpoint(ep)
	if berr != nil {
		h.decrementInflight(context.Background(), res.teamID, res.projectID, res.realModel)
		abortErr(c, http.StatusInternalServerError, "backend_error", berr.Error())
		return
	}

	atomic.AddInt64(&ep.ActiveConns, 1)
	defer atomic.AddInt64(&ep.ActiveConns, -1)
	if h.activator != nil {
		h.activator.RecordActivity(c.Request.Context(), ep.ID)
		defer h.activator.RecordActivity(context.Background(), ep.ID)
	}

	start := time.Now()
	embURL := ep.URL
	if ep.UpstreamBaseURL != "" {
		embURL = ep.UpstreamBaseURL
	}
	resp, err := backend.Embeddings(c.Request.Context(), runtime.EmbedRequest{Req: &req, EndpointURL: embURL, UpstreamAPIKey: ep.UpstreamAPIKey, Client: h.registry.ClientForEndpoint(ep)})
	if err != nil {
		if runtime.IsProviderBackend(ep.BackendType) {
			middleware.RecordProviderConnectionError(string(ep.BackendType), req.Model, err)
		}
		abortErr(c, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	latencyMs := int(time.Since(start).Milliseconds())
	_ = h.policy.RecordProjectTokenUsage(context.Background(), res.projectID, resp.Usage.TotalTokens, 0)
	_ = h.policy.RecordOrgTokenUsage(context.Background(), res.orgID, resp.Usage.TotalTokens, 0)
	if res.projectID == "" {
		_ = h.policy.RecordTokenUsage(context.Background(), res.teamID, resp.Usage.TotalTokens, 0)
	}
	middleware.RecordTokens(res.teamID, res.projectID, res.realModel, resp.Usage.TotalTokens, 0)
	// FIX C-1: provider metrics for embedding requests.
	if runtime.IsProviderBackend(ep.BackendType) {
		middleware.RecordProviderRequest(string(ep.BackendType), res.realModel, "success", float64(latencyMs)/1000.0, 0)
		middleware.RecordProviderTokens(string(ep.BackendType), res.realModel, resp.Usage.TotalTokens, 0, 0, 0)
	}
	h.usageTracker.Record(context.Background(), usage.Event{
		OrgID: res.orgID, TeamID: res.teamID, ModelName: res.realModel,
		EndpointID: ep.ID, PromptTokens: resp.Usage.TotalTokens,
		LatencyMs: latencyMs, Status: "success",
	})
	// Normalize model field — always echo back the NexusLLM model name the
	// client used, not the upstream provider's internal model ID.
	resp.Model = res.realModel
	c.JSON(http.StatusOK, resp)
}

// Models handles GET /v1/models
//
// Response combines two sources depending on the caller's context:
//
//  1. Public Models (Managed / Hybrid)
//     Every model from claims.Permissions that is currently routable in the
//     registry. This is the unchanged, existing behaviour.
//
//  2. Virtual catalog models (Catalog / Hybrid)
//     When the caller has a project context (ProjectID ≠ ""), models from
//     providers in catalog/hybrid mode that the project has been granted
//     access to via project_provider_access are appended.
//     These are deduplicated — if a virtual name also exists as a Public Model
//     it is not listed twice.
//
// Managed mode providers only ever appear via path 1.
// catalog/hybrid providers appear via path 2 (and path 1 if also registered).
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

	// ── Path 1: Public Models (Managed / Hybrid) ──────────────────────────
	// All callable models in claims.Permissions — local and remote Public
	// Models — filtered to those currently routable in the registry.
	seen := make(map[string]bool)
	var data []models.ModelObject
	allowedModels := claims.Permissions
	if len(allowedModels) == 0 && (claims.OrgID != "" || claims.TeamID != "") && h.db != nil {
		_ = h.db.SelectContext(c.Request.Context(), &allowedModels, `
			SELECT DISTINCT m.name
			FROM models m
			LEFT JOIN team_model_permissions tmp ON tmp.model_id = m.id
			LEFT JOIN teams t ON t.id = tmp.team_id
			WHERE m.enabled = TRUE
			  AND (t.org_id = $1 OR tmp.team_id = $2 OR $1 = '')`,
			claims.OrgID, claims.TeamID)
	}
	for _, modelName := range allowedModels {
		created, ok := modelCreatedAt[modelName]
		if !ok {
			if !registered[modelName] {
				continue
			}
			created = fallbackCreated
		}
		data = append(data, models.ModelObject{
			ID: modelName, Object: "model", Created: created, OwnedBy: "nexusllm",
		})
		seen[modelName] = true
	}

	// ── Path 2: Virtual catalog models (Catalog / Hybrid) ─────────────────
	// Only available to project-scoped callers — virtual models require a
	// project context for rate limiting and provider ACL enforcement.
	if claims.ProjectID != "" && h.virtualResolver != nil {
		entries, err := h.virtualResolver.ListExposedForProjectWithEndpoints(c.Request.Context(), claims.ProjectID)
		if err != nil {
			h.log.Warn("models: virtual resolver list error", zap.Error(err))
		}

		// Bulk-fetch catalog metadata for all virtual models in one query.
		// The result enriches ModelObject with name, description, context_length,
		// pricing, architecture and supported_parameters — the same fields
		// OpenRouter returns on its /api/v1/models endpoint.
		type metaRow struct {
			ProviderID         string   `db:"provider_id"`
			ProviderModelID    string   `db:"provider_model_id"`
			DisplayName        string   `db:"display_name"`
			Description        string   `db:"description"`
			ContextLength      *int     `db:"context_length"`
			MaxOutputTokens    *int     `db:"max_output_tokens"`
			InputCostPer1M     *float64 `db:"input_cost_per_1m"`
			OutputCostPer1M    *float64 `db:"output_cost_per_1m"`
			ProviderInputCost  *float64 `db:"provider_input_cost"`
			ProviderOutputCost *float64 `db:"provider_output_cost"`
			SupportsStreaming  bool     `db:"supports_streaming"`
			SupportsTools      bool     `db:"supports_tools"`
			SupportsVision     bool     `db:"supports_vision"`
			SupportsAudio      bool     `db:"supports_audio"`
			SupportsEmbedding  bool     `db:"supports_embeddings"`
			SupportsReasoning  bool     `db:"supports_reasoning"`
			SupportsJsonMode   bool     `db:"supports_json_mode"`
			ServiceType        string   `db:"service_type"`
			// metadata JSONB stores the raw provider response fields including
			// supported_parameters, canonical_slug, owned_by, and created epoch.
			MetadataRaw string `db:"metadata_raw"`
			// created_at from last_seen_at used as stable created timestamp
			LastSeenAt time.Time `db:"last_seen_at"`
		}
		var metaRows []metaRow
		if h.db != nil && len(entries) > 0 {
			_ = h.db.SelectContext(c.Request.Context(), &metaRows, `
				SELECT provider_id::text, provider_model_id,
				       COALESCE(display_name,'')          AS display_name,
				       COALESCE(description,'')           AS description,
				       context_length,
				       max_output_tokens,
				       input_cost_per_1m,
				       output_cost_per_1m,
				       provider_input_cost,
				       provider_output_cost,
				       supports_streaming, supports_tools,
				       supports_vision, supports_audio,
				       supports_embeddings, supports_reasoning,
				       COALESCE(supports_json_mode, FALSE)  AS supports_json_mode,
				       COALESCE(service_type,'chat')        AS service_type,
				       COALESCE(metadata::text,'{}')        AS metadata_raw,
				       last_seen_at
				FROM provider_remote_models
				WHERE enabled = TRUE`)
		}
		// Index: (providerID, providerModelID) → metaRow.
		type metaKey struct{ p, m string }
		metaByKey := make(map[metaKey]*metaRow, len(metaRows))
		for i := range metaRows {
			metaByKey[metaKey{metaRows[i].ProviderID, metaRows[i].ProviderModelID}] = &metaRows[i]
		}

		for _, e := range entries {
			if seen[e.Name] {
				continue
			}
			obj := models.ModelObject{
				ID:      e.Name,
				Object:  "model",
				Created: fallbackCreated,
				OwnedBy: "nexusllm",
			}
			// Enrich with catalog metadata if available.
			parts := catalog.SplitVirtID(e.VEP.ID)
			if len(parts) == 3 {
				if row := metaByKey[metaKey{parts[1], parts[2]}]; row != nil {
					if row.DisplayName != "" {
						obj.Name = row.DisplayName
					}
					if row.Description != "" {
						obj.Description = row.Description
					}
					obj.ContextLength = row.ContextLength

					// Use actual last_seen_at as the created timestamp so
					// it matches what the provider reported.
					if !row.LastSeenAt.IsZero() {
						obj.Created = row.LastSeenAt.Unix()
					}

					// Architecture block from capability flags.
					obj.Architecture = buildModelArchitecture(row.SupportsVision, row.SupportsAudio, row.ServiceType)

					// Pricing — prefer provider_* cost columns (written by syncer
					// from provider's own pricing data) over legacy per-1M columns.
					obj.Pricing = buildModelPricing(
						row.ProviderInputCost, row.ProviderOutputCost,
						row.InputCostPer1M, row.OutputCostPer1M,
					)

					// top_provider block.
					obj.TopProvider = buildTopProvider(row.ContextLength, row.MaxOutputTokens)

					// supported_parameters — prefer the stored list from the
					// provider's /models response (via metadata JSONB), which
					// contains the full provider-specific parameter list.
					// Fall back to deriving from capability flags.
					if params := extractSupportedParameters(row.MetadataRaw); len(params) > 0 {
						obj.SupportedParameters = params
					} else {
						obj.SupportedParameters = buildSupportedParameters(
							row.SupportsStreaming, row.SupportsTools,
							row.SupportsJsonMode, row.SupportsReasoning,
						)
					}
				}
			}
			data = append(data, obj)
			seen[e.Name] = true
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
		Model:       resolveModelWithProvider(c, req.Model),
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
//
// Resolution order:
//  1. Public Models in registry + claims.Permissions (existing behaviour).
//  2. Virtual catalog models accessible to the caller's project (new).
func (h *Handler) ModelByID(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		abortErr(c, http.StatusUnauthorized, "unauthorized", "Missing authentication")
		return
	}
	modelID := c.Param("model_id")

	// Apply X-Nexus-Provider shorthand so GET /v1/models/gpt-4o with
	// X-Nexus-Provider: openrouter resolves to the virtual name "openrouter/gpt-4o".
	modelID = resolveModelWithProvider(c, modelID)

	// ── Path 1: Public Model ───────────────────────────────────────────────
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
	if allowed && registered[modelID] {
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
		return
	}

	// ── Path 2: Virtual catalog model ─────────────────────────────────────
	// Only available for project-scoped callers.
	if claims.ProjectID != "" && h.virtualResolver != nil {
		vep, verr := h.virtualResolver.Resolve(c.Request.Context(), modelID)
		if verr == nil && vep != nil {
			// Verify the project has provider access before confirming the model.
			store := catalog.NewProjectProviderAccessStore(h.db)
			grants, _ := store.ListForProject(c.Request.Context(), claims.ProjectID)
			for _, g := range grants {
				if g.IsAllowed(modelID) {
					c.JSON(http.StatusOK, models.ModelObject{
						ID:      modelID,
						Object:  "model",
						Created: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Unix(),
						OwnedBy: "nexusllm",
					})
					return
				}
			}
		}
	}

	c.JSON(http.StatusNotFound, models.ErrorResponse{
		Error: models.ErrorDetail{
			Message: "The model '" + modelID + "' does not exist",
			Type:    "invalid_request_error",
			Code:    "model_not_found",
		},
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
	skipBillingIfEmpty bool,
) (ok bool, emptyContent bool) {
	resp, err := backend.Chat(c.Request.Context(), chatReq)
	if err != nil {
		if isConnectError(err) {
			if runtime.IsProviderBackend(ep.BackendType) {
				middleware.RecordProviderConnectionError(string(ep.BackendType), req.Model, err)
			}
			return false, false // caller will mark endpoint down and retry
		}
		abortErr(c, http.StatusBadGateway, "upstream_error", err.Error())
		return true, false
	}

	// Check for non-2xx BEFORE trying to unmarshal as a completion response.
	// A 400 body is a JSON error object, not a ChatCompletionResponse.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		h.log.Warn("upstream returned non-2xx (sync)",
			zap.Int("status", resp.StatusCode),
			zap.String("model", req.Model),
			zap.String("body", string(resp.Body)),
		)
		if len(resp.Body) > 0 {
			c.Data(resp.StatusCode, "application/json", resp.Body)
		} else {
			abortErr(c, resp.StatusCode, "upstream_error",
				fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode))
		}
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
						claims.TeamID, claims.ProjectID, req.Model).Add(float64(thinkTok))
					chatResp.Usage.ThinkingTokens = thinkTok
				}
				contentEmpty = thinking.IsEmptyVisible(s)
			}
		}
	}

	// ── Token usage recording ─────────────────────────────────────────────
	// Skipped entirely when this attempt burned its whole budget on hidden
	// thinking and produced no visible content, AND the caller is about to
	// silently retry with thinking disabled (skipBillingIfEmpty) — otherwise
	// one client-visible response bills two usage events (this attempt's,
	// discarded, plus the retry's), across every quota counter, Prometheus
	// metric, and the usage_events row alike. The retry call always passes
	// skipBillingIfEmpty=false, so whichever attempt's content actually reaches
	// the client is exactly the one that gets billed.
	if !(contentEmpty && skipBillingIfEmpty) {
		// Layer-1: project token counters (RPM/TPM/daily/monthly budget tracking)
		_ = h.policy.RecordProjectTokenUsage(context.Background(), claims.ProjectID,
			chatResp.Usage.PromptTokens, chatResp.Usage.CompletionTokens)
		// Layer-2: org monthly governance counter
		_ = h.policy.RecordOrgTokenUsage(context.Background(), claims.OrgID,
			chatResp.Usage.PromptTokens, chatResp.Usage.CompletionTokens)
		// Legacy: team daily quota counter (only meaningful for team-only keys)
		if claims.ProjectID == "" {
			_ = h.policy.RecordTokenUsage(context.Background(), claims.TeamID,
				chatResp.Usage.PromptTokens, chatResp.Usage.CompletionTokens)
		}
		middleware.RecordTokens(claims.TeamID, claims.ProjectID, req.Model,
			chatResp.Usage.PromptTokens, chatResp.Usage.CompletionTokens)

		visibleTok := chatResp.Usage.CompletionTokens - chatResp.Usage.ThinkingTokens
		if visibleTok < 0 {
			visibleTok = 0
		}
		if thinkingCaps.SupportsThinking {
			middleware.VisibleCompletionTokensTotal.WithLabelValues(
				claims.TeamID, claims.ProjectID, req.Model).Add(float64(visibleTok))
		}

		// FIX C-1: record per-provider Prometheus metrics for cloud provider backends.
		if runtime.IsProviderBackend(ep.BackendType) {
			latencySec := float64(latencyMs) / 1000.0
			middleware.RecordProviderRequest(string(ep.BackendType), req.Model, "success", latencySec, 0)
			middleware.RecordProviderTokens(string(ep.BackendType), req.Model,
				chatResp.Usage.PromptTokens, chatResp.Usage.CompletionTokens, 0, chatResp.Usage.ThinkingTokens)
		}
		// FIX L-1: skip redundant DB lookup when project context already in claims.
		var projID *string
		var projName *string
		var projPriority *string
		var projPriorityWeight *int
		if claims.ProjectID != "" {
			pid := claims.ProjectID
			pname := claims.ProjectName
			ppw := claims.ProjectPriorityWeight
			projID = &pid
			projName = &pname
			projPriorityWeight = &ppw
		} else {
			projID, projName, projPriority, projPriorityWeight = h.lookupProjectContext(context.Background(), req.Model, claims)
		}
		syncCachedTokens := 0
		syncReasoningTokens := 0
		if chatResp.Usage.PromptTokensDetails != nil {
			syncCachedTokens = chatResp.Usage.PromptTokensDetails.CachedTokens
		}
		if chatResp.Usage.CompletionTokensDetails != nil {
			syncReasoningTokens = chatResp.Usage.CompletionTokensDetails.ReasoningTokens
		}
		h.usageTracker.Record(context.Background(), usage.Event{
			OrgID: claims.OrgID, TeamID: claims.TeamID, ModelName: req.Model,
			EndpointID: ep.ID, PromptTokens: chatResp.Usage.PromptTokens,
			CompletionTokens: chatResp.Usage.CompletionTokens,
			TotalTokens:      chatResp.Usage.TotalTokens,
			CachedTokens:     syncCachedTokens,
			ReasoningTokens:  syncReasoningTokens,
			LatencyMs:        latencyMs, Status: "success",
			ProjectID: projID, ProjectName: projName, ProjectPriority: projPriority,
			ProjectPriorityWeight: projPriorityWeight,
		})
	}
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
		if runtime.IsProviderBackend(ep.BackendType) {
			middleware.RecordProviderConnectionError(string(ep.BackendType), req.Model, err)
		}
		abortErr(c, http.StatusBadGateway, "upstream_error", err.Error())
		return true
	}

	// Check for non-2xx from the upstream BEFORE setting SSE headers.
	// If the upstream returns 400/500, the body contains a JSON error,
	// not SSE events. Read and forward it directly so the client gets the
	// actual error message rather than "upstream returned HTTP 400 with no body".
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var bodyBytes []byte
		if resp.Stream != nil {
			// The stream reader wraps the raw response body. Drain it fully.
			var buf strings.Builder
			for {
				line, readErr := resp.Stream.ReadLine()
				if line != "" {
					buf.WriteString(line)
					buf.WriteString("\n")
				}
				if readErr != nil {
					break
				}
			}
			resp.Stream.Close()
			bodyBytes = []byte(strings.TrimSpace(buf.String()))
		} else if len(resp.Body) > 0 {
			// Non-streaming error response — body already buffered.
			bodyBytes = resp.Body
		}
		if len(bodyBytes) == 0 {
			// Log at warn so operators can see what the backend actually sent.
			h.log.Warn("upstream returned non-2xx with empty body",
				zap.Int("status", resp.StatusCode),
				zap.String("model", req.Model),
				zap.String("endpoint", ep.URL),
			)
			abortErr(c, resp.StatusCode, "upstream_error",
				fmt.Sprintf("upstream returned HTTP %d with no body", resp.StatusCode))
		} else {
			h.log.Warn("upstream returned non-2xx",
				zap.Int("status", resp.StatusCode),
				zap.String("model", req.Model),
				zap.String("body", string(bodyBytes)),
			)
			c.Data(resp.StatusCode, "application/json", bodyBytes)
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
		cachedTokens     int
		reasoningTokens  int
		seenUsageChunk   bool   // true if upstream already sent a usage chunk
		streamID         string // captured from first chunk for synthesized usage chunk
		streamModel      string
		streamCreated    int64
		done             bool

		// streamStatus tracks actual completion outcome for accurate usage accounting.
		//   "success"            — upstream sent [DONE] cleanly
		//   "client_disconnected"— context was canceled (client dropped connection)
		//   "stream_error"       — upstream closed the stream before [DONE]
		// Initialized to "stream_error" so any early-exit path that forgets to set
		// it explicitly is still recorded as a non-success rather than silently
		// inflating the success counter.
		streamStatus = "stream_error"
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
			// Distinguish client disconnect (context canceled/deadline exceeded)
			// from a genuine provider-side stream error.
			if c.Request.Context().Err() != nil {
				streamStatus = "client_disconnected"
			} else {
				streamStatus = "stream_error"
			}
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
			streamStatus = "success" // clean provider termination
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
			middleware.ObserveTTFT(claims.TeamID, claims.ProjectID, req.Model, time.Since(start))
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
				promptTokens = chunk.Usage.PromptTokens
				completionTokens = chunk.Usage.CompletionTokens
				if chunk.Usage.PromptTokensDetails != nil {
					cachedTokens = chunk.Usage.PromptTokensDetails.CachedTokens
				}
				if chunk.Usage.CompletionTokensDetails != nil {
					reasoningTokens = chunk.Usage.CompletionTokensDetails.ReasoningTokens
				}
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
		// Layer-1: project token counters
		_ = h.policy.RecordProjectTokenUsage(context.Background(), claims.ProjectID, promptTokens, completionTokens)
		// Layer-2: org monthly governance counter
		_ = h.policy.RecordOrgTokenUsage(context.Background(), claims.OrgID, promptTokens, completionTokens)
		// Legacy: team daily quota (only for team-only keys)
		if claims.ProjectID == "" {
			_ = h.policy.RecordTokenUsage(context.Background(), claims.TeamID, promptTokens, completionTokens)
		}
		middleware.RecordTokens(claims.TeamID, claims.ProjectID, req.Model, promptTokens, completionTokens)
	}
	// FIX C-1: provider metrics for streaming cloud requests.
	if runtime.IsProviderBackend(ep.BackendType) {
		latencySec := float64(latencyMs) / 1000.0
		providerStatus := streamStatus
		if promptTokens+completionTokens == 0 {
			providerStatus = "error"
		}
		middleware.RecordProviderRequest(string(ep.BackendType), req.Model, providerStatus, latencySec, 0)
		if promptTokens+completionTokens > 0 {
			middleware.RecordProviderTokens(string(ep.BackendType), req.Model,
				promptTokens, completionTokens, 0, 0)
		}
	}
	// FIX L-1: skip redundant DB lookup when project context already in claims.
	var projID *string
	var projName *string
	var projPriority *string
	var projPriorityWeight *int
	if claims.ProjectID != "" {
		pid := claims.ProjectID
		pname := claims.ProjectName
		ppw := claims.ProjectPriorityWeight
		projID = &pid
		projName = &pname
		projPriorityWeight = &ppw
	} else {
		projID, projName, projPriority, projPriorityWeight = h.lookupProjectContext(context.Background(), req.Model, claims)
	}
	h.usageTracker.Record(context.Background(), usage.Event{
		OrgID: claims.OrgID, TeamID: claims.TeamID, ModelName: req.Model,
		EndpointID: ep.ID, PromptTokens: promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
		CachedTokens:     cachedTokens,
		ReasoningTokens:  reasoningTokens,
		LatencyMs:        latencyMs, Status: streamStatus,
		ProjectID: projID, ProjectName: projName, ProjectPriority: projPriority,
		ProjectPriorityWeight: projPriorityWeight,
	})
	return true
}

// isConnectError returns true for connection-refused and similar network errors
// that indicate the upstream server is not reachable (as opposed to returning
// an HTTP error code). These errors are safe to retry on a different endpoint.
//
// Explicitly excluded:
//   - "context deadline exceeded" / "context canceled" — these are client-side
//     timeouts, not server failures. The endpoint may be healthy; marking it
//     down would cause spurious failovers.
//   - "EOF" — can indicate a mid-response disconnect, but treating it as a
//     connect failure would mark a healthy endpoint down prematurely.
func isConnectError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no route to host") ||
		strings.Contains(msg, "connect: connection refused") ||
		strings.Contains(msg, "dial tcp") ||
		strings.Contains(msg, "i/o timeout")
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

// handleColdStart is the single cold-start dispatch point for the proxy.
// It is called by ChatCompletions and pipelineSetup when ResolveWithFailover
// finds no healthy endpoint for a local (non-remote) model.
//
// Behaviour:
//  1. If StartTracker is wired: ensure exactly one background EnsureRunning
//     goroutine is running for the model, then return 503 model_starting.
//  2. If StartTracker is nil (fallback / test paths): original 8-second
//     probe behaviour is preserved so existing behaviour is unchanged.
//
// Always writes a 503 response to c and returns. The caller must return
// immediately after this call.
func (h *Handler) handleColdStart(c *gin.Context, modelName string) {
	// ── Manually-deployed models ─────────────────────────────────────────────
	// The operator owns the container of a model registered with
	// deployment_mode='manual' (migration 061). There is nothing for NexusLLM
	// to start: report the endpoint as unhealthy and let the operator bring the
	// container up. Starting one here would either duplicate the operator's
	// container on another port or, when the names collide, replace it.
	if h.activator.IsManuallyDeployed(c.Request.Context(), modelName) {
		h.log.Warn("request for manually-deployed model with no healthy endpoint",
			zap.String("model", modelName),
		)
		abortErr(c, http.StatusServiceUnavailable, "manual_runtime_unhealthy",
			fmt.Sprintf("model %q is deployed manually (deployment_mode=manual) and has no healthy endpoint — "+
				"NexusLLM does not manage its container; start it on the host and it will be routable again "+
				"as soon as the health check passes", modelName))
		return
	}

	if h.startTracker != nil {
		// Goroutine-dedup path: at most one background EnsureRunning per model.
		if !h.startTracker.IsStarting(modelName) {
			if h.startTracker.TryStart(modelName) {
				// This request wins the slot — launch the startup goroutine.
				go func() {
					defer h.startTracker.Done(modelName)
					bgCtx, bgCancel := context.WithTimeout(context.Background(), h.coldStartTimeout())
					defer bgCancel()
					if _, startErr := h.activator.EnsureRunning(bgCtx, modelName); startErr != nil {
						h.log.Warn("background cold-start failed",
							zap.String("model", modelName),
							zap.Error(startErr),
						)
					}
				}()
				h.log.Info("cold-start goroutine launched",
					zap.String("model", modelName),
				)
			}
			// Whether this request launched the goroutine or lost a concurrent
			// TryStart race, a startup goroutine is now running.
		}
		// In all cases: tell the client to retry.
		c.Header("Retry-After", "10")
		abortErr(c, http.StatusServiceUnavailable, "model_starting",
			fmt.Sprintf("model %q is starting up, please retry in ~10 seconds", modelName))
		return
	}

	// Fallback: original probe behaviour when StartTracker is not wired.
	// Preserved for backward compatibility with tests and deployments that
	// do not call WithStartTracker.
	probeCtx, probeCancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	_, probeErr := h.activator.EnsureRunning(probeCtx, modelName)
	probeCancel()

	if probeErr != nil {
		h.log.Info("cold-start triggered (no tracker) — returning 503 Retry-After",
			zap.String("model", modelName),
		)
		go func() {
			bgCtx, bgCancel := context.WithTimeout(context.Background(), h.coldStartTimeout())
			defer bgCancel()
			if _, startErr := h.activator.EnsureRunning(bgCtx, modelName); startErr != nil {
				h.log.Warn("background cold-start failed",
					zap.String("model", modelName),
					zap.Error(startErr),
				)
			}
		}()
		c.Header("Retry-After", "10")
		abortErr(c, http.StatusServiceUnavailable, "model_starting",
			fmt.Sprintf("model %q is starting up, please retry in ~10 seconds", modelName))
		return
	}
	// Probe succeeded immediately — re-resolve.
	// The caller must handle err after this returns, but since we return after
	// abortErr above or fall through here, we set a header for observability.
	c.Header("X-Nexus-Warmup-Ms", "0")
}

func (h *Handler) coldStartTimeout() time.Duration {
	if h.coldStartDur > 0 {
		return h.coldStartDur
	}
	// Default to 20 minutes — large models (235B) take 10-15 min to load.
	return 20 * time.Minute
}

// sanitizeForBackend removes fields that are valid in the OpenAI API spec but
// cause local backends (llama.cpp, TGI, vLLM) to return 400 errors
// because they don't recognise them. Called on a copy of the request so the
// original is untouched for logging and retry purposes.
//
// Also performs field translation:
//   - max_completion_tokens (new OpenAI SDK name) → max_tokens (understood by all backends)
//
// Fields stripped for local backends (confirmed to cause 400):
//
//	stream_options, parallel_tool_calls, service_tier, store,
//	max_completion_tokens (translated then cleared), logprobs, top_logprobs,
//	metadata, modalities, prediction, audio, web_search_options, reasoning_effort
func sanitizeForBackend(req models.InferenceRequest, backendType runtime.BackendType) models.InferenceRequest {
	switch backendType {
	case runtime.BackendOpenAICompat:
		// True OpenAI-compatible remote provider — forward everything as-is.
		return req
	default:
		// Local backends: llama.cpp, vllm, tgi, cpu_native.

		// Translate max_completion_tokens → max_tokens.
		// The newer OpenAI SDK (>=1.26) sends max_completion_tokens; local backends
		// only understand max_tokens. Prefer max_completion_tokens when both are set.
		if req.MaxCompletionTokens != nil {
			if req.MaxTokens == nil || *req.MaxCompletionTokens > *req.MaxTokens {
				req.MaxTokens = req.MaxCompletionTokens
			}
			req.MaxCompletionTokens = nil
		}

		// Strip all OpenAI-only fields that local backends reject.
		req.StreamOptions = nil
		req.ParallelToolCalls = nil
		req.ServiceTier = nil
		req.Store = nil
		req.Logprobs = nil
		req.TopLogprobs = nil
		req.Metadata = nil
		req.Modalities = nil
		req.Prediction = nil
		req.Audio = nil
		req.WebSearchOptions = nil
		req.ReasoningEffort = nil
		req.Effort = nil
		return req
	}
}

// teamProjectOverride is the result of resolveTeamProjectOverride.
// applyProjectHeaderOverride implements the X-Nexus-Project header override:
// it allows a TEAM-ONLY key (no project_id of its own) to attribute a
// request to one of its OWN team's projects for priority/quota purposes.
//
// SECURITY (forensic audit, project-authorization round): this used to look
// up the target project org-wide, with no check that the caller's key had
// any relationship to it at all. Once project scope started gating model
// access too (internal/policy Option A), that made this header a direct
// authorization bypass — a key restricted via its own project's model grants
// could simply name a different, unconfigured (passthrough) project in the
// same org and inherit full team access.
//
// Fixed scoping, in order of precedence:
//   - A key that is ALREADY project-scoped (api_keys.project_id set) is
//     explicitly associated with exactly that one project and no other — the
//     header is ignored entirely for such a key. A project-scoped key's
//     restriction must never be escapable by a request header.
//   - A team-only key (no project_id) may select among projects EXPLICITLY
//     belonging to its own team (projects.team_id = claims.TeamID) — never
//     org-wide, never a different team's project.
//
// Extracted into its own method (production security re-audit) so this guard
// itself — not just resolveTeamProjectOverride's DB lookup — has direct,
// end-to-end test coverage independent of the full ChatCompletions pipeline;
// see internal/proxy/project_override_test.go.
func (h *Handler) applyProjectHeaderOverride(c *gin.Context, claims *auth.TeamClaims) *auth.TeamClaims {
	if claims.ProjectID != "" {
		return claims
	}
	projectHdr := c.GetHeader("X-Nexus-Project")
	if projectHdr == "" || h.db == nil {
		return claims
	}
	proj, ok := resolveTeamProjectOverride(c.Request.Context(), h.db, claims.TeamID, claims.OrgID, projectHdr)
	if !ok {
		return claims
	}
	overriddenClaims := *claims
	overriddenClaims.ProjectID = proj.ID
	overriddenClaims.ProjectName = proj.Name
	overriddenClaims.ProjectPriorityWeight = proj.PriorityWeight
	return &overriddenClaims
}

type teamProjectOverride struct {
	ID             string `db:"id"`
	Name           string `db:"name"`
	PriorityWeight int    `db:"priority_weight"`
}

// resolveTeamProjectOverride looks up a project a TEAM-ONLY key (no
// project_id of its own) may attribute a request to via the X-Nexus-Project
// header, for priority/quota purposes only.
//
// SECURITY: the project MUST belong to the caller's own team (teamID) — not
// merely the same organization. A caller must never be able to name a
// project it has no real relationship to; a project scoped org-wide (the
// original behavior) let any key in an org pick any other team's project,
// which — once project scope started gating model access too (Option A in
// internal/policy) — became a direct authorization bypass: a key restricted
// via its own project's grants could simply name an unconfigured (legacy
// passthrough) project belonging to a different team and inherit that
// team's full model access. Callers with claims.ProjectID already set must
// never reach this function at all (see handler.go call site) — such a key
// is explicitly associated with exactly one project and no other.
func resolveTeamProjectOverride(ctx context.Context, db *sqlx.DB, teamID, orgID, projectHdr string) (teamProjectOverride, bool) {
	var proj teamProjectOverride
	err := db.GetContext(ctx, &proj, `
		SELECT id::text AS id, name, priority_weight
		FROM projects
		WHERE (name = $1 OR id::text = $1)
		  AND team_id = $2
		  AND organization_id = $3
		  AND status = 'active'
		LIMIT 1`, projectHdr, teamID, orgID)
	if err != nil {
		return teamProjectOverride{}, false
	}
	return proj, true
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

// ── Catalog model response helpers ───────────────────────────────────────────
// These convert provider_remote_models capability flags into the rich
// ModelObject fields that match OpenRouter's /v1/models response shape.

func buildModelArchitecture(supportsVision, supportsAudio bool, serviceType string) *models.ModelArchitecture {
	arch := &models.ModelArchitecture{
		Tokenizer: "Router",
	}
	inputs := []string{"text"}
	if supportsVision {
		inputs = append(inputs, "image")
	}
	if supportsAudio {
		inputs = append(inputs, "audio")
	}
	outputs := []string{"text"}
	switch serviceType {
	case "image", "image_generation":
		outputs = []string{"image"}
		arch.Modality = "text->image"
	case "speech", "tts":
		outputs = []string{"audio"}
		arch.Modality = "text->audio"
	case "embedding":
		outputs = []string{"embedding"}
		arch.Modality = "text->embedding"
	default:
		if supportsVision {
			arch.Modality = "text+image->text"
		} else {
			arch.Modality = "text->text"
		}
	}
	arch.InputModalities = inputs
	arch.OutputModalities = outputs
	return arch
}

func buildModelPricing(provIn, provOut, legacyIn, legacyOut *float64) *models.ModelPricing {
	p := &models.ModelPricing{}
	set := false
	// Prefer provider_input_cost / provider_output_cost (migration 050 columns);
	// fall back to input_cost_per_1m / output_cost_per_1m (pre-050).
	in := provIn
	if in == nil {
		in = legacyIn
	}
	out := provOut
	if out == nil {
		out = legacyOut
	}
	if in != nil {
		// Convert per-1M tokens → per-token (OpenRouter format).
		p.Prompt = formatCostPerToken(*in)
		set = true
	}
	if out != nil {
		p.Completion = formatCostPerToken(*out)
		set = true
	}
	if !set {
		return nil
	}
	return p
}

// formatCostPerToken converts a per-1M-token price to a decimal string
// matching OpenRouter's format (e.g. 0.000001 per token from $1/1M).
func formatCostPerToken(perMillion float64) string {
	perToken := perMillion / 1_000_000
	return fmt.Sprintf("%g", perToken)
}

func buildTopProvider(contextLen, maxOutput *int) *models.ModelTopProvider {
	if contextLen == nil && maxOutput == nil {
		return nil
	}
	return &models.ModelTopProvider{
		ContextLength:       contextLen,
		MaxCompletionTokens: maxOutput,
		IsModerated:         false,
	}
}

func buildSupportedParameters(streaming, tools, jsonMode, reasoning bool) []string {
	params := []string{"temperature", "top_p", "max_tokens"}
	if streaming {
		params = append(params, "stream")
	}
	if tools {
		params = append(params, "tools", "tool_choice")
	}
	if jsonMode {
		params = append(params, "response_format")
	}
	if reasoning {
		params = append(params, "reasoning")
	}
	return params
}

// extractSupportedParameters reads the supported_parameters list from the
// JSONB metadata column written by the catalog syncer.
// OpenRouter stores this as metadata["supported_parameters"] = ["temperature", "tools", ...].
// Returns nil when the metadata is absent or doesn't contain the field.
func extractSupportedParameters(metadataRaw string) []string {
	if metadataRaw == "" || metadataRaw == "{}" || metadataRaw == "null" {
		return nil
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal([]byte(metadataRaw), &meta); err != nil {
		return nil
	}
	raw, ok := meta["supported_parameters"]
	if !ok {
		return nil
	}
	var params []string
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil
	}
	return params
}

// ProviderModels handles GET /v1/providers/:provider_name/models
//
// Proxies the raw /models response directly from the named cloud provider
// back to the caller without any transformation. This lets clients get the
// exact JSON the provider returns — including every provider-specific field
// (canonical_slug, alias_target, supported_parameters, pricing, reasoning,
// knowledge_cutoff, per_request_limits, etc.) — for any configured provider.
//
// Auth still applies. The caller must have a valid NexusLLM API key.
//
// The HTTP client is built from the provider's full transport config
// (proxy, TLS, timeouts, connection pool) — the same client used for
// chat completions and catalog sync. This ensures outbound proxy settings
// are honoured identically across all provider traffic.
//
// Examples:
//
//	GET /v1/providers/openrouter/models   → proxied from https://openrouter.ai/api/v1/models
//	GET /v1/providers/openai/models       → proxied from https://api.openai.com/v1/models
//	GET /v1/providers/anthropic/models    → proxied from https://api.anthropic.com/v1/models
//
// The `:provider_name` matches the `name` column of the providers table.
func (h *Handler) ProviderModels(c *gin.Context) {
	if middleware.GetClaims(c) == nil {
		abortErr(c, http.StatusUnauthorized, "unauthorized", "Missing authentication")
		return
	}
	if h.db == nil {
		abortErr(c, http.StatusServiceUnavailable, "no_db", "Database not available")
		return
	}

	providerName := c.Param("provider_name")

	// Load the full provider row — credentials AND complete transport config.
	// Using the same column list as catalog.ProviderStore.Get() so transport
	// settings (proxy, TLS, timeouts, pool) are applied identically to how
	// chat completions and catalog sync reach the provider.
	var prov struct {
		ID           string `db:"id"`
		Name         string `db:"name"`
		BackendType  string `db:"backend_type"`
		BaseURL      string `db:"base_url"`
		APIKey       string `db:"api_key"`
		APIKeyHeader string `db:"api_key_header"`

		// Full transport config — mirrors catalog.Provider transport fields.
		ProxyURL                     string `db:"proxy_url"`
		TLSInsecureSkipVerify        bool   `db:"tls_insecure_skip_verify"`
		TLSRootCAPEM                 string `db:"tls_root_ca_pem"`
		ConnectTimeoutSeconds        int    `db:"connect_timeout_seconds"`
		ReadTimeoutSeconds           int    `db:"read_timeout_seconds"`
		IdleConnTimeoutSeconds       int    `db:"idle_conn_timeout_seconds"`
		ResponseHeaderTimeoutSeconds int    `db:"response_header_timeout_seconds"`
		MaxIdleConnsPerHost          int    `db:"max_idle_conns_per_host"`
		MaxConnsPerHost              int    `db:"max_conns_per_host"`
		DisableHTTP2                 bool   `db:"disable_http2"`
	}
	err := h.db.GetContext(c.Request.Context(), &prov, `
		SELECT id::text, name, backend_type, base_url, api_key,
		       COALESCE(api_key_header,'Authorization')  AS api_key_header,
		       COALESCE(proxy_url,'')                    AS proxy_url,
		       tls_insecure_skip_verify,
		       COALESCE(tls_root_ca_pem,'')              AS tls_root_ca_pem,
		       connect_timeout_seconds,
		       read_timeout_seconds,
		       idle_conn_timeout_seconds,
		       response_header_timeout_seconds,
		       max_idle_conns_per_host,
		       max_conns_per_host,
		       disable_http2
		FROM providers
		WHERE name = $1 AND enabled = TRUE
		LIMIT 1`, providerName)
	if err != nil {
		abortErr(c, http.StatusNotFound, "provider_not_found",
			fmt.Sprintf("provider %q not found or not enabled", providerName))
		return
	}

	// FIX C-3: enforce provider access authorization.
	// A caller may only list a provider's models when their org has at least
	// one active project_provider_access grant for this provider. This prevents
	// authenticated users from enumerating providers that belong to other orgs.
	// Project-scoped callers must have an explicit grant for their own project.
	// Org-level callers (no ProjectID) are allowed if any project in their org
	// has been granted access — this covers admin API keys.
	claims := middleware.GetClaims(c)
	if claims != nil {
		var accessCount int
		if claims.ProjectID != "" {
			// Project-scoped: require a direct grant for this project+provider.
			_ = h.db.QueryRowContext(c.Request.Context(), `
				SELECT COUNT(*) FROM project_provider_access
				WHERE project_id::text = $1
				  AND provider_id::text = $2
				  AND enabled = TRUE`, claims.ProjectID, prov.ID,
			).Scan(&accessCount)
		} else {
			// Org-level key: allow if any active project in this org has access.
			_ = h.db.QueryRowContext(c.Request.Context(), `
				SELECT COUNT(*) FROM project_provider_access ppa
				JOIN projects p ON p.id = ppa.project_id
				WHERE p.organization_id::text = $1
				  AND ppa.provider_id::text = $2
				  AND ppa.enabled = TRUE`, claims.OrgID, prov.ID,
			).Scan(&accessCount)
		}
		if accessCount == 0 {
			abortErr(c, http.StatusForbidden, "provider_access_denied",
				fmt.Sprintf("provider %q is not accessible to your organization", providerName))
			return
		}
	}

	// Build the models URL for this provider's backend type.
	// Mirrors the path logic in CatalogSyncer.fetchModels().
	modelsURL := strings.TrimSuffix(prov.BaseURL, "/")
	switch prov.BackendType {
	case "openrouter_provider":
		modelsURL += "/api/v1/models"
	case "groq_provider":
		modelsURL += "/openai/v1/models"
	case "google_provider":
		modelsURL += "/v1beta/openai/models"
	default:
		modelsURL += "/v1/models"
	}
	// Forward any query string the caller passed (e.g. ?supported_parameters=tools).
	if rawQuery := c.Request.URL.RawQuery; rawQuery != "" {
		modelsURL += "?" + rawQuery
	}

	// Build an isolated HTTP client from the provider's full transport config.
	// This is identical to how virtual endpoint clients are built for chat
	// completions — ensures the outbound proxy and TLS settings are applied.
	transport := runtime.ProviderTransportConfig{
		ProxyURL:                     prov.ProxyURL,
		TLSInsecureSkipVerify:        prov.TLSInsecureSkipVerify,
		TLSRootCAPEM:                 prov.TLSRootCAPEM,
		ConnectTimeoutSeconds:        prov.ConnectTimeoutSeconds,
		ReadTimeoutSeconds:           prov.ReadTimeoutSeconds,
		IdleConnTimeoutSeconds:       prov.IdleConnTimeoutSeconds,
		ResponseHeaderTimeoutSeconds: prov.ResponseHeaderTimeoutSeconds,
		MaxIdleConnsPerHost:          prov.MaxIdleConnsPerHost,
		MaxConnsPerHost:              prov.MaxConnsPerHost,
		DisableHTTP2:                 prov.DisableHTTP2,
	}
	provClient, clientErr := runtime.BuildProviderClient(transport)
	if clientErr != nil {
		// Non-fatal: fall back to the shared direct client so the request
		// still works, but log so operators can fix the transport config.
		h.log.Warn("provider models: failed to build provider client, using fallback",
			zap.String("provider", providerName),
			zap.Error(clientErr),
		)
		provClient = h.httpClient
	}

	// Build and fire the upstream request.
	upstreamReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, modelsURL, nil)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, "request_error", err.Error())
		return
	}

	// Inject API key using the provider's configured header convention.
	if prov.APIKey != "" {
		if prov.APIKeyHeader == "Authorization" || prov.APIKeyHeader == "" {
			upstreamReq.Header.Set("Authorization", "Bearer "+prov.APIKey)
		} else {
			upstreamReq.Header.Set(prov.APIKeyHeader, prov.APIKey)
		}
	}

	// Forward benign client headers.
	for _, hdr := range []string{"Accept", "Accept-Encoding", "User-Agent"} {
		if v := c.Request.Header.Get(hdr); v != "" {
			upstreamReq.Header.Set(hdr, v)
		}
	}

	resp, err := provClient.Do(upstreamReq)
	if err != nil {
		abortErr(c, http.StatusBadGateway, "upstream_error",
			fmt.Sprintf("failed to reach provider %q: %s", providerName, err.Error()))
		return
	}
	defer resp.Body.Close()

	// Pass the provider's status code, content-type and body straight through.
	// No parsing, no transformation — the client gets exactly what the provider sent.
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		abortErr(c, http.StatusBadGateway, "read_error", readErr.Error())
		return
	}
	c.Header("X-Nexus-Provider", prov.Name)
	c.Header("X-Nexus-Provider-URL", modelsURL)
	c.Data(resp.StatusCode, ct, body)
}
