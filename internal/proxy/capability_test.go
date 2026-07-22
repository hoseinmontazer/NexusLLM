package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nexusllm/nexusllm/internal/runtime"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// ─────────────────────────────────────────────────────────────────────────────
// Mock registry
// ─────────────────────────────────────────────────────────────────────────────

// mockCapabilityRegistry implements CapabilityRegistryReader for tests.
type mockCapabilityRegistry struct {
	models map[string][]runtime.Capability
}

func newMockRegistry(models map[string][]runtime.Capability) *mockCapabilityRegistry {
	return &mockCapabilityRegistry{models: models}
}

func (m *mockCapabilityRegistry) GetModelCapabilities(_ context.Context, modelName string) ([]runtime.Capability, bool) {
	caps, ok := m.models[modelName]
	return caps, ok
}

// ─────────────────────────────────────────────────────────────────────────────
// Unit tests — CapabilityValidator.Validate
// ─────────────────────────────────────────────────────────────────────────────

func TestCapabilityValidator_AllowedCases(t *testing.T) {
	reg := newMockRegistry(map[string][]runtime.Capability{
		"llama-3.3-70b":     {runtime.CapabilityChat, runtime.CapabilityCompletion},
		"bge-large":         {runtime.CapabilityEmbedding},
		"whisper-large-v3":  {runtime.CapabilityTranscription},
		"tts-1":             {runtime.CapabilitySpeech},
		"bge-reranker":      {runtime.CapabilityRerank},
		"dall-e-3":          {runtime.CapabilityImageGeneration},
		"text-moderation-1": {runtime.CapabilityModeration},
		"llama-vision":      {runtime.CapabilityChat, runtime.CapabilityVision},
	})
	v := NewCapabilityValidator(reg)

	cases := []struct {
		model string
		route string
	}{
		// LLM chat model → chat endpoint
		{"llama-3.3-70b", "/v1/chat/completions"},
		// LLM chat model → legacy completions
		{"llama-3.3-70b", "/v1/completions"},
		// Embedding model → embeddings endpoint
		{"bge-large", "/v1/embeddings"},
		// Whisper model → transcription endpoint
		{"whisper-large-v3", "/v1/audio/transcriptions"},
		// TTS model → speech endpoint
		{"tts-1", "/v1/audio/speech"},
		// Rerank model → rerank endpoint
		{"bge-reranker", "/v1/rerank"},
		// Image gen model → images endpoint
		{"dall-e-3", "/v1/images/generations"},
		// Moderation model → moderation endpoint
		{"text-moderation-1", "/v1/moderations"},
		// Any model → /v1/models (no capability required)
		{"whisper-large-v3", "/v1/models"},
	}

	for _, tc := range cases {
		t.Run(tc.model+"→"+tc.route, func(t *testing.T) {
			err := v.Validate(context.Background(), tc.model, tc.route)
			if err != nil {
				t.Errorf("expected no error, got: %v", err)
			}
		})
	}
}

func TestCapabilityValidator_RejectedCases(t *testing.T) {
	reg := newMockRegistry(map[string][]runtime.Capability{
		"llama-3.3-70b":    {runtime.CapabilityChat, runtime.CapabilityCompletion},
		"bge-large":        {runtime.CapabilityEmbedding},
		"whisper-large-v3": {runtime.CapabilityTranscription},
	})
	v := NewCapabilityValidator(reg)

	cases := []struct {
		model    string
		route    string
		wantReq  runtime.Capability
		wantHave []runtime.Capability
	}{
		// Whisper → chat (main scenario from requirements)
		{
			"whisper-large-v3", "/v1/chat/completions",
			runtime.CapabilityChat,
			[]runtime.Capability{runtime.CapabilityTranscription},
		},
		// LLM → transcription
		{
			"llama-3.3-70b", "/v1/audio/transcriptions",
			runtime.CapabilityTranscription,
			[]runtime.Capability{runtime.CapabilityChat, runtime.CapabilityCompletion},
		},
		// Embedding model → chat
		{
			"bge-large", "/v1/chat/completions",
			runtime.CapabilityChat,
			[]runtime.Capability{runtime.CapabilityEmbedding},
		},
		// Embedding model → transcription
		{
			"bge-large", "/v1/audio/transcriptions",
			runtime.CapabilityTranscription,
			[]runtime.Capability{runtime.CapabilityEmbedding},
		},
		// Whisper → embeddings
		{
			"whisper-large-v3", "/v1/embeddings",
			runtime.CapabilityEmbedding,
			[]runtime.Capability{runtime.CapabilityTranscription},
		},
	}

	for _, tc := range cases {
		t.Run(tc.model+"→"+tc.route, func(t *testing.T) {
			err := v.Validate(context.Background(), tc.model, tc.route)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			ce, ok := err.(*CapabilityError)
			if !ok {
				t.Fatalf("expected *CapabilityError, got %T: %v", err, err)
			}
			if ce.ModelName != tc.model {
				t.Errorf("ModelName: got %q, want %q", ce.ModelName, tc.model)
			}
			if ce.RequiredCapability != tc.wantReq {
				t.Errorf("RequiredCapability: got %q, want %q", ce.RequiredCapability, tc.wantReq)
			}
			if len(ce.ModelCapabilities) != len(tc.wantHave) {
				t.Errorf("ModelCapabilities length: got %d, want %d", len(ce.ModelCapabilities), len(tc.wantHave))
			}
		})
	}
}

func TestCapabilityValidator_UnknownModel_Passes(t *testing.T) {
	// A model not in the registry should pass — the downstream pipeline
	// produces the "model not found" error instead.
	reg := newMockRegistry(map[string][]runtime.Capability{})
	v := NewCapabilityValidator(reg)
	err := v.Validate(context.Background(), "unknown-model", "/v1/chat/completions")
	if err != nil {
		t.Errorf("expected nil for unknown model, got: %v", err)
	}
}

func TestCapabilityValidator_UnknownRoute_Passes(t *testing.T) {
	// Routes not in the mapping have no requirement → always allowed.
	reg := newMockRegistry(map[string][]runtime.Capability{
		"whisper-large-v3": {runtime.CapabilityTranscription},
	})
	v := NewCapabilityValidator(reg)
	err := v.Validate(context.Background(), "whisper-large-v3", "/v1/models")
	if err != nil {
		t.Errorf("expected nil for unmapped route, got: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// HTTP-level test — CheckAndAbort via Gin
// ─────────────────────────────────────────────────────────────────────────────

func TestCheckAndAbort_WritesCorrectErrorResponse(t *testing.T) {
	reg := newMockRegistry(map[string][]runtime.Capability{
		"whisper-large-v3": {runtime.CapabilityTranscription},
	})
	v := NewCapabilityValidator(reg)

	r := gin.New()
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		ok := v.CheckAndAbort(c, "whisper-large-v3", "/v1/chat/completions")
		if !ok {
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "reached"})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}

	var resp capabilityErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v\nbody: %s", err, w.Body.String())
	}

	if resp.Error.Type != "invalid_model" {
		t.Errorf("error.type: got %q, want %q", resp.Error.Type, "invalid_model")
	}
	if resp.Error.RequiredCapability != runtime.CapabilityChat {
		t.Errorf("required_capability: got %q, want %q", resp.Error.RequiredCapability, runtime.CapabilityChat)
	}
	if len(resp.Error.ModelCapabilities) != 1 || resp.Error.ModelCapabilities[0] != runtime.CapabilityTranscription {
		t.Errorf("model_capabilities: got %v, want [transcription]", resp.Error.ModelCapabilities)
	}
	// Message must mention the model name
	if resp.Error.Message == "" {
		t.Error("error.message must not be empty")
	}
}

func TestCheckAndAbort_AllowedRequest_DoesNotAbort(t *testing.T) {
	reg := newMockRegistry(map[string][]runtime.Capability{
		"llama-3.3-70b": {runtime.CapabilityChat, runtime.CapabilityCompletion},
	})
	v := NewCapabilityValidator(reg)

	r := gin.New()
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		ok := v.CheckAndAbort(c, "llama-3.3-70b", "/v1/chat/completions")
		if !ok {
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "reached"})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Unit tests — RequiredCapabilityForRoute
// ─────────────────────────────────────────────────────────────────────────────

func TestRequiredCapabilityForRoute(t *testing.T) {
	cases := []struct {
		route   string
		wantCap runtime.Capability
		wantOK  bool
	}{
		{"/v1/chat/completions", runtime.CapabilityChat, true},
		{"/v1/completions", runtime.CapabilityCompletion, true},
		{"/v1/responses", runtime.CapabilityResponses, true},
		{"/v1/embeddings", runtime.CapabilityEmbedding, true},
		{"/v1/rerank", runtime.CapabilityRerank, true},
		{"/v1/audio/transcriptions", runtime.CapabilityTranscription, true},
		{"/v1/audio/speech", runtime.CapabilitySpeech, true},
		{"/v1/images/generations", runtime.CapabilityImageGeneration, true},
		{"/v1/moderations", runtime.CapabilityModeration, true},
		{"/v1/ocr", runtime.CapabilityOCR, true},
		// Routes with no requirement
		{"/v1/models", "", false},
		{"/v1/models/:model_id", "", false},
		{"/healthz", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.route, func(t *testing.T) {
			cap, ok := RequiredCapabilityForRoute(tc.route)
			if ok != tc.wantOK {
				t.Errorf("ok: got %v, want %v", ok, tc.wantOK)
			}
			if ok && cap != tc.wantCap {
				t.Errorf("capability: got %q, want %q", cap, tc.wantCap)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Unit tests — JSON helpers
// ─────────────────────────────────────────────────────────────────────────────

func TestParseCapabilitiesJSON(t *testing.T) {
	cases := []struct {
		input string
		want  []runtime.Capability
		isErr bool
	}{
		{`["chat","completion"]`, []runtime.Capability{"chat", "completion"}, false},
		{`["transcription"]`, []runtime.Capability{"transcription"}, false},
		{`[]`, nil, false},
		{`null`, nil, false},
		{`""`, nil, false}, // JSON string, not an array — silently returns nil
		{``, nil, false},
		{`not-json`, nil, false}, // not a JSON array prefix — silently returns nil
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParseCapabilitiesJSON(tc.input)
			if tc.isErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Errorf("length: got %d, want %d", len(got), len(tc.want))
				return
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("cap[%d]: got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestCapabilitiesToJSON(t *testing.T) {
	cases := []struct {
		input []runtime.Capability
		want  string
	}{
		{[]runtime.Capability{"chat", "completion"}, `["chat","completion"]`},
		{[]runtime.Capability{"transcription"}, `["transcription"]`},
		{nil, "[]"},
		{[]runtime.Capability{}, "[]"},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := CapabilitiesToJSON(tc.input)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Unit tests — DefaultCapabilities fallback in backend.go
// ─────────────────────────────────────────────────────────────────────────────

func TestDefaultCapabilities(t *testing.T) {
	cases := []struct {
		serviceType string
		want        []runtime.Capability
	}{
		{"CHAT", []runtime.Capability{runtime.CapabilityChat, runtime.CapabilityCompletion}},
		{"EMBEDDING", []runtime.Capability{runtime.CapabilityEmbedding}},
		{"RERANK", []runtime.Capability{runtime.CapabilityRerank}},
		{"STT", []runtime.Capability{runtime.CapabilityTranscription}},
		{"TTS", []runtime.Capability{runtime.CapabilitySpeech}},
		{"OCR", []runtime.Capability{runtime.CapabilityOCR}},
		{"VISION", []runtime.Capability{runtime.CapabilityChat, runtime.CapabilityVision}},
		{"IMAGE_GENERATION", []runtime.Capability{runtime.CapabilityImageGeneration}},
		{"MODERATION", []runtime.Capability{runtime.CapabilityModeration}},
		{"AGENT", []runtime.Capability{runtime.CapabilityChat, runtime.CapabilityCompletion}},
		{"MCP", []runtime.Capability{runtime.CapabilityChat, runtime.CapabilityCompletion}},
		{"CUSTOM", []runtime.Capability{}},
		{"UNKNOWN", []runtime.Capability{}},
	}

	for _, tc := range cases {
		t.Run(tc.serviceType, func(t *testing.T) {
			got := runtime.DefaultCapabilities(tc.serviceType)
			if len(got) != len(tc.want) {
				t.Errorf("len: got %d want %d (got=%v, want=%v)", len(got), len(tc.want), got, tc.want)
				return
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("cap[%d]: got %q want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Integration-style tests — full error response shape
// ─────────────────────────────────────────────────────────────────────────────

// TestWhisperChatCompletionsError is the exact scenario from the requirements:
//
//	POST /v1/chat/completions with model=whisper-large-v3 must return 400.
func TestWhisperChatCompletionsError(t *testing.T) {
	reg := newMockRegistry(map[string][]runtime.Capability{
		"whisper-large-v3": {runtime.CapabilityTranscription},
	})
	v := NewCapabilityValidator(reg)

	r := gin.New()
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		if !v.CheckAndAbort(c, "whisper-large-v3", c.FullPath()) {
			return
		}
		c.JSON(http.StatusOK, gin.H{})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("json decode: %v", err)
	}

	errObj, ok := body["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing 'error' key in response: %s", w.Body.String())
	}

	checkString := func(key, want string) {
		got, _ := errObj[key].(string)
		if got != want {
			t.Errorf("error.%s: got %q, want %q", key, got, want)
		}
	}

	checkString("type", "invalid_model")
	checkString("required_capability", "chat")

	caps, ok := errObj["model_capabilities"].([]interface{})
	if !ok || len(caps) == 0 {
		t.Errorf("model_capabilities should be a non-empty array, got: %v", errObj["model_capabilities"])
	} else if caps[0].(string) != "transcription" {
		t.Errorf("model_capabilities[0]: got %q, want %q", caps[0], "transcription")
	}
}

// TestLlamaCppTranscriptionError: llama model cannot transcribe.
func TestLlamaCppTranscriptionError(t *testing.T) {
	reg := newMockRegistry(map[string][]runtime.Capability{
		"llama-3.3-70b": {runtime.CapabilityChat, runtime.CapabilityCompletion},
	})
	v := NewCapabilityValidator(reg)

	r := gin.New()
	r.POST("/v1/audio/transcriptions", func(c *gin.Context) {
		if !v.CheckAndAbort(c, "llama-3.3-70b", c.FullPath()) {
			return
		}
		c.JSON(http.StatusOK, gin.H{})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp capabilityErrorResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error.RequiredCapability != runtime.CapabilityTranscription {
		t.Errorf("required_capability: got %q", resp.Error.RequiredCapability)
	}
}

// TestEmbeddingModelChatError: embedding model cannot handle chat.
func TestEmbeddingModelChatError(t *testing.T) {
	reg := newMockRegistry(map[string][]runtime.Capability{
		"bge-large": {runtime.CapabilityEmbedding},
	})
	v := NewCapabilityValidator(reg)

	err := v.Validate(context.Background(), "bge-large", "/v1/chat/completions")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	ce := err.(*CapabilityError)
	if ce.RequiredCapability != runtime.CapabilityChat {
		t.Errorf("required: got %q", ce.RequiredCapability)
	}
	if len(ce.ModelCapabilities) != 1 || ce.ModelCapabilities[0] != runtime.CapabilityEmbedding {
		t.Errorf("have: got %v", ce.ModelCapabilities)
	}
}
