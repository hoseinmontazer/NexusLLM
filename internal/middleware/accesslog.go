package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// AccessLog returns a Gin middleware that writes one structured log line per
// request after it completes. Fields logged:
//
//	request_id   — from X-Nexus-Request-ID (injected by RequestID middleware)
//	method       — GET / POST / …
//	path         — /v1/chat/completions
//	status       — HTTP response status code
//	latency_ms   — wall-clock time from first byte received to last byte sent
//	team         — authenticated team name (empty for unauthenticated routes)
//	model        — model name extracted by the proxy handler ("" if not a chat route)
//	endpoint_id  — backend endpoint that served the request
//	ip           — client remote address
//	user_agent   — client User-Agent
//	bytes_out    — response body size in bytes
//
// Log level:
//
//	INFO  — 2xx and 3xx
//	WARN  — 4xx (client errors, policy blocks, bad requests)
//	ERROR — 5xx (upstream failures, panics caught by recovery)
//
// Skipped routes: /healthz and /readyz (high-frequency, low-value).
func AccessLog(log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip liveness/readiness probes — they fire every few seconds and
		// add noise without providing debugging value.
		if c.Request.URL.Path == "/healthz" || c.Request.URL.Path == "/readyz" {
			c.Next()
			return
		}

		start := time.Now()
		c.Next() // ← execute the full handler chain
		latency := time.Since(start)

		// Gather context populated by downstream handlers.
		reqID := c.GetString(RequestIDKey)
		model := c.GetString("model")
		endpointID := c.GetHeader("X-Nexus-Endpoint") // set by proxy handler on response

		claims := GetClaims(c)
		team := ""
		teamID := ""
		if claims != nil {
			team = claims.TeamName
			teamID = claims.TeamID
		}

		status := c.Writer.Status()
		bytesOut := c.Writer.Size()
		if bytesOut < 0 {
			bytesOut = 0
		}

		fields := []zap.Field{
			zap.String("request_id", reqID),
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
			zap.Int("status", status),
			zap.Int64("latency_ms", latency.Milliseconds()),
			zap.String("ip", c.ClientIP()),
			zap.String("user_agent", c.Request.UserAgent()),
			zap.Int("bytes_out", bytesOut),
		}
		if team != "" {
			fields = append(fields, zap.String("team", team))
		}
		if teamID != "" {
			fields = append(fields, zap.String("team_id", teamID))
		}
		if model != "" {
			fields = append(fields, zap.String("model", model))
		}
		if endpointID != "" {
			fields = append(fields, zap.String("endpoint_id", endpointID))
		}
		// Include any errors gin collected (e.g. binding failures).
		if len(c.Errors) > 0 {
			fields = append(fields, zap.String("errors", c.Errors.String()))
		}

		switch {
		case status >= 500:
			log.Error("request", fields...)
		case status >= 400:
			log.Warn("request", fields...)
		default:
			log.Info("request", fields...)
		}
	}
}
