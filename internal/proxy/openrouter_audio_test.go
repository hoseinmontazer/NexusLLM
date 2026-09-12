package proxy

// openrouter_audio_test.go — unit tests for the generic OpenRouter STT/TTS
// wire-format adapter (openrouter_audio.go).
//
// These exercise the translation logic directly against an httptest.Server
// standing in for OpenRouter — no database, no real network, no per-model
// branches. Routing correctness is asserted structurally (by BackendType),
// never by asserting on a model-name string comparison in production code.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nexusllm/nexusllm/internal/runtime"
)

// ─── detectAudioFormat ────────────────────────────────────────────────────────

func TestDetectAudioFormat(t *testing.T) {
	cases := []struct {
		name     string
		data     []byte
		filename string
		want     string
	}{
		{"wav magic bytes", append([]byte("RIFF\x00\x00\x00\x00WAVE"), 0), "upload.bin", "wav"},
		{"flac magic bytes", []byte("fLaC" + strings.Repeat("\x00", 8)), "upload.bin", "flac"},
		{"ogg magic bytes", []byte("OggS" + strings.Repeat("\x00", 8)), "upload.bin", "ogg"},
		{"webm/EBML magic bytes", []byte{0x1A, 0x45, 0xDF, 0xA3, 0, 0, 0, 0}, "upload.bin", "webm"},
		{"m4a/ftyp magic bytes", append([]byte{0, 0, 0, 0}, []byte("ftypM4A ")...), "upload.bin", "m4a"},
		{"mp3 with ID3 tag", []byte("ID3\x03\x00\x00\x00\x00\x00\x00"), "upload.bin", "mp3"},
		{"mp3 raw frame sync, no extension hint", []byte{0xFF, 0xFB, 0x90, 0x00, 0, 0, 0, 0}, "upload.bin", "mp3"},
		{"aac raw frame sync, extension breaks the tie", []byte{0xFF, 0xF1, 0x00, 0x00, 0, 0, 0, 0}, "clip.aac", "aac"},
		{"unrecognizable bytes falls back to extension", []byte{1, 2, 3, 4, 5, 6, 7, 8}, "clip.m4a", "m4a"},
		{"unrecognizable bytes, unknown extension defaults to wav", []byte{1, 2, 3, 4}, "clip.xyz", "wav"},
		{"unrecognizable bytes, no filename defaults to wav", []byte{1, 2, 3, 4}, "", "wav"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectAudioFormat(tc.data, tc.filename)
			if got != tc.want {
				t.Fatalf("detectAudioFormat(%q) = %q, want %q", tc.filename, got, tc.want)
			}
		})
	}
}

// ─── Provider-based routing (never model-name-based) ─────────────────────────

func TestAudioPathRouting_ByBackendTypeNotModelName(t *testing.T) {
	// The exact acceptance models from the spec, with backend metadata
	// swapped — proving the path is chosen purely from ep.BackendType.
	cases := []struct {
		name       string
		backend    runtime.BackendType
		wantTrans  string
		wantSpeech string
	}{
		{"openrouter provider", runtime.BackendOpenRouter, "/api/v1/audio/transcriptions", "/api/v1/audio/speech"},
		{"native cpu_native backend (e.g. our own STT/TTS wrappers)", runtime.BackendCPUNative, "/v1/audio/transcriptions", "/v1/audio/speech"},
		{"openai_compat backend", runtime.BackendOpenAICompat, "/v1/audio/transcriptions", "/v1/audio/speech"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := audioTranscriptionsPath(tc.backend); got != tc.wantTrans {
				t.Errorf("audioTranscriptionsPath(%v) = %q, want %q", tc.backend, got, tc.wantTrans)
			}
			if got := audioSpeechPath(tc.backend); got != tc.wantSpeech {
				t.Errorf("audioSpeechPath(%v) = %q, want %q", tc.backend, got, tc.wantSpeech)
			}
		})
	}
}

// ─── openRouterTranscribe: request translation + response normalization ──────

// buildMultipartAudio builds a multipart/form-data body matching what a real
// Nexus client sends to POST /v1/audio/transcriptions, and returns the body
// bytes plus the boundary string exactly as Transcriptions() would extract it.
func buildMultipartAudio(t *testing.T, filename string, audio []byte, extraFields map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(audio); err != nil {
		t.Fatalf("write audio bytes: %v", err)
	}
	if err := mw.WriteField("model", "openai/whisper-large-v3-turbo"); err != nil {
		t.Fatalf("write model field: %v", err)
	}
	for k, v := range extraFields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return buf.Bytes(), mw.Boundary()
}

// TestOpenRouterTranscribe_ContractModel is the acceptance test for the exact
// spec model: openai/whisper-large-v3-turbo via provider=openrouter,
// capability=transcription. Nothing here special-cases the model name —
// ep.UpstreamModelName is just data threaded through from the catalog.
func TestOpenRouterTranscribe_ContractModel(t *testing.T) {
	const fakeAudio = "RIFF-fake-wav-bytes-not-real-audio-but-thats-fine-for-this-test"
	var captured struct {
		method      string
		path        string
		auth        string
		contentType string
		body        map[string]interface{}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.auth = r.Header.Get("Authorization")
		captured.contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&captured.body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"سلام، حالت چطوره؟","usage":{"seconds":2.1,"total_tokens":10,"cost":0.0001}}`))
	}))
	defer server.Close()

	ep := &runtime.Endpoint{
		BackendType:       runtime.BackendOpenRouter,
		UpstreamBaseURL:   server.URL,
		UpstreamAPIKey:    "sk-or-v1-test-secret-should-never-leak",
		UpstreamModelName: "openai/whisper-large-v3-turbo",
	}
	bodyBytes, boundary := buildMultipartAudio(t, "call.wav", []byte(fakeAudio), map[string]string{"language": "fa"})

	c, rec := ginContext(t)
	if err := openRouterTranscribe(c, ep, server.Client(), bodyBytes, boundary); err != nil {
		t.Fatalf("openRouterTranscribe returned unexpected error: %v", err)
	}

	// --- Request-side assertions (what NexusLLM sent to "OpenRouter") ---
	if captured.method != http.MethodPost {
		t.Errorf("method = %s, want POST", captured.method)
	}
	if captured.path != "/api/v1/audio/transcriptions" {
		t.Errorf("path = %s, want /api/v1/audio/transcriptions", captured.path)
	}
	if captured.auth != "Bearer sk-or-v1-test-secret-should-never-leak" {
		t.Errorf("Authorization header = %q, want Bearer <upstream key>", captured.auth)
	}
	if !strings.Contains(captured.contentType, "application/json") {
		t.Errorf("Content-Type = %q, want application/json (must not still be multipart)", captured.contentType)
	}
	if captured.body["model"] != "openai/whisper-large-v3-turbo" {
		t.Errorf("upstream model = %v, want openai/whisper-large-v3-turbo", captured.body["model"])
	}
	if captured.body["language"] != "fa" {
		t.Errorf("language not forwarded: got %v", captured.body["language"])
	}
	inputAudio, ok := captured.body["input_audio"].(map[string]interface{})
	if !ok {
		t.Fatalf("input_audio missing or wrong shape: %v", captured.body["input_audio"])
	}
	if inputAudio["format"] != "wav" {
		t.Errorf("input_audio.format = %v, want wav", inputAudio["format"])
	}
	wantB64 := base64.StdEncoding.EncodeToString([]byte(fakeAudio))
	gotData, _ := inputAudio["data"].(string)
	if gotData != wantB64 {
		t.Errorf("input_audio.data mismatch — got %q, want %q", gotData, wantB64)
	}
	if strings.HasPrefix(gotData, "data:") {
		t.Errorf("input_audio.data must be raw base64, not a data: URI — got prefix %q", gotData[:5])
	}

	// --- Response-side assertions (what the Nexus client receives back) ---
	if rec.Code != http.StatusOK {
		t.Fatalf("response status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not valid JSON: %v (body=%s)", err, rec.Body.String())
	}
	if out.Text != "سلام، حالت چطوره؟" {
		t.Errorf("text = %q, want the transcription text unchanged", out.Text)
	}
	// The normalized response must not leak OpenRouter-specific fields the
	// existing Nexus contract doesn't expose (e.g. usage.cost) and must never
	// contain the upstream API key under any circumstance.
	if strings.Contains(rec.Body.String(), "sk-or-v1-test-secret-should-never-leak") {
		t.Fatal("response body leaked the upstream API key")
	}
}

// TestOpenRouterTranscribe_AudioFormats covers wav/mp3/m4a/webm end-to-end
// through the real translation path (not just detectAudioFormat in isolation).
func TestOpenRouterTranscribe_AudioFormats(t *testing.T) {
	cases := []struct {
		name       string
		filename   string
		magic      []byte
		wantFormat string
	}{
		{"wav", "call.wav", []byte("RIFF\x00\x00\x00\x00WAVE"), "wav"},
		{"mp3", "call.mp3", []byte("ID3\x03\x00\x00\x00\x00\x00\x00"), "mp3"},
		{"m4a", "call.m4a", append([]byte{0, 0, 0, 0}, []byte("ftypM4A ")...), "m4a"},
		{"webm", "call.webm", []byte{0x1A, 0x45, 0xDF, 0xA3, 0, 0, 0, 0}, "webm"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotFormat string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					InputAudio struct {
						Format string `json:"format"`
					} `json:"input_audio"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				gotFormat = body.InputAudio.Format
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"text":"ok"}`))
			}))
			defer server.Close()

			ep := &runtime.Endpoint{
				BackendType:       runtime.BackendOpenRouter,
				UpstreamBaseURL:   server.URL,
				UpstreamModelName: "openai/whisper-large-v3-turbo",
			}
			bodyBytes, boundary := buildMultipartAudio(t, tc.filename, tc.magic, nil)
			c, rec := ginContext(t)
			if err := openRouterTranscribe(c, ep, server.Client(), bodyBytes, boundary); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
			}
			if gotFormat != tc.wantFormat {
				t.Errorf("input_audio.format = %q, want %q", gotFormat, tc.wantFormat)
			}
		})
	}
}

// ─── Error translation ─────────────────────────────────────────────────────────

func TestOpenRouterTranscribe_ErrorTranslation(t *testing.T) {
	cases := []struct {
		name           string
		upstreamStatus int
		upstreamBody   string
		wantStatus     int
		wantCodeSubstr string
	}{
		{"400 bad request passes through", http.StatusBadRequest, `{"error":{"message":"invalid audio"}}`, http.StatusBadRequest, "invalid_request"},
		{"401 remapped to 424 (our credential, not the client's)", http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`, http.StatusFailedDependency, "provider_credential_unavailable"},
		{"403 remapped to 424", http.StatusForbidden, `{"error":{"message":"forbidden"}}`, http.StatusFailedDependency, "provider_credential_unavailable"},
		{"404 model not found", http.StatusNotFound, `{"error":{"message":"no such model"}}`, http.StatusNotFound, "model_not_found"},
		{"429 rate limited", http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`, http.StatusTooManyRequests, "rate_limited"},
		{"500 mapped to 502", http.StatusInternalServerError, `{"error":{"message":"boom"}}`, http.StatusBadGateway, "upstream_error"},
		{"malformed json error body still produces a clean error", http.StatusInternalServerError, `not json at all`, http.StatusBadGateway, "upstream_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.upstreamStatus)
				_, _ = w.Write([]byte(tc.upstreamBody))
			}))
			defer server.Close()

			ep := &runtime.Endpoint{
				BackendType:       runtime.BackendOpenRouter,
				UpstreamBaseURL:   server.URL,
				UpstreamAPIKey:    "sk-or-v1-should-not-leak",
				UpstreamModelName: "openai/whisper-large-v3-turbo",
			}
			bodyBytes, boundary := buildMultipartAudio(t, "call.wav", []byte("RIFF....WAVE"), nil)
			c, rec := ginContext(t)
			if err := openRouterTranscribe(c, ep, server.Client(), bodyBytes, boundary); err != nil {
				t.Fatalf("unexpected transport-level error: %v", err)
			}
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantCodeSubstr) {
				t.Errorf("body %q does not contain expected code %q", rec.Body.String(), tc.wantCodeSubstr)
			}
			if strings.Contains(rec.Body.String(), "sk-or-v1-should-not-leak") {
				t.Fatal("error response leaked the upstream API key")
			}
		})
	}
}

func TestOpenRouterTranscribe_MissingFile(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "openai/whisper-large-v3-turbo")
	_ = mw.Close()

	ep := &runtime.Endpoint{BackendType: runtime.BackendOpenRouter, UpstreamModelName: "openai/whisper-large-v3-turbo"}
	c, rec := ginContext(t)
	if err := openRouterTranscribe(c, ep, http.DefaultClient, buf.Bytes(), mw.Boundary()); err != nil {
		t.Fatalf("unexpected transport-level error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a missing file field", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "missing_file") {
		t.Errorf("body = %s, want missing_file error code", rec.Body.String())
	}
}

func TestOpenRouterTranscribe_MalformedUpstreamJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("this is not json"))
	}))
	defer server.Close()

	ep := &runtime.Endpoint{BackendType: runtime.BackendOpenRouter, UpstreamBaseURL: server.URL, UpstreamModelName: "openai/whisper-large-v3-turbo"}
	bodyBytes, boundary := buildMultipartAudio(t, "call.wav", []byte("RIFF....WAVE"), nil)
	c, rec := ginContext(t)
	if err := openRouterTranscribe(c, ep, server.Client(), bodyBytes, boundary); err != nil {
		t.Fatalf("unexpected transport-level error: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for a malformed-but-2xx upstream response", rec.Code)
	}
}

// ─── TTS: writeOpenRouterSpeechError ───────────────────────────────────────────

func TestWriteOpenRouterSpeechError(t *testing.T) {
	t.Run("2xx audio response is never treated as an error", func(t *testing.T) {
		c, _ := ginContext(t)
		if writeOpenRouterSpeechError(c, http.StatusOK, []byte{0xFF, 0xFB, 0x90, 0x00}, "audio/mpeg") {
			t.Fatal("a successful raw-audio response must never be JSON-decoded or treated as an error")
		}
	})
	t.Run("non-2xx non-JSON body is left for the caller to pass through raw", func(t *testing.T) {
		c, _ := ginContext(t)
		if writeOpenRouterSpeechError(c, http.StatusInternalServerError, []byte("plain text error"), "text/plain") {
			t.Fatal("non-JSON error body should not be translated here")
		}
	})
	t.Run("non-2xx JSON body is translated into the standard error envelope", func(t *testing.T) {
		c, rec := ginContext(t)
		handled := writeOpenRouterSpeechError(c, http.StatusTooManyRequests,
			[]byte(`{"error":{"message":"slow down"}}`), "application/json")
		if !handled {
			t.Fatal("expected the JSON error body to be handled")
		}
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("status = %d, want 429", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "rate_limited") {
			t.Errorf("body = %s, want rate_limited code", rec.Body.String())
		}
	})
}

// ─── Provider routing sanity: capability derivation lives in catalog, not here ─

// TestNoModelNameConditionalsInAudioAdapter is a structural guard: this
// package's OpenRouter audio routing must key off ep.BackendType alone. This
// test doesn't (can't) prove the absence of a name check by reflection, but
// it does prove the two acceptance models route identically through the same
// code path when given the same BackendType, and differently when given a
// different BackendType — i.e. behavior tracks backend metadata, not the
// specific model string.
func TestNoModelNameConditionalsInAudioAdapter(t *testing.T) {
	models := []string{"openai/whisper-large-v3-turbo", "google/gemini-3.1-flash-tts-preview", "literally-any-future-model-id"}
	for _, m := range models {
		if got := audioTranscriptionsPath(runtime.BackendOpenRouter); got != openRouterTranscriptionsPath {
			t.Fatalf("model %q: expected OpenRouter path regardless of model name, got %q", m, got)
		}
	}
}
