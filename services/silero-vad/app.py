"""Silero VAD (voice activity detection) wrapper.

Serves runanywhere/silero-vad-v5's actual weights via the `silero-vad-notorch`
PyPI package rather than fetching the HF repo directly. That HF repo packages
the same v5 ONNX weights specifically for the RunAnywhere mobile/cross-language
SDK, which has no plain-Python entry point; silero-vad-notorch is an
independent community fork of `silero-vad` (NOT an official Silero/snakers4
release — verified via `pip show`) with the torch/torchaudio dependency
removed, running purely on onnxruntime. Same v5 model, dramatically smaller
image, no GPU need (Silero's own docs: <1ms per 30ms+ chunk on a single CPU
thread). Its top-level module is `silero_vad_notorch`, not `silero_vad` —
confirmed by inspecting the installed package, despite READMEs describing it
as a drop-in `from silero_vad import ...` replacement.

Unlike the pyannote services, this one is NOT gated — no HF_TOKEN, no
network fetch at startup at all (the ONNX weights ship inside the pip
package), so there is no auth-failure startup mode to guard against.

Serves the same NexusLLM byte-for-byte STT-proxy trick (POST
/v1/audio/transcriptions) as services/pyannote-diarization, but the response
has no "speaker" field — VAD detects presence of speech, not who is speaking.
"""

import os
import subprocess
import sys
import tempfile
import threading
from contextlib import suppress

import soundfile as sf
from fastapi import FastAPI, Form, HTTPException, UploadFile
from fastapi.responses import JSONResponse

VAD_MODEL = os.environ.get("VAD_MODEL", "runanywhere/silero-vad-v5")

_ALLOWED_SUFFIXES = {".wav", ".mp3", ".flac", ".ogg", ".m4a", ".webm", ".opus"}
_inference_lock = threading.Lock()


def _load_model():
    try:
        from silero_vad_notorch import get_speech_timestamps, load_silero_vad
    except Exception as exc:  # noqa: BLE001
        print(f"FATAL: failed to import silero_vad_notorch: {exc}", file=sys.stderr)
        sys.exit(1)

    try:
        model = load_silero_vad()
    except Exception as exc:  # noqa: BLE001
        print(f"FATAL: failed to load the Silero VAD model: {exc}", file=sys.stderr)
        sys.exit(1)

    return model, get_speech_timestamps


_model, _get_speech_timestamps = _load_model()
_ready = True

app = FastAPI()


@app.get("/health")
def health():
    if not _ready or _model is None:
        raise HTTPException(status_code=503, detail="model not ready")
    return {"status": "ok", "model": VAD_MODEL, "device": "cpu"}


@app.post("/v1/audio/transcriptions")
async def detect_speech(file: UploadFile, model: str = Form(None)):
    if file is None:
        raise HTTPException(status_code=400, detail="Field 'file' is required")

    _, ext = os.path.splitext(file.filename or "")
    suffix = ext.lower() if ext.lower() in _ALLOWED_SUFFIXES else ".wav"

    tmp_path = None
    wav_path = None
    try:
        data = await file.read()
        if not data:
            raise HTTPException(status_code=400, detail="Uploaded file is empty")

        fd, tmp_path = tempfile.mkstemp(suffix=suffix, prefix="vad-")
        with os.fdopen(fd, "wb") as f:
            f.write(data)

        # Normalize to 16kHz mono via our own ffmpeg call — Silero VAD only
        # accepts exactly 8kHz or 16kHz input, same normalization approach
        # used in services/pyannote-diarization/app.py.
        wav_fd, wav_path = tempfile.mkstemp(suffix=".wav", prefix="vad-pcm-")
        os.close(wav_fd)
        ffmpeg_proc = subprocess.run(
            ["ffmpeg", "-y", "-i", tmp_path, "-ar", "16000", "-ac", "1", "-f", "wav", wav_path],
            capture_output=True,
        )
        if ffmpeg_proc.returncode != 0:
            raise HTTPException(
                status_code=400,
                detail="Could not decode uploaded audio (unsupported or corrupt file): "
                + ffmpeg_proc.stderr.decode("utf-8", errors="replace")[-500:],
            )

        waveform, sample_rate = sf.read(wav_path, dtype="float32", always_2d=False)

        try:
            with _inference_lock:
                timestamps = _get_speech_timestamps(
                    waveform, _model, sampling_rate=sample_rate, return_seconds=True
                )
        except Exception as exc:  # noqa: BLE001
            raise HTTPException(status_code=502, detail=f"VAD inference failed: {exc}") from exc

        segments = [{"start": round(t["start"], 3), "end": round(t["end"], 3)} for t in timestamps]
        return JSONResponse({"model": VAD_MODEL, "segments": segments})
    finally:
        for p in (tmp_path, wav_path):
            if p is not None:
                with suppress(OSError):
                    os.remove(p)
