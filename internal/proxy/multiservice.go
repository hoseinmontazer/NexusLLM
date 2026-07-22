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
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nexusllm/nexusllm/internal/middleware"
	"github.com/nexusllm/nexusllm/internal/models"
	"github.com/nexusllm/nexusllm/internal/policy"
	"github.com/nexusllm/nexusllm/internal/runtime"
	"github.com/nexusllm/nexusllm/internal/usage"
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

	// ── 6. EnsureRunning (lazy-load cold start) ───────────────────────────────
	if h.activator != nil {
		if _, _, err := h.registry.ResolveWithFailover(realModel, 1); err != nil {
			probeCtx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
			_, probeErr := h.activator.EnsureRunning(probeCtx, realModel)
			cancel()
			if probeErr != nil {
				go func() {
					bgCtx, bgCancel := context.WithTimeout(context.Background(), h.coldStartTimeout())
					defer bgCancel()
					_, _ = h.activator.EnsureRunning(bgCtx, realModel)
				}()
				c.Header("Retry-After", "10")
				abortErr(c, http.StatusServiceUnavailable, "model_starting",
					fmt.Sprintf("model %q is starting, please retry in ~10 seconds", realModel))
				return pipelineResult{}, false
			}
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
	ep, _, err := h.registry.ResolveWithFailover(realModel, maxFailoverAttempts)
	if err != nil {
		// Roll back inflight — endpoint couldn't be resolved after cold start.
		h.decrementInflight(c.Request.Context(), claims.TeamID, claims.ProjectID, realModel)
		abortErr(c, http.StatusServiceUnavailable, "no_healthy_endpoint", err.Error())
		return pipelineResult{}, false
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
		ep.URL+"/v1/rerank", bytes.NewReader(body))
	if err != nil {
		abortErr(c, http.StatusInternalServerError, "request_build_error", err.Error())
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(httpReq)
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
	rawModel := c.PostForm("model")
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

	if err := h.forwardRaw(c, ep.URL+"/v1/audio/transcriptions"); err != nil {
		abortErr(c, http.StatusBadGateway, "upstream_error", err.Error())
		return
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
		ep.URL+"/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		abortErr(c, http.StatusInternalServerError, "request_build_error", err.Error())
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(httpReq)
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
		ep.URL+"/v1/ocr", bytes.NewReader(body))
	if err != nil {
		abortErr(c, http.StatusInternalServerError, "request_build_error", err.Error())
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(httpReq)
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
func (h *Handler) forwardRaw(c *gin.Context, targetURL string) error {
	httpReq, err := http.NewRequestWithContext(
		c.Request.Context(), c.Request.Method, targetURL, c.Request.Body)
	if err != nil {
		return err
	}
	for key, vals := range c.Request.Header {
		if key == "Authorization" {
			continue // never forward internal auth tokens to backends
		}
		for _, v := range vals {
			httpReq.Header.Add(key, v)
		}
	}
	resp, err := h.httpClient.Do(httpReq)
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

func statusFromHTTP(code int) string {
	if code >= 200 && code < 300 {
		return "success"
	}
	return fmt.Sprintf("error_%d", code)
}
