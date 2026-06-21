package proxy

// multiservice.go — extends the proxy.Handler with multi-service API handlers.
//
// New endpoints:
//   POST /v1/rerank
//   POST /v1/audio/transcriptions
//   POST /v1/audio/speech
//   POST /v1/ocr
//
// Each handler follows the same pipeline as ChatCompletions:
//   Auth → Policy → Alias → Endpoint resolution → Backend dispatch → Usage
//
// The gateway routes to the correct runtime automatically via the service_type
// stored in the models table. No consumer-visible API change is required when
// models are swapped behind a service name.

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
	"github.com/nexusllm/nexusllm/internal/usage"
)

// ─────────────────────────────────────────────────────────────────────────────
// Rerank — POST /v1/rerank
// ─────────────────────────────────────────────────────────────────────────────

// Rerank handles POST /v1/rerank.
// Compatible with Cohere's rerank API and Jina AI reranker format.
func (h *Handler) Rerank(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		abortErr(c, http.StatusUnauthorized, "unauthorized", "Missing authentication")
		return
	}

	var req models.RerankRequest
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

	// Policy check (document count is used as a proxy for token cost)
	tp := h.teamPolicy(claims.TeamID)
	decision := h.policy.Evaluate(c.Request.Context(), &policy.InferenceRequest{
		Model:                req.Model,
		EstimatedInputTokens: len(req.Documents) * 50, // rough estimate
		TeamID:               claims.TeamID,
	}, claims.TeamPriority, tp)
	if !decision.Allowed {
		middleware.RecordRejection(claims.TeamName, decision.RejectReason)
		abortErr(c, http.StatusForbidden, decision.RejectReason, "Request rejected")
		return
	}

	ep, _, err := h.registry.ResolveWithFailover(req.Model, maxFailoverAttempts)
	if err != nil {
		abortErr(c, http.StatusServiceUnavailable, "no_healthy_endpoint", err.Error())
		return
	}

	atomic.AddInt64(&ep.ActiveConns, 1)
	defer atomic.AddInt64(&ep.ActiveConns, -1)
	h.lifecycleMgr.RecordActivity(c.Request.Context(), ep.ID)

	start := time.Now()

	// Forward to backend (OpenAI-compat or Cohere-compat rerank endpoint)
	body, err := json.Marshal(req)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, "marshal_error", err.Error())
		return
	}

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

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		abortErr(c, http.StatusBadGateway, "read_error", err.Error())
		return
	}

	latencyMs := int(time.Since(start).Milliseconds())
	h.usageTracker.Record(context.Background(), usage.Event{
		OrgID: claims.OrgID, TeamID: claims.TeamID,
		ModelName:  req.Model,
		EndpointID: ep.ID,
		LatencyMs:  latencyMs,
		Status:     statusFromHTTP(resp.StatusCode),
	})

	c.Data(resp.StatusCode, "application/json", respBody)
}

// ─────────────────────────────────────────────────────────────────────────────
// STT — POST /v1/audio/transcriptions
// ─────────────────────────────────────────────────────────────────────────────

// Transcriptions handles POST /v1/audio/transcriptions.
// Proxies to faster-whisper-server, whisper.cpp HTTP server, or any
// OpenAI-compatible STT backend.
//
// The request is multipart/form-data (OpenAI convention) and is forwarded
// as-is to the backend — no body parsing needed at the gateway layer.
func (h *Handler) Transcriptions(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		abortErr(c, http.StatusUnauthorized, "unauthorized", "Missing authentication")
		return
	}

	// Read the model from the form field (multipart)
	modelName := c.PostForm("model")
	if modelName == "" {
		modelName = "whisper" // default alias
	}

	realModel, _ := h.aliasResolver.Resolve(c.Request.Context(), modelName, claims.TeamID, claims.OrgID)
	c.Set("model", realModel)

	tp := h.teamPolicy(claims.TeamID)
	decision := h.policy.Evaluate(c.Request.Context(), &policy.InferenceRequest{
		Model:  realModel,
		TeamID: claims.TeamID,
	}, claims.TeamPriority, tp)
	if !decision.Allowed {
		middleware.RecordRejection(claims.TeamName, decision.RejectReason)
		abortErr(c, http.StatusForbidden, decision.RejectReason, "Request rejected")
		return
	}

	ep, _, err := h.registry.ResolveWithFailover(realModel, maxFailoverAttempts)
	if err != nil {
		abortErr(c, http.StatusServiceUnavailable, "no_healthy_endpoint", err.Error())
		return
	}

	atomic.AddInt64(&ep.ActiveConns, 1)
	defer atomic.AddInt64(&ep.ActiveConns, -1)
	h.lifecycleMgr.RecordActivity(c.Request.Context(), ep.ID)
	start := time.Now()

	// Forward the raw multipart body to the backend
	proxyURL := ep.URL + "/v1/audio/transcriptions"
	if err := h.forwardRaw(c, proxyURL); err != nil {
		abortErr(c, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	latencyMs := int(time.Since(start).Milliseconds())
	h.usageTracker.Record(context.Background(), usage.Event{
		OrgID: claims.OrgID, TeamID: claims.TeamID,
		ModelName: realModel, EndpointID: ep.ID,
		LatencyMs: latencyMs, Status: "success",
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// TTS — POST /v1/audio/speech
// ─────────────────────────────────────────────────────────────────────────────

// Speech handles POST /v1/audio/speech.
// Proxies to Kokoro TTS, Coqui, or any OpenAI-compatible TTS backend.
// Returns binary audio — the Content-Type from the backend is passed through.
func (h *Handler) Speech(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		abortErr(c, http.StatusUnauthorized, "unauthorized", "Missing authentication")
		return
	}

	var req models.SpeechRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abortErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Model == "" {
		req.Model = "tts"
	}

	realModel, _ := h.aliasResolver.Resolve(c.Request.Context(), req.Model, claims.TeamID, claims.OrgID)
	req.Model = realModel
	c.Set("model", realModel)

	tp := h.teamPolicy(claims.TeamID)
	decision := h.policy.Evaluate(c.Request.Context(), &policy.InferenceRequest{
		Model:                realModel,
		EstimatedInputTokens: len(req.Input) / 4,
		TeamID:               claims.TeamID,
	}, claims.TeamPriority, tp)
	if !decision.Allowed {
		middleware.RecordRejection(claims.TeamName, decision.RejectReason)
		abortErr(c, http.StatusForbidden, decision.RejectReason, "Request rejected")
		return
	}

	ep, _, err := h.registry.ResolveWithFailover(realModel, maxFailoverAttempts)
	if err != nil {
		abortErr(c, http.StatusServiceUnavailable, "no_healthy_endpoint", err.Error())
		return
	}

	atomic.AddInt64(&ep.ActiveConns, 1)
	defer atomic.AddInt64(&ep.ActiveConns, -1)
	h.lifecycleMgr.RecordActivity(c.Request.Context(), ep.ID)
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

	audioData, err := io.ReadAll(resp.Body)
	if err != nil {
		abortErr(c, http.StatusBadGateway, "read_error", err.Error())
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "audio/mpeg"
	}

	latencyMs := int(time.Since(start).Milliseconds())
	h.usageTracker.Record(context.Background(), usage.Event{
		OrgID: claims.OrgID, TeamID: claims.TeamID,
		ModelName: realModel, EndpointID: ep.ID,
		LatencyMs: latencyMs, Status: statusFromHTTP(resp.StatusCode),
	})

	c.Data(resp.StatusCode, contentType, audioData)
}

// ─────────────────────────────────────────────────────────────────────────────
// OCR — POST /v1/ocr
// ─────────────────────────────────────────────────────────────────────────────

// OCR handles POST /v1/ocr.
// Routes to any registered OCR service backend.
func (h *Handler) OCR(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		abortErr(c, http.StatusUnauthorized, "unauthorized", "Missing authentication")
		return
	}

	var req models.OCRRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abortErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Model == "" {
		req.Model = "ocr"
	}

	realModel, _ := h.aliasResolver.Resolve(c.Request.Context(), req.Model, claims.TeamID, claims.OrgID)
	req.Model = realModel
	c.Set("model", realModel)

	tp := h.teamPolicy(claims.TeamID)
	decision := h.policy.Evaluate(c.Request.Context(), &policy.InferenceRequest{
		Model:  realModel,
		TeamID: claims.TeamID,
	}, claims.TeamPriority, tp)
	if !decision.Allowed {
		middleware.RecordRejection(claims.TeamName, decision.RejectReason)
		abortErr(c, http.StatusForbidden, decision.RejectReason, "Request rejected")
		return
	}

	ep, _, err := h.registry.ResolveWithFailover(realModel, maxFailoverAttempts)
	if err != nil {
		abortErr(c, http.StatusServiceUnavailable, "no_healthy_endpoint", err.Error())
		return
	}

	atomic.AddInt64(&ep.ActiveConns, 1)
	defer atomic.AddInt64(&ep.ActiveConns, -1)
	h.lifecycleMgr.RecordActivity(c.Request.Context(), ep.ID)
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
	latencyMs := int(time.Since(start).Milliseconds())
	h.usageTracker.Record(context.Background(), usage.Event{
		OrgID: claims.OrgID, TeamID: claims.TeamID,
		ModelName: realModel, EndpointID: ep.ID,
		LatencyMs: latencyMs, Status: statusFromHTTP(resp.StatusCode),
	})

	c.Data(resp.StatusCode, "application/json", respBody)
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// forwardRaw proxies the current Gin request (body + headers) verbatim to
// targetURL and writes the response back. Used for binary/multipart requests
// where we must not buffer or reparse the body.
func (h *Handler) forwardRaw(c *gin.Context, targetURL string) error {
	httpReq, err := http.NewRequestWithContext(
		c.Request.Context(), c.Request.Method, targetURL, c.Request.Body)
	if err != nil {
		return err
	}
	// Forward relevant headers
	for key, vals := range c.Request.Header {
		if key == "Authorization" {
			continue // do not forward internal auth to backends
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

	// Copy response headers
	for key, vals := range resp.Header {
		for _, v := range vals {
			c.Header(key, v)
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
