package catalog

// syncer_openrouter_audio_test.go — regression test for the actual root
// cause of "OpenRouter STT/TTS model never appears in the catalog": verified
// live against the real OpenRouter API that its default /api/v1/models
// listing does NOT include audio-only models (STT/TTS) at all — they only
// appear behind the output_modalities=transcription / output_modalities=speech
// filters. fetchModels must fetch and merge those in for OpenRouter, or no
// such model — present or future — could ever be discovered, no matter how
// correct the capability-derivation parsing is.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func TestFetchModels_OpenRouterMergesModalityFilteredEndpoints(t *testing.T) {
	var gotPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("output_modalities") {
		case "transcription":
			_, _ = w.Write([]byte(`{"data":[{"id":"openai/whisper-large-v3-turbo","name":"Whisper Large v3 Turbo",
				"architecture":{"modality":"audio->transcription","input_modalities":["audio"],"output_modalities":["transcription"]}}]}`))
		case "speech":
			_, _ = w.Write([]byte(`{"data":[{"id":"google/gemini-3.1-flash-tts-preview","name":"Gemini 3.1 Flash TTS",
				"architecture":{"modality":"text->speech","input_modalities":["text"],"output_modalities":["speech"]}}]}`))
		default:
			// The base /api/v1/models listing — deliberately does NOT include
			// either audio model, matching the real, verified OpenRouter behavior.
			_, _ = w.Write([]byte(`{"data":[{"id":"openai/gpt-5.6-luna","name":"GPT-5.6 Luna",
				"architecture":{"modality":"text->text","input_modalities":["text"],"output_modalities":["text"]}}]}`))
		}
	}))
	defer server.Close()

	s := &CatalogSyncer{log: zap.NewNop()}
	p := &Provider{ID: "prov-1", Name: "openrouter", BackendType: "openrouter_provider", BaseURL: server.URL}

	models, err := s.fetchModels(context.Background(), p, server.Client())
	if err != nil {
		t.Fatalf("fetchModels returned unexpected error: %v", err)
	}

	byID := make(map[string]RemoteModel, len(models))
	for _, m := range models {
		byID[m.ProviderModelID] = m
	}

	if _, ok := byID["openai/gpt-5.6-luna"]; !ok {
		t.Error("base catalog listing model missing from merged results")
	}

	whisper, ok := byID["openai/whisper-large-v3-turbo"]
	if !ok {
		t.Fatal("openai/whisper-large-v3-turbo missing — the modality-filtered fetch was not merged in")
	}
	if !whisper.SupportsAudio {
		t.Error("whisper: SupportsAudio should be true (input_modalities contains audio)")
	}
	if whisper.SupportsSpeech {
		t.Error("whisper: SupportsSpeech should be false (its output modality is transcription, not speech)")
	}

	tts, ok := byID["google/gemini-3.1-flash-tts-preview"]
	if !ok {
		t.Fatal("google/gemini-3.1-flash-tts-preview missing — the modality-filtered fetch was not merged in")
	}
	if !tts.SupportsSpeech {
		t.Error("gemini tts: SupportsSpeech should be true (output_modalities contains speech)")
	}
	if tts.SupportsAudio {
		t.Error("gemini tts: SupportsAudio should be false (its input modality is text, not audio)")
	}

	if len(gotPaths) != 3 {
		t.Errorf("expected exactly 3 upstream fetches (base + 2 modality filters), got %d: %v", len(gotPaths), gotPaths)
	}
}

func TestFetchModels_NonOpenRouterProviderSkipsModalityFilters(t *testing.T) {
	var gotPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"some-model","name":"Some Model"}]}`))
	}))
	defer server.Close()

	s := &CatalogSyncer{log: zap.NewNop()}
	// Any non-OpenRouter backend — the modality-filter fetch is OpenRouter-
	// specific and must not fire for other providers.
	p := &Provider{ID: "prov-2", Name: "groq", BackendType: "groq_provider", BaseURL: server.URL}

	if _, err := s.fetchModels(context.Background(), p, server.Client()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotPaths) != 1 {
		t.Errorf("expected exactly 1 fetch for a non-OpenRouter provider, got %d: %v", len(gotPaths), gotPaths)
	}
}

func TestFetchModels_ModalityFilterFailureIsNonFatal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("output_modalities") != "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"openai/gpt-5.6-luna","name":"GPT-5.6 Luna"}]}`))
	}))
	defer server.Close()

	s := &CatalogSyncer{log: zap.NewNop()}
	p := &Provider{ID: "prov-3", Name: "openrouter", BackendType: "openrouter_provider", BaseURL: server.URL}

	models, err := s.fetchModels(context.Background(), p, server.Client())
	if err != nil {
		t.Fatalf("a failing modality-filtered fetch must not fail the whole sync: %v", err)
	}
	if len(models) != 1 || models[0].ProviderModelID != "openai/gpt-5.6-luna" {
		t.Errorf("expected the base listing to still succeed, got %+v", models)
	}
}
