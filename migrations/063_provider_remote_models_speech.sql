-- NexusLLM Migration 063 — supports_speech capability flag
--
-- Problem: provider_remote_models tracks input-modality capabilities
-- (supports_audio for audio INPUT, i.e. STT models like Whisper) but has no
-- flag for audio OUTPUT (TTS models like Gemini's speech-preview models).
-- internal/catalog/syncer.go's architecture-block parser only ever checked
-- OutputModalities for "image" (image generation) and "embedding" — never
-- "audio" — so a virtual-catalog TTS model had no way to be tagged with the
-- CapabilitySpeech capability at all, regardless of what OpenRouter's own
-- /api/v1/models response reported.
--
-- Fix: add supports_speech, populated from architecture.output_modalities
-- containing "audio", mirroring how supports_audio is populated from
-- input_modalities containing "audio". See internal/catalog/resolver.go's
-- capabilitiesFromVirtualEndpoint for the capability derivation this feeds.

ALTER TABLE provider_remote_models
    ADD COLUMN IF NOT EXISTS supports_speech BOOLEAN NOT NULL DEFAULT FALSE;
