// Package proxy — capability.go
//
// # Model Capability Validation
//
// Every incoming inference request is checked against the model's declared
// capabilities before being routed to any backend. This prevents, for
// example, a chat request being forwarded to a Whisper transcription model.
//
// Validation happens BEFORE endpoint resolution so the backend never sees
// incompatible requests. The check is engine-independent — it operates only
// on the model's capability metadata stored in the models.capabilities JSONB
// column (single source of truth).
//
// Endpoint → Capability mapping:
//
//	/v1/chat/completions       → chat
//	/v1/completions            → completion
//	/v1/responses              → responses
//	/v1/embeddings             → embedding
//	/v1/rerank                 → rerank
//	/v1/audio/transcriptions   → transcription
//	/v1/audio/speech           → speech
//	/v1/images/generations     → image_generation
//	/v1/moderations            → moderation
//	/v1/ocr                    → ocr
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/nexusllm/nexusllm/internal/runtime"
)

// ─────────────────────────────────────────────────────────────────────────────
// Endpoint → Capability mapping
// ─────────────────────────────────────────────────────────────────────────────

// endpointCapability maps a gateway route path to the Capability it requires.
// The key is the Gin route pattern (as registered in cmd/gateway/main.go).
var endpointCapability = map[string]runtime.Capability{
	"/v1/chat/completions":     runtime.CapabilityChat,
	"/v1/completions":          runtime.CapabilityCompletion,
	"/v1/responses":            runtime.CapabilityResponses,
	"/v1/embeddings":           runtime.CapabilityEmbedding,
	"/v1/rerank":               runtime.CapabilityRerank,
	"/v1/audio/transcriptions": runtime.CapabilityTranscription,
	"/v1/audio/speech":         runtime.CapabilitySpeech,
	"/v1/images/generations":   runtime.CapabilityImageGeneration,
	"/v1/moderations":          runtime.CapabilityModeration,
	"/v1/ocr":                  runtime.CapabilityOCR,
}

// RequiredCapabilityForRoute returns the Capability required by a given route path.
// The second return value is false when the route has no capability requirement
// (e.g. /v1/models) — those routes are always allowed through.
func RequiredCapabilityForRoute(routePath string) (runtime.Capability, bool) {
	cap, ok := endpointCapability[routePath]
	return cap, ok
}

// ─────────────────────────────────────────────────────────────────────────────
// CapabilityError — standard error response
// ─────────────────────────────────────────────────────────────────────────────

// CapabilityError is returned when a model does not support the requested endpoint.
type CapabilityError struct {
	ModelName          string
	RequiredCapability runtime.Capability
	ModelCapabilities  []runtime.Capability
}

func (e *CapabilityError) Error() string {
	return fmt.Sprintf("model %q does not support %q (has: %v)",
		e.ModelName, e.RequiredCapability, e.ModelCapabilities)
}

// capabilityErrorResponse is the JSON payload returned for capability mismatches.
type capabilityErrorResponse struct {
	Error capabilityErrorDetail `json:"error"`
}

type capabilityErrorDetail struct {
	Type               string               `json:"type"`
	Message            string               `json:"message"`
	RequiredCapability runtime.Capability   `json:"required_capability"`
	ModelCapabilities  []runtime.Capability `json:"model_capabilities"`
}

// friendlyEndpointName returns a human-readable description of an endpoint.
func friendlyEndpointName(cap runtime.Capability) string {
	switch cap {
	case runtime.CapabilityChat:
		return "Chat Completions"
	case runtime.CapabilityCompletion:
		return "Text Completions"
	case runtime.CapabilityResponses:
		return "Responses"
	case runtime.CapabilityEmbedding:
		return "Embeddings"
	case runtime.CapabilityRerank:
		return "Rerank"
	case runtime.CapabilityTranscription:
		return "Audio Transcription"
	case runtime.CapabilitySpeech:
		return "Audio Speech"
	case runtime.CapabilityImageGeneration:
		return "Image Generation"
	case runtime.CapabilityModeration:
		return "Moderation"
	case runtime.CapabilityOCR:
		return "OCR"
	default:
		return string(cap)
	}
}

// abortCapabilityError writes a standard HTTP 400 capability error response and
// aborts the Gin chain. Must be called before any other response is written.
func abortCapabilityError(c *gin.Context, modelName string, required runtime.Capability, have []runtime.Capability) {
	if have == nil {
		have = []runtime.Capability{}
	}
	c.AbortWithStatusJSON(http.StatusBadRequest, capabilityErrorResponse{
		Error: capabilityErrorDetail{
			Type: "invalid_model",
			Message: fmt.Sprintf("Model '%s' does not support %s.",
				modelName, friendlyEndpointName(required)),
			RequiredCapability: required,
			ModelCapabilities:  have,
		},
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// CapabilityValidator
// ─────────────────────────────────────────────────────────────────────────────

// CapabilityValidator fetches model capabilities from the registry and validates
// that a given model supports the requested API endpoint.
//
// It is the single authority for capability validation — no backend adapter or
// runtime-specific code performs this check.
type CapabilityValidator struct {
	registry        CapabilityRegistryReader
	catalogResolver catalogCapabilityReader // nil-safe; set via WithCatalogResolver
}

// catalogCapabilityReader is the subset of catalog.VirtualModelResolver used
// for capability lookup. Defined as an interface to avoid an import cycle.
type catalogCapabilityReader interface {
	Capabilities(ctx context.Context, modelName string) ([]runtime.Capability, bool)
}

// CapabilityRegistryReader is the subset of runtime.Registry used by the validator.
// Using an interface makes it trivial to inject a mock in tests.
type CapabilityRegistryReader interface {
	// GetModelCapabilities returns the capability list for a model.
	// Returns (nil, false) when the model is not found or has no metadata.
	GetModelCapabilities(ctx context.Context, modelName string) ([]runtime.Capability, bool)
}

// NewCapabilityValidator constructs a CapabilityValidator backed by reg.
func NewCapabilityValidator(reg CapabilityRegistryReader) *CapabilityValidator {
	return &CapabilityValidator{registry: reg}
}

// WithCatalogResolver attaches a catalog resolver so that virtual (Mode-B)
// models can also be capability-validated without being in the registry pool.
func (v *CapabilityValidator) WithCatalogResolver(r catalogCapabilityReader) *CapabilityValidator {
	v.catalogResolver = r
	return v
}

// Validate checks whether modelName supports the capability required by
// routePath. Returns nil when the request is allowed. Returns a
// *CapabilityError when the model does not support the endpoint.
//
// Special cases:
//   - routePath not in the mapping → no requirement → always allowed (returns nil)
//   - model capabilities unknown (not in DB yet) → falls back to DefaultCapabilities
//     derived from the service_type column so the gateway is never more restrictive
//     than the old behaviour
func (v *CapabilityValidator) Validate(ctx context.Context, modelName, routePath string) error {
	required, hasCap := RequiredCapabilityForRoute(routePath)
	if !hasCap {
		// No capability requirement for this route.
		return nil
	}

	caps, found := v.registry.GetModelCapabilities(ctx, modelName)
	if !found {
		// Fall back to catalog resolver for virtual (Mode-B) models.
		if v.catalogResolver != nil {
			if catalogCaps, ok := v.catalogResolver.Capabilities(ctx, modelName); ok {
				caps = catalogCaps
				found = true
			}
		}
	}
	if !found {
		// Model not in registry or catalog — capability check cannot be performed.
		// The downstream pipeline will handle the "model not found" error.
		return nil
	}

	for _, c := range caps {
		if c == required {
			return nil // supported
		}
	}

	return &CapabilityError{
		ModelName:          modelName,
		RequiredCapability: required,
		ModelCapabilities:  caps,
	}
}

// CheckAndAbort is the Gin-integrated helper. It runs Validate and, on failure,
// writes the error response and aborts the chain. Returns true when the check
// passes (caller should continue), false when it aborted (caller must return).
func (v *CapabilityValidator) CheckAndAbort(c *gin.Context, modelName, routePath string) bool {
	err := v.Validate(c.Request.Context(), modelName, routePath)
	if err == nil {
		return true
	}
	ce, ok := err.(*CapabilityError)
	if !ok {
		// Unexpected error type — treat as a pass to avoid blocking valid requests.
		return true
	}
	abortCapabilityError(c, ce.ModelName, ce.RequiredCapability, ce.ModelCapabilities)
	return false
}

// ─────────────────────────────────────────────────────────────────────────────
// JSON helpers for capabilities JSONB column
// ─────────────────────────────────────────────────────────────────────────────

// ParseCapabilitiesJSON decodes a raw JSONB string (e.g. `["chat","embedding"]`)
// into a []Capability slice. Returns nil on empty/null/non-array input.
func ParseCapabilitiesJSON(raw string) ([]runtime.Capability, error) {
	if raw == "" || raw == "null" || raw == "[]" {
		return nil, nil
	}
	// Only try to parse JSON arrays — reject bare strings, numbers, objects.
	if len(raw) == 0 || raw[0] != '[' {
		return nil, nil
	}
	var strs []string
	if err := json.Unmarshal([]byte(raw), &strs); err != nil {
		return nil, fmt.Errorf("parse capabilities JSON: %w", err)
	}
	caps := make([]runtime.Capability, len(strs))
	for i, s := range strs {
		caps[i] = runtime.Capability(s)
	}
	return caps, nil
}

// CapabilitiesToJSON serializes a []Capability slice to a JSON array string
// suitable for storage in a JSONB column.
func CapabilitiesToJSON(caps []runtime.Capability) string {
	if len(caps) == 0 {
		return "[]"
	}
	strs := make([]string, len(caps))
	for i, c := range caps {
		strs[i] = string(c)
	}
	b, _ := json.Marshal(strs)
	return string(b)
}
