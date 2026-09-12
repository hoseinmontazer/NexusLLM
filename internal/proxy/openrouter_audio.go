package proxy

// openrouter_audio.go — generic OpenRouter STT/TTS wire-format translation.
//
// OpenRouter's /api/v1/audio/transcriptions and /api/v1/audio/speech
// endpoints do not speak NexusLLM's OpenAI-compatible multipart/JSON wire
// format (transcriptions: JSON + base64 audio, not multipart; both endpoints
// live under /api/v1/... not /v1/...). This file is the ONLY place that
// knows that difference — Transcriptions()/Speech() in multiservice.go branch
// into it purely on ep.BackendType == runtime.BackendOpenRouter, never on a
// model name, so any current or future OpenRouter STT/TTS model works
// automatically the moment the catalog marks it with the transcription/speech
// capability (internal/catalog/resolver.go's capabilitiesFromVirtualEndpoint).
//
// Verified against OpenRouter's own documented request/response shape
// (openrouter.ai/docs/guides/overview/multimodal/stt and the TTS endpoint
// description), not assumed.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/nexusllm/nexusllm/internal/runtime"
)

const (
	openRouterTranscriptionsPath = "/api/v1/audio/transcriptions"
	openRouterSpeechPath         = "/api/v1/audio/speech"
)

// audioTranscriptionsPath and audioSpeechPath return the correct upstream
// path suffix for ep's backend type. Every non-OpenRouter backend (native STT/
// TTS servers, our own cpu_native wrappers) keeps the existing /v1/audio/...
// path unchanged.
func audioTranscriptionsPath(bt runtime.BackendType) string {
	if bt == runtime.BackendOpenRouter {
		return openRouterTranscriptionsPath
	}
	return "/v1/audio/transcriptions"
}

func audioSpeechPath(bt runtime.BackendType) string {
	if bt == runtime.BackendOpenRouter {
		return openRouterSpeechPath
	}
	return "/v1/audio/speech"
}

// detectAudioFormat sniffs common audio container magic bytes and only falls
// back to the uploaded filename's extension when sniffing is inconclusive —
// per the spec, the extension alone is never trusted (a client can rename a
// file, or omit a filename entirely).
func detectAudioFormat(data []byte, filename string) string {
	switch {
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WAVE":
		return "wav"
	case len(data) >= 4 && string(data[0:4]) == "fLaC":
		return "flac"
	case len(data) >= 4 && string(data[0:4]) == "OggS":
		return "ogg"
	case len(data) >= 4 && data[0] == 0x1A && data[1] == 0x45 && data[2] == 0xDF && data[3] == 0xA3:
		return "webm" // EBML magic — webm is a Matroska/EBML container
	case len(data) >= 8 && string(data[4:8]) == "ftyp":
		return "m4a"
	case len(data) >= 3 && string(data[0:3]) == "ID3":
		return "mp3"
	}
	// MPEG frame sync (0xFF Ex/Fx) covers both raw MP3 frames and ADTS AAC —
	// genuinely ambiguous from the magic bytes alone. Break the tie with the
	// extension only for this pair; every other format above was sniffed.
	if len(data) >= 2 && data[0] == 0xFF && (data[1]&0xE0) == 0xE0 {
		if strings.EqualFold(strings.TrimPrefix(filepath.Ext(filename), "."), "aac") {
			return "aac"
		}
		return "mp3"
	}
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), ".")) {
	case "wav", "mp3", "flac", "m4a", "ogg", "webm", "aac":
		return strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
	}
	return "wav"
}

// openRouterTranscriptionRequest is OpenRouter's documented STT request body.
// input_audio.data is raw base64 — deliberately NOT a data: URI.
type openRouterTranscriptionRequest struct {
	Model      string               `json:"model"`
	InputAudio openRouterInputAudio `json:"input_audio"`
	Language   string               `json:"language,omitempty"`
	Prompt     string               `json:"prompt,omitempty"`
	RespFormat string               `json:"response_format,omitempty"`
}

type openRouterInputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

type openRouterTranscriptionResponse struct {
	Text  string `json:"text"`
	Usage *struct {
		Seconds      float64 `json:"seconds"`
		TotalTokens  int     `json:"total_tokens"`
		InputTokens  int     `json:"input_tokens"`
		OutputTokens int     `json:"output_tokens"`
		Cost         float64 `json:"cost"`
	} `json:"usage,omitempty"`
}

// openRouterTranscribe implements the generic STT adapter: parse the
// client's multipart upload, translate it into OpenRouter's JSON+base64 wire
// format, execute the request against ep's configured base URL, and
// normalize the response back into NexusLLM's existing {"text": "..."}
// transcription contract.
//
// bodyBytes is the already-buffered original multipart request body (the
// caller re-parses it here rather than relying on Transcriptions()'s partial
// model-field-only scan). boundary is the multipart boundary extracted from
// the Content-Type header. client is the per-endpoint HTTP client to use
// (normally h.registry.ClientForEndpoint(ep)) — taken as a parameter rather
// than a Handler field so this function has no dependency on Registry/DB and
// is directly unit-testable against an httptest.Server.
//
// Returns an error only for genuine transport/network failures — the caller
// (Transcriptions) turns that into a 502 upstream_error, matching forwardRaw's
// existing contract. All translatable failures (bad request, malformed
// upstream JSON, etc.) write the response directly and return nil.
func openRouterTranscribe(c *gin.Context, ep *runtime.Endpoint, client *http.Client, bodyBytes []byte, boundary string) error {
	if boundary == "" {
		abortErr(c, http.StatusBadRequest, "invalid_request", "missing multipart boundary")
		return nil
	}

	var audioData []byte
	var filename, language, prompt, responseFormat string
	mr := multipart.NewReader(bytes.NewReader(bodyBytes), boundary)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			abortErr(c, http.StatusBadRequest, "invalid_request", "malformed multipart body: "+err.Error())
			return nil
		}
		switch part.FormName() {
		case "file":
			filename = part.FileName()
			data, rerr := io.ReadAll(part)
			part.Close()
			if rerr != nil {
				abortErr(c, http.StatusBadRequest, "invalid_request", "failed to read uploaded audio: "+rerr.Error())
				return nil
			}
			audioData = data
			continue
		case "language":
			v, _ := io.ReadAll(part)
			language = strings.TrimSpace(string(v))
		case "prompt":
			v, _ := io.ReadAll(part)
			prompt = strings.TrimSpace(string(v))
		case "response_format":
			v, _ := io.ReadAll(part)
			responseFormat = strings.TrimSpace(string(v))
		}
		part.Close()
	}
	if len(audioData) == 0 {
		abortErr(c, http.StatusBadRequest, "missing_file", "Field 'file' is required")
		return nil
	}

	orReq := openRouterTranscriptionRequest{
		Model: ep.UpstreamModelName,
		InputAudio: openRouterInputAudio{
			Data:   base64.StdEncoding.EncodeToString(audioData),
			Format: detectAudioFormat(audioData, filename),
		},
		Language:   language,
		Prompt:     prompt,
		RespFormat: responseFormat,
	}
	jsonBody, err := json.Marshal(orReq)
	if err != nil {
		return fmt.Errorf("marshal openrouter transcription request: %w", err)
	}

	url := epEffectiveURL(ep) + audioTranscriptionsPath(ep.BackendType)
	httpReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("build openrouter transcription request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if ep.UpstreamAPIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+ep.UpstreamAPIKey)
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("openrouter transcription request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read openrouter transcription response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeOpenRouterError(c, resp.StatusCode, respBody)
		return nil
	}

	var orResp openRouterTranscriptionResponse
	if jsonErr := json.Unmarshal(respBody, &orResp); jsonErr != nil {
		abortErr(c, http.StatusBadGateway, "upstream_invalid_response",
			"OpenRouter returned a malformed transcription response")
		return nil
	}
	c.JSON(http.StatusOK, gin.H{"text": orResp.Text})
	return nil
}

// writeOpenRouterError translates an OpenRouter error response into
// NexusLLM's existing abortErr envelope. It preserves the real upstream
// status/message instead of collapsing every failure into a generic 500, but
// remaps 401/403 to 424 Failed Dependency — those reflect NexusLLM's own
// configured OpenRouter credential being rejected, an operator-side problem,
// not the caller's NexusLLM API key, which would be misleading to surface as
// a plain 401/403 to the client. Never echoes upstream headers or the API key.
func writeOpenRouterError(c *gin.Context, upstreamStatus int, body []byte) {
	var envelope struct {
		Error struct {
			Message string      `json:"message"`
			Code    interface{} `json:"code"`
			Type    string      `json:"type"`
		} `json:"error"`
	}
	msg := fmt.Sprintf("openrouter upstream returned HTTP %d", upstreamStatus)
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
		msg = envelope.Error.Message
	}

	status := upstreamStatus
	code := "upstream_error"
	switch upstreamStatus {
	case http.StatusBadRequest:
		code = "invalid_request"
	case http.StatusUnauthorized, http.StatusForbidden:
		status = http.StatusFailedDependency
		code = "provider_credential_unavailable"
		msg = "NexusLLM's configured OpenRouter credential was rejected: " + msg
	case http.StatusNotFound:
		code = "model_not_found"
	case http.StatusTooManyRequests:
		code = "rate_limited"
	default:
		if upstreamStatus >= 500 {
			status = http.StatusBadGateway
			code = "upstream_error"
		}
	}
	abortErr(c, status, code, msg)
}

// openRouterSpeak implements the generic TTS adapter. The Nexus request body
// (models.SpeechRequest: model/input/voice/response_format/speed) is already
// wire-compatible with OpenRouter's TTS endpoint — the only translation
// needed is the /api/v1/audio/speech path (handled by audioSpeechPath at the
// call site in Speech()) and normalizing a non-2xx response, since a failed
// request returns JSON while a successful one returns raw audio bytes and
// must never be JSON-decoded.
func writeOpenRouterSpeechError(c *gin.Context, upstreamStatus int, body []byte, contentType string) bool {
	if upstreamStatus >= 200 && upstreamStatus < 300 {
		return false
	}
	if !strings.Contains(contentType, "json") {
		// Not a JSON error body — nothing to translate; let the caller pass
		// the raw upstream status/body/content-type through unchanged.
		return false
	}
	writeOpenRouterError(c, upstreamStatus, body)
	return true
}
