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
