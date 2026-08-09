package proxy

// endpoints.go — Additional inference endpoint handlers (non-chat).
//
// Every handler executes the IDENTICAL pipeline as ChatCompletions:
//
//	Auth → Alias → Project Context → Gateway Policy → Project Policy →
//	Prompt Policy → EnsureRunning → Inflight Tracking →
//	Endpoint Resolution → Backend Dispatch → Usage Recording → Inflight Decrement
//
// No endpoint is permitted to bypass any stage. Adding a new inference endpoint
// (Rerank, STT, TTS, OCR, Vision, ImageGen, ...) requires only:
//   1. A handler function that calls pipelineSetup then dispatches the request.
//   2. A route registration in cmd/gateway/main.go.
// No changes to RuntimeManager, Scheduler, or NodeAgent.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nexusllm/nexusllm/internal/middleware"
	"github.com/nexusllm/nexusllm/internal/models"
	"github.com/nexusllm/nexusllm/internal/policy"
	"github.com/nexusllm/nexusllm/internal/runtime"
	"github.com/nexusllm/nexusllm/internal/usage"
	"go.uber.org/zap"
)

// ─────────────────────────────────────────────────────────────────────────────
// pipelineResult holds everything resolved by the pre-dispatch pipeline.
// ─────────────────────────────────────────────────────────────────────────────

type pipelineResult struct {
	realModel string
	ep        *runtime.Endpoint
	teamID    string
	projectID string
	orgID     string
}

// pipelineSetup runs the full pre-dispatch pipeline for every inference endpoint.
//
// Stages executed (identical order to ChatCompletions):
//  1. Authentication (claims check)
//  2. Alias resolution
//  3. Gateway policy (temperature cap, stream/tool restrictions, model ACL)
//  4. Project/team policy (RPM, TPM, daily budget, concurrency, context length)
//  5. Prompt policy (system prompt injection, content filter)
//  6. EnsureRunning (lazy-load cold start)
//  7. Project inflight increment
//  8. Endpoint resolution (registry + failover)
//
// On success it returns a populated pipelineResult and true.
// On failure it writes the error response and returns false — the caller must
// return immediately without touching the response writer again.
//
// estimatedTokens should reflect the approximate input token count.
// Use 0 for workloads with no token-based billing (STT, TTS, OCR, Rerank).
func (h *Handler) pipelineSetup(c *gin.Context, rawModel string, estimatedTokens int) (pipelineResult, bool) {
	// Apply X-Nexus-Provider shorthand before anything else so every stage
	// (alias resolution, policy ACL, capability check, registry lookup) sees
	// the fully-qualified virtual model name.
	rawModel = resolveModelWithProvider(c, rawModel)

	// ── 1. Authentication ─────────────────────────────────────────────────────
	claims := middleware.GetClaims(c)
	if claims == nil {
		abortErr(c, http.StatusUnauthorized, "unauthorized", "Missing authentication")
		return pipelineResult{}, false
	}

	// ── 2. Alias resolution ───────────────────────────────────────────────────
	realModel, _ := h.aliasResolver.Resolve(c.Request.Context(), rawModel, claims.TeamID, claims.OrgID)
	c.Set("model", realModel)

	// ── 2b. Capability validation ─────────────────────────────────────────────
	// Check that the model supports this endpoint before policy evaluation,
	// quota checks, or any backend interaction. Engine-independent — uses the
	// registry DB metadata as single source of truth.
	if h.capValidator != nil {
		if !h.capValidator.CheckAndAbort(c, realModel, c.FullPath()) {
			return pipelineResult{}, false
		}
	}

	// ── 3. Gateway policy ─────────────────────────────────────────────────────
	// Build a minimal InferenceRequest for gateway policy; non-chat endpoints
	// don't have full message arrays so we pass nil — gateway policy will only
	// check model ACL, stream permissions, and token caps that apply.
	if h.gwPolicy != nil {
		if v := h.gwPolicy.Enforce(
			c.Request.Context(),
			claims.OrgID, claims.ProjectID, claims.TeamID, claims.APIKeyID,
			&models.InferenceRequest{Model: realModel},
			estimatedTokens,
		); v != nil {
			abortErr(c, http.StatusForbidden, v.Code, v.Message)
			return pipelineResult{}, false
		}
	}

	// ── 4. Project / team policy (RPM, TPM, budgets, concurrency) ────────────
	effectivePriority := 500
	if claims.ProjectPriorityWeight > 0 {
		effectivePriority = claims.ProjectPriorityWeight
	}
	tp := h.teamPolicy(claims.TeamID)
	decision := h.policy.Evaluate(c.Request.Context(), &policy.InferenceRequest{
		Model:                realModel,
		EstimatedInputTokens: estimatedTokens,
		OrgID:                claims.OrgID,
		TeamID:               claims.TeamID,
		ProjectID:            claims.ProjectID,
	}, effectivePriority, tp)
	if !decision.Allowed {
		middleware.RecordRejection(claims.TeamID, claims.ProjectID, decision.RejectReason)
		if decision.QueueInstead {
			c.Header("Retry-After", "5")
			abortErr(c, http.StatusTooManyRequests, decision.RejectReason, "Request queued — retry shortly")
			return pipelineResult{}, false
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
		return pipelineResult{}, false
	}

	// ── 5. Prompt policy ──────────────────────────────────────────────────────
	// Non-chat endpoints don't have a message array to inject into, so we pass
	// an empty InferenceRequest. The prompt policy engine checks org/team rules
	// that may block the request entirely (e.g. content moderation flags).
	if h.promptPolicy != nil {
		ppDecision := h.promptPolicy.Evaluate(
			c.Request.Context(), claims.OrgID, claims.TeamID, realModel,
			&models.InferenceRequest{Model: realModel},
		)
		if !ppDecision.Allowed {
			abortErr(c, http.StatusForbidden, "prompt_policy_blocked", ppDecision.BlockReason)
			return pipelineResult{}, false
		}
	}

	// ── 6. EnsureRunning (lazy-load cold start — local models only) ──────────
	// Authorization is complete at this point. Backend selection begins here.
	//
	// Remote/virtual models (BackendType is a provider backend, or the model
	// resolves via the virtual catalog) MUST NOT trigger EnsureRunning. They
	// have no container lifecycle — calling EnsureRunning on them would
	// attempt to schedule a container that does not exist.
	//
	// Decision is based on BackendType metadata (IsRemoteModel), never the
	// model name string. This mirrors the same check in ChatCompletions.
	if h.activator != nil {
		if _, _, err := h.registry.ResolveWithFailover(realModel, 1); err != nil {
			// Registry miss. Check whether this is a remote model before
			// attempting a cold start. Remote models skip EnsureRunning entirely
			// and are handled in stage 8 via the virtual resolver.
			if !h.registry.IsRemoteModel(c.Request.Context(), realModel) {
				// Local model not yet running — delegate to the shared cold-start handler.
				// handleColdStart writes 503 and returns for the starting case, or sets
				// X-Nexus-Warmup-Ms and returns (probe fast-path) when the model is
				// already ready but the registry was stale.
				h.handleColdStart(c, realModel)
				if c.IsAborted() || c.Writer.Written() {
					return pipelineResult{}, false
				}
				// Probe fast-path: fall through to endpoint re-resolution below.
			}
			// Remote model — fall through to stage 8 where the virtual
			// resolver will handle it. No cold-start attempted.
		}
	}

	// ── 7. Project inflight increment ─────────────────────────────────────────
	if claims.ProjectID != "" {
		_ = h.policy.IncrementProjectInflight(c.Request.Context(), claims.ProjectID)
	} else {
		_ = h.policy.IncrementInflight(c.Request.Context(), claims.TeamID)
	}
	middleware.ActiveRequests.WithLabelValues(claims.TeamID, claims.ProjectID, realModel).Inc()

	// ── 8. Endpoint resolution ────────────────────────────────────────────────
	// Try the registry first (covers local runtimes and Mode-A catalog aliases).
	// On miss, fall through to the virtual catalog resolver (Mode-B), identical
	// to the path in ChatCompletions. This ensures all endpoint types — local,
	// remote registered, and virtual catalog — are routed uniformly after the
	// full auth pipeline has already completed.
	ep, _, err := h.registry.ResolveWithFailover(realModel, maxFailoverAttempts)
	if err != nil {
		// Registry miss — try Mode-B virtual catalog resolver.
		if h.virtualResolver != nil {
			vep, verr := h.virtualResolver.Resolve(c.Request.Context(), realModel)
			if verr != nil {
				h.log.Warn("virtual resolver error in pipelineSetup",
					zap.String("model", realModel), zap.Error(verr))
			}
			if vep != nil {
				ep = vep.AsEndpoint()
				// Build a dedicated HTTP client for the virtual endpoint and
				// register it in the registry client cache so ClientForEndpoint
				// returns it correctly when the handler dispatches the request.
				virtualClient, clientErr := runtime.BuildProviderClient(vep.Transport)
				if clientErr != nil {
					virtualClient = h.httpClient
				}
				h.registry.CacheVirtualClient(ep.ID, virtualClient)
				// ep is set — fall through to return pipelineResult below.
			} else {
				// Not in virtual catalog either — model truly not found.
				h.decrementInflight(c.Request.Context(), claims.TeamID, claims.ProjectID, realModel)
				abortErr(c, http.StatusNotFound, "model_not_found",
					fmt.Sprintf("model %q not found", realModel))
				return pipelineResult{}, false
			}
		} else {
			// No virtual resolver configured — cannot route.
			h.decrementInflight(c.Request.Context(), claims.TeamID, claims.ProjectID, realModel)
			abortErr(c, http.StatusServiceUnavailable, "no_healthy_endpoint", err.Error())
			return pipelineResult{}, false
		}
	}

	return pipelineResult{
		realModel: realModel,
		ep:        ep,
		teamID:    claims.TeamID,
		projectID: claims.ProjectID,
		orgID:     claims.OrgID,
	}, true
}

// decrementInflight is a convenience wrapper that mirrors the defer in ChatCompletions.
func (h *Handler) decrementInflight(ctx context.Context, teamID, projectID, model string) {
	if projectID != "" {
		_ = h.policy.DecrementProjectInflight(ctx, projectID)
	} else {
		_ = h.policy.DecrementInflight(ctx, teamID)
	}
	middleware.ActiveRequests.WithLabelValues(teamID, projectID, model).Dec()
}

// ─────────────────────────────────────────────────────────────────────────────
// Rerank — POST /v1/rerank
// ─────────────────────────────────────────────────────────────────────────────

func (h *Handler) Rerank(c *gin.Context) {
	var req models.RerankRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abortErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Model == "" {
		abortErr(c, http.StatusBadRequest, "missing_model", "Field 'model' is required")
		return
	}

	res, ok := h.pipelineSetup(c, req.Model, len(req.Documents)*50)
	if !ok {
		return
	}
	defer h.decrementInflight(context.Background(), res.teamID, res.projectID, res.realModel)
	req.Model = res.realModel

	ep := res.ep
	atomic.AddInt64(&ep.ActiveConns, 1)
	defer atomic.AddInt64(&ep.ActiveConns, -1)
	if h.activator != nil {
		h.activator.RecordActivity(c.Request.Context(), ep.ID)
	}
	start := time.Now()

	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost,
		epEffectiveURL(ep)+"/v1/rerank", bytes.NewReader(body))
	if err != nil {
		abortErr(c, http.StatusInternalServerError, "request_build_error", err.Error())
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if ep.UpstreamAPIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+ep.UpstreamAPIKey)
	}

	resp, err := h.registry.ClientForEndpoint(ep).Do(httpReq)
	if err != nil {
		abortErr(c, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	h.usageTracker.Record(context.Background(), usage.Event{
		OrgID: res.orgID, TeamID: res.teamID,
		ModelName: res.realModel, EndpointID: ep.ID,
		LatencyMs: int(time.Since(start).Milliseconds()),
		Status:    statusFromHTTP(resp.StatusCode),
	})
	c.Data(resp.StatusCode, "application/json", respBody)
}

// ─────────────────────────────────────────────────────────────────────────────
// STT — POST /v1/audio/transcriptions
// ─────────────────────────────────────────────────────────────────────────────

func (h *Handler) Transcriptions(c *gin.Context) {
	// Read and buffer the entire request body so we can:
	//   1. Extract the "model" field from the multipart form
	//   2. Extract the optional "upstream_model" field (user-specified model size)
	//   3. Forward the complete original body (including the audio file) upstream
	//
	// We MUST NOT use c.PostForm() directly because it parses + consumes the
	// multipart body, leaving c.Request.Body empty for the upstream forward.
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		abortErr(c, http.StatusBadRequest, "read_error", "failed to read request body")
		return
	}
	// Restore the body so forwardRaw can read it.
	c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	// Parse multipart from the buffered copy to extract the model and upstream_model fields.
	rawModel := ""
	upstreamModel := ""
	mr := multipart.NewReader(
		bytes.NewReader(bodyBytes),
		extractBoundary(c.Request.Header.Get("Content-Type")),
	)
	for {
		part, partErr := mr.NextPart()
		if partErr != nil {
			break
		}
		fieldName := part.FormName()
		if fieldName == "model" {
			val, _ := io.ReadAll(part)
			rawModel = strings.TrimSpace(string(val))
		} else if fieldName == "upstream_model" {
			val, _ := io.ReadAll(part)
			upstreamModel = strings.TrimSpace(string(val))
		}
		part.Close()
		// Stop early if we have both fields
		if rawModel != "" && upstreamModel != "" {
			break
		}
	}

	if rawModel == "" {
		abortErr(c, http.StatusBadRequest, "missing_model", "Field 'model' is required")
		return
	}

	res, ok := h.pipelineSetup(c, rawModel, 0)
	if !ok {
		return
	}
	defer h.decrementInflight(context.Background(), res.teamID, res.projectID, res.realModel)

	ep := res.ep
	atomic.AddInt64(&ep.ActiveConns, 1)
	defer atomic.AddInt64(&ep.ActiveConns, -1)
	if h.activator != nil {
		h.activator.RecordActivity(c.Request.Context(), ep.ID)
	}
	start := time.Now()

	// Determine which upstream model name to use:
	// 1. User-provided upstream_model in request (highest priority)
	// 2. Endpoint's configured UpstreamModelName (DB config)
	// 3. No substitution (forward as-is)
	effectiveUpstreamModel := upstreamModel
	if effectiveUpstreamModel == "" {
		effectiveUpstreamModel = ep.UpstreamModelName
	}

	// If we have an upstream model name (from request or config), rewrite the form.
	if effectiveUpstreamModel != "" {
		if err := h.forwardMultipartWithModelSubstitution(c,
			epEffectiveURL(ep)+"/v1/audio/transcriptions",
			ep.UpstreamAPIKey,
			h.registry.ClientForEndpoint(ep),
			effectiveUpstreamModel, bodyBytes, true); err != nil {
			abortErr(c, http.StatusBadGateway, "upstream_error", err.Error())
			return
		}
	} else {
		// No model name substitution needed - forward as-is
		c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		if err := h.forwardRaw(c, epEffectiveURL(ep)+"/v1/audio/transcriptions",
			ep.UpstreamAPIKey,
			h.registry.ClientForEndpoint(ep)); err != nil {
			abortErr(c, http.StatusBadGateway, "upstream_error", err.Error())
			return
		}
	}

	h.usageTracker.Record(context.Background(), usage.Event{
		OrgID: res.orgID, TeamID: res.teamID,
		ModelName: res.realModel, EndpointID: ep.ID,
		LatencyMs: int(time.Since(start).Milliseconds()), Status: "success",
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// TTS — POST /v1/audio/speech
// ─────────────────────────────────────────────────────────────────────────────

func (h *Handler) Speech(c *gin.Context) {
	var req models.SpeechRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abortErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Model == "" {
		abortErr(c, http.StatusBadRequest, "missing_model", "Field 'model' is required")
		return
	}

	res, ok := h.pipelineSetup(c, req.Model, len(req.Input)/4)
	if !ok {
		return
	}
	defer h.decrementInflight(context.Background(), res.teamID, res.projectID, res.realModel)
	req.Model = res.realModel

	ep := res.ep
	atomic.AddInt64(&ep.ActiveConns, 1)
	defer atomic.AddInt64(&ep.ActiveConns, -1)
	if h.activator != nil {
		h.activator.RecordActivity(c.Request.Context(), ep.ID)
	}
	start := time.Now()

	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost,
		epEffectiveURL(ep)+"/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		abortErr(c, http.StatusInternalServerError, "request_build_error", err.Error())
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if ep.UpstreamAPIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+ep.UpstreamAPIKey)
	}

	resp, err := h.registry.ClientForEndpoint(ep).Do(httpReq)
	if err != nil {
		abortErr(c, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()

	audioData, _ := io.ReadAll(resp.Body)
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "audio/mpeg"
	}

	h.usageTracker.Record(context.Background(), usage.Event{
		OrgID: res.orgID, TeamID: res.teamID,
		ModelName: res.realModel, EndpointID: ep.ID,
		LatencyMs: int(time.Since(start).Milliseconds()),
		Status:    statusFromHTTP(resp.StatusCode),
	})
	c.Data(resp.StatusCode, ct, audioData)
}

// ─────────────────────────────────────────────────────────────────────────────
// OCR — POST /v1/ocr
// ─────────────────────────────────────────────────────────────────────────────

func (h *Handler) OCR(c *gin.Context) {
	var req models.OCRRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abortErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Model == "" {
		abortErr(c, http.StatusBadRequest, "missing_model", "Field 'model' is required")
		return
	}

	res, ok := h.pipelineSetup(c, req.Model, 0)
	if !ok {
		return
	}
	defer h.decrementInflight(context.Background(), res.teamID, res.projectID, res.realModel)
	req.Model = res.realModel

	ep := res.ep
	atomic.AddInt64(&ep.ActiveConns, 1)
	defer atomic.AddInt64(&ep.ActiveConns, -1)
	if h.activator != nil {
		h.activator.RecordActivity(c.Request.Context(), ep.ID)
	}
	start := time.Now()

	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost,
		epEffectiveURL(ep)+"/v1/ocr", bytes.NewReader(body))
	if err != nil {
		abortErr(c, http.StatusInternalServerError, "request_build_error", err.Error())
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if ep.UpstreamAPIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+ep.UpstreamAPIKey)
	}

	resp, err := h.registry.ClientForEndpoint(ep).Do(httpReq)
	if err != nil {
		abortErr(c, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	h.usageTracker.Record(context.Background(), usage.Event{
		OrgID: res.orgID, TeamID: res.teamID,
		ModelName: res.realModel, EndpointID: ep.ID,
		LatencyMs: int(time.Since(start).Milliseconds()),
		Status:    statusFromHTTP(resp.StatusCode),
	})
	c.Data(resp.StatusCode, "application/json", respBody)
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// forwardRaw proxies the current Gin request (body + headers) verbatim to
// targetURL and writes the response back. Used for binary/multipart requests
// where we must not buffer or reparse the body (e.g. STT audio upload).
// upstreamAPIKey, when non-empty, is injected as Authorization: Bearer.
// client must be the per-endpoint *http.Client from Registry.ClientForEndpoint —
// it carries the correct proxy, TLS, timeout, and connection-pool configuration.
func (h *Handler) forwardRaw(c *gin.Context, targetURL, upstreamAPIKey string, client *http.Client) error {
	httpReq, err := http.NewRequestWithContext(
		c.Request.Context(), c.Request.Method, targetURL, c.Request.Body)
	if err != nil {
		return err
	}
	for key, vals := range c.Request.Header {
		if key == "Authorization" || key == "Host" {
			continue // never forward internal auth tokens or client Host to backends
		}
		for _, v := range vals {
			httpReq.Header.Add(key, v)
		}
	}
	// Inject upstream key after stripping the client's Authorization header.
	if upstreamAPIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+upstreamAPIKey)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	for key, vals := range resp.Header {
		for _, v := range vals {
			c.Writer.Header().Add(key, v)
		}
	}
	c.Status(resp.StatusCode)
	_, _ = io.Copy(c.Writer, resp.Body)
	return nil
}

// forwardMultipartWithModelSubstitution rebuilds a multipart form request,
// replacing the "model" field value with upstreamModelName before forwarding.
// Used when the backend expects a different model identifier than the gateway
// (e.g. "whisper" → "large-v3" for faster-whisper-server).
// If stripUpstreamModel is true, the "upstream_model" field is removed from the
// forwarded request (it's a gateway-only field, not sent to the backend).
// client must be the per-endpoint *http.Client from Registry.ClientForEndpoint.
func (h *Handler) forwardMultipartWithModelSubstitution(c *gin.Context, targetURL, upstreamAPIKey string, client *http.Client, upstreamModelName string, originalBody []byte, stripUpstreamModel bool) error {
	// Parse the original multipart form
	boundary := extractBoundary(c.Request.Header.Get("Content-Type"))
	mr := multipart.NewReader(bytes.NewReader(originalBody), boundary)

	// Build a new multipart form with the substituted model name
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("parse multipart: %w", err)
		}

		// Create corresponding part in the new form
		var pw io.Writer
		fieldName := part.FormName()

		if fieldName == "model" {
			// Replace the model value with upstreamModelName
			pw, _ = mw.CreateFormField(fieldName)
			_, _ = io.WriteString(pw, upstreamModelName)
		} else if fieldName == "upstream_model" && stripUpstreamModel {
			// Skip the upstream_model field - it's gateway-only, not for the backend
			part.Close()
			continue
		} else if part.FileName() != "" {
			// File upload part (e.g. audio file)
			pw, _ = mw.CreateFormFile(fieldName, part.FileName())
			_, _ = io.Copy(pw, part)
		} else {
			// Regular form field
			pw, _ = mw.CreateFormField(fieldName)
			_, _ = io.Copy(pw, part)
		}
		part.Close()
	}
	mw.Close()

	// Create the HTTP request with the rewritten body
	httpReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, targetURL, &buf)
	if err != nil {
		return err
	}

	// Copy headers except Authorization and Content-Type (which we set below)
	for key, vals := range c.Request.Header {
		if key == "Authorization" || key == "Content-Type" {
			continue
		}
		for _, v := range vals {
			httpReq.Header.Add(key, v)
		}
	}

	// Set the new Content-Type with the new boundary
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())

	if upstreamAPIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+upstreamAPIKey)
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Forward response back to client
	for key, vals := range resp.Header {
		for _, v := range vals {
			c.Writer.Header().Add(key, v)
		}
	}
	c.Status(resp.StatusCode)
	_, _ = io.Copy(c.Writer, resp.Body)
	return nil
}

// epEffectiveURL returns the URL to use for an endpoint.
// If the endpoint has an UpstreamBaseURL (cloud/external), that is used.
// Otherwise falls back to the in-process host:port URL.
func epEffectiveURL(ep *runtime.Endpoint) string {
	if ep.UpstreamBaseURL != "" {
		return ep.UpstreamBaseURL
	}
	return ep.URL
}

func statusFromHTTP(code int) string {
	if code >= 200 && code < 300 {
		return "success"
	}
	return fmt.Sprintf("error_%d", code)
}

// extractBoundary parses the multipart boundary from a Content-Type header.
// Returns "" if the header is not multipart/form-data or has no boundary.
func extractBoundary(contentType string) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return params["boundary"]
}
