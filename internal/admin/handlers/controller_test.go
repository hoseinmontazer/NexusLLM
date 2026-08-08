package handlers

import (
	"reflect"
	"testing"
)

func TestBuildStartPayload_LazyConfigPropagation(t *testing.T) {
	row := &runtimeRow{
		RuntimeID:      "rt-123",
		EndpointID:     "ep-123",
		ModelID:        "m-123",
		RuntimeName:    "nexus-whisper",
		Backend:        "openai_compat",
		Image:          "fedora/faster-whisper-server:latest",
		ModelName:      "whisper-large-v3",
		BindHost:       "127.0.0.1",
		BindPort:       8100,
		GPUDevicesJSON: "[0,1]",
		TensorParallel: 1,
		GPUMemoryUtil:  0.9,
		MaxModelLen:    4096,
		Dtype:          "float16",
		Quantization:   "int8",
		ExecutionMode:  "cpu",
		GGUFPath:       "",
		HFRepo:         "Systran/faster-whisper-large-v3",
		HFFile:         "",
		CtxSize:        4096,
		NGPULayers:     0,
		MemoryLimit:    "8g",
		ModelsVolume:   "whisper_models",
		ExtraArgsJSON:  `["--language","en"]`,
		EnvJSON:        `{"WHISPER__MODEL":"Systran/faster-whisper-large-v3","UVICORN_PORT":"8100"}`,
	}

	payload := buildStartPayload("rt-123", row)

	if payload.ModelsVolume != "whisper_models" {
		t.Errorf("expected ModelsVolume whisper_models, got %s", payload.ModelsVolume)
	}

	expectedEnv := map[string]string{
		"WHISPER__MODEL": "Systran/faster-whisper-large-v3",
		"UVICORN_PORT":   "8100",
	}
	if !reflect.DeepEqual(payload.Env, expectedEnv) {
		t.Errorf("expected Env %v, got %v", expectedEnv, payload.Env)
	}

	expectedExtraArgs := []string{"--language", "en"}
	if !reflect.DeepEqual(payload.ExtraArgs, expectedExtraArgs) {
		t.Errorf("expected ExtraArgs %v, got %v", expectedExtraArgs, payload.ExtraArgs)
	}

	expectedGPUDevices := []int{0, 1}
	if !reflect.DeepEqual(payload.GPUDevices, expectedGPUDevices) {
		t.Errorf("expected GPUDevices %v, got %v", expectedGPUDevices, payload.GPUDevices)
	}
}

func TestMultiCycleDynamicPortAllocation(t *testing.T) {
	// Simulate 3 consecutive start/stop/restart cycles with different dynamic ports
	ports := []int{32001, 32002, 32003}

	for cycle, port := range ports {
		row := &runtimeRow{
			RuntimeID:   "rt-cycle",
			EndpointID:  "ep-cycle",
			ModelID:     "m-cycle",
			RuntimeName: "nexus-stt",
			Backend:     "openai_compat",
			Image:       "fedora/faster-whisper-server:latest",
			ModelName:   "whisper-large-v3",
			BindHost:    "127.0.0.1",
			BindPort:    port,
			EnvJSON:     `{"WHISPER__MODEL":"large-v3"}`,
		}

		payload := buildStartPayload("rt-cycle", row)

		if payload.BindPort != port {
			t.Errorf("Cycle %d: expected BindPort %d, got %d", cycle+1, port, payload.BindPort)
		}
		if payload.Env["WHISPER__MODEL"] != "large-v3" {
			t.Errorf("Cycle %d: expected WHISPER__MODEL 'large-v3', got %s", cycle+1, payload.Env["WHISPER__MODEL"])
		}
	}
}

func TestOriginalBugReproduction_STTLazyRestart(t *testing.T) {
	// Step 1: Initial deploy row with port 8000
	deployRow := &runtimeRow{
		RuntimeID:   "rt-1",
		EndpointID:  "ep-stt",
		ModelID:     "m-stt",
		RuntimeName: "nexus-whisper",
		Backend:     "openai_compat",
		Image:       "fedora/faster-whisper-server:latest",
		ModelName:   "whisper-large-v3",
		BindHost:    "127.0.0.1",
		BindPort:    8000,
		EnvJSON:     `{"WHISPER__MODEL":"Systran/faster-whisper-large-v3"}`,
	}
	p1 := buildStartPayload("rt-1", deployRow)
	if p1.BindPort != 8000 {
		t.Errorf("Initial deploy: expected BindPort 8000, got %d", p1.BindPort)
	}

	// Step 2: Unload / Cold restart on new allocated port 32781
	coldRestartRow := &runtimeRow{
		RuntimeID:   "rt-2",
		EndpointID:  "ep-stt",
		ModelID:     "m-stt",
		RuntimeName: "nexus-whisper",
		Backend:     "openai_compat",
		Image:       "fedora/faster-whisper-server:latest",
		ModelName:   "whisper-large-v3",
		BindHost:    "127.0.0.1",
		BindPort:    32781,
		EnvJSON:     `{"WHISPER__MODEL":"Systran/faster-whisper-large-v3"}`,
	}
	p2 := buildStartPayload("rt-2", coldRestartRow)

	if p2.BindPort != 32781 {
		t.Errorf("Cold restart: expected BindPort 32781, got %d", p2.BindPort)
	}
	if p2.Env["WHISPER__MODEL"] != "Systran/faster-whisper-large-v3" {
		t.Errorf("Cold restart: expected WHISPER__MODEL preserved, got %s", p2.Env["WHISPER__MODEL"])
	}
}

func TestEmbeddingAndLLMProviderNonRegression(t *testing.T) {
	providers := []struct {
		name       string
		backend    string
		image      string
		ggufPath   string
		hfRepo     string
		hfFile     string
		nGPULayers int
	}{
		{"llamacpp", "llamacpp", "ghcr.io/ggerganov/llama.cpp:server", "/models/gemma-2b.gguf", "", "", 33},
		{"vllm", "vllm", "vllm/vllm-openai:latest", "", "meta-llama/Llama-2-7b-hf", "", -1},
		{"tgi", "tgi", "ghcr.io/huggingface/text-generation-inference:latest", "", "tiiuae/falcon-7b", "", -1},
		{"tei", "tei", "ghcr.io/huggingface/text-embeddings-inference:cpu-latest", "", "BAAI/bge-large-en-v1.5", "", 0},
		{"openai_compat", "openai_compat", "openai/api:latest", "", "", "", 0},
	}

	for _, p := range providers {
		row := &runtimeRow{
			RuntimeID:   "rt-" + p.name,
			EndpointID:  "ep-" + p.name,
			ModelID:     "m-" + p.name,
			RuntimeName: "nexus-" + p.name,
			Backend:     p.backend,
			Image:       p.image,
			ModelName:   p.name,
			BindHost:    "127.0.0.1",
			BindPort:    8080,
			GGUFPath:    p.ggufPath,
			HFRepo:      p.hfRepo,
			HFFile:      p.hfFile,
			NGPULayers:  p.nGPULayers,
		}

		payload := buildStartPayload("rt-"+p.name, row)
		if payload.Backend != p.backend {
			t.Errorf("Provider %s: expected Backend %s, got %s", p.name, p.backend, payload.Backend)
		}
		if payload.GGUFPath != p.ggufPath {
			t.Errorf("Provider %s: expected GGUFPath %s, got %s", p.name, p.ggufPath, payload.GGUFPath)
		}
		if payload.HFRepo != p.hfRepo {
			t.Errorf("Provider %s: expected HFRepo %s, got %s", p.name, p.hfRepo, payload.HFRepo)
		}
	}
}
