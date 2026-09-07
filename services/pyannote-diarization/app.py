"""Pyannote speaker-diarization wrapper.

Serves a pyannote.audio diarization pipeline behind a FastAPI HTTP API that
mimics the NexusLLM STT contract (POST /v1/audio/transcriptions) closely
enough to ride through NexusLLM's existing byte-for-byte STT proxy path with
zero Go changes (see internal/proxy/multiservice.go Transcriptions/forwardRaw).

Deployment is entirely manual: this process does not know about NexusLLM at
all, and is meant to be registered with deployment_mode=manual so NexusLLM
only routes to it and health-checks it, never starts/stops/recreates it.

Scaling: this process loads exactly one pipeline instance and serializes
inference through a single lock (pyannote pipelines are not verified safe for
concurrent inference on one instance, and loading a second copy to parallelize
would double GPU/CPU memory for no throughput guarantee). To scale, run more
replicas of this container (more ports, each registered as its own NexusLLM
model) rather than adding uvicorn workers or threads here.
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

PYANNOTE_MODEL = os.environ.get("PYANNOTE_MODEL", "pyannote/speaker-diarization-3.1")
DEVICE = os.environ.get("DEVICE", "cpu")
HF_TOKEN = os.environ.get("HF_TOKEN", "")

# A small allowlist of extensions we'll preserve on the temp file so the audio
# backend (ffmpeg/soundfile) gets a format hint. Never derived from arbitrary
# user input beyond this lookup — see diarize() below.
_ALLOWED_SUFFIXES = {".wav", ".mp3", ".flac", ".ogg", ".m4a", ".webm", ".opus"}

_inference_lock = threading.Lock()


def _redact(exc: Exception) -> str:
    """Stringify an exception, stripping HF_TOKEN if it ever leaked into a message."""
    msg = str(exc)
    if HF_TOKEN:
        msg = msg.replace(HF_TOKEN, "***")
    return msg


def _load_pipeline():
    """Load the pyannote pipeline, failing loudly and non-secretly on error.

    Runs at import time (before uvicorn starts accepting connections) so the
    container can never answer /health before the pipeline is actually ready
    — if this raises, the process exits non-zero and nothing ever listens.
    """
    if not HF_TOKEN:
        print(
            "FATAL: HF_TOKEN is not set. Both pyannote/speaker-diarization-3.1 "
            "and pyannote/speaker-diarization-community-1 are gated Hugging Face "
            "repos — accept the model's terms on huggingface.co and pass a read "
            "token as HF_TOKEN.",
            file=sys.stderr,
        )
        sys.exit(1)

    import torch
    from pyannote.audio import Pipeline

    if DEVICE == "cuda" and not torch.cuda.is_available():
        print(
            "FATAL: DEVICE=cuda was requested but torch.cuda.is_available() is "
            "False in this container (no GPU visible, missing --gpus flag, or "
            "no NVIDIA Container Toolkit on the host).",
            file=sys.stderr,
        )
        sys.exit(1)

    try:
        try:
            # pyannote.audio >= 4.0 renamed use_auth_token -> token.
            pipeline = Pipeline.from_pretrained(PYANNOTE_MODEL, token=HF_TOKEN)
        except TypeError:
            pipeline = Pipeline.from_pretrained(PYANNOTE_MODEL, use_auth_token=HF_TOKEN)
    except Exception as exc:  # noqa: BLE001 - intentionally broad, re-raised below
        print(
            f"FATAL: failed to load pipeline '{PYANNOTE_MODEL}' on device "
            f"'{DEVICE}': {_redact(exc)}. Common causes: HF_TOKEN is invalid/expired, "
            f"the gated model's terms were not accepted on huggingface.co, or an "
            f"incompatible pyannote.audio/torch version is installed.",
            file=sys.stderr,
        )
        sys.exit(1)

    if pipeline is None:
        print(f"FATAL: Pipeline.from_pretrained('{PYANNOTE_MODEL}') returned None.", file=sys.stderr)
        sys.exit(1)

    pipeline.to(torch.device(DEVICE))
    return pipeline, torch


_pipeline, _torch = _load_pipeline()
_ready = True  # only reachable after the load above succeeded — see _load_pipeline docstring

app = FastAPI()


@app.get("/health")
def health():
    if not _ready or _pipeline is None:
        raise HTTPException(status_code=503, detail="pipeline not ready")
    return {
        "status": "ok",
        "model": PYANNOTE_MODEL,
        "device": DEVICE,
        "cuda_available": _torch.cuda.is_available(),
        "pyannote_audio_version": _pyannote_audio_version(),
        "torch_version": _torch.__version__,
    }


def _pyannote_audio_version() -> str:
    try:
        import pyannote.audio as pa

        return getattr(pa, "__version__", "unknown")
    except Exception:  # noqa: BLE001
        return "unknown"


@app.post("/v1/audio/transcriptions")
async def diarize(file: UploadFile, model: str = Form(None)):
    if file is None:
        raise HTTPException(status_code=400, detail="Field 'file' is required")

    # Never build a path from the client-supplied filename. Only its
    # extension (checked against an allowlist) is reused, purely so the audio
    # backend gets a format hint; the actual path is generated by tempfile.
    _, ext = os.path.splitext(file.filename or "")
    suffix = ext.lower() if ext.lower() in _ALLOWED_SUFFIXES else ".wav"

    tmp_path = None
    wav_path = None
    try:
        data = await file.read()
        if not data:
            raise HTTPException(status_code=400, detail="Uploaded file is empty")

        fd, tmp_path = tempfile.mkstemp(suffix=suffix, prefix="pyannote-")
        with os.fdopen(fd, "wb") as f:
            f.write(data)

        # Decode with our own ffmpeg call rather than handing pyannote the raw
        # path. pyannote.audio >= 4.0 dropped its soundfile/sox decode paths —
        # its only remaining file-path route goes through torchcodec, which
        # (verified against the actual GPU image) fails to load because the
        # pytorch/pytorch *-runtime base image lacks libnvrtc.so — a static
        # missing-library problem, not a "no GPU visible" one, so it would
        # recur on real GPU hardware too. Decoding to a normalized 16kHz mono
        # WAV ourselves and handing pyannote the in-memory waveform instead is
        # pyannote's other officially-supported input mode, and sidesteps
        # torchcodec entirely on both the CPU (3.x) and GPU (4.x) pipelines.
        wav_fd, wav_path = tempfile.mkstemp(suffix=".wav", prefix="pyannote-pcm-")
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

        waveform_np, sample_rate = sf.read(wav_path, dtype="float32", always_2d=True)
        waveform = _torch.from_numpy(waveform_np.T)  # soundfile: (frames, channels) -> pyannote: (channel, time)

        try:
            with _inference_lock:
                result = _pipeline({"waveform": waveform, "sample_rate": sample_rate})
        except Exception as exc:  # noqa: BLE001
            raise HTTPException(
                status_code=502,
                detail=f"diarization inference failed: {_redact(exc)}",
            ) from exc

        # pyannote.audio >= 4.0 returns a DiarizeOutput wrapper; the Annotation
        # (itertracks) lives at .speaker_diarization. pyannote.audio 3.x
        # returns the Annotation directly. Handle both without caring which
        # version is installed.
        annotation = getattr(result, "speaker_diarization", result)

        segments = [
            {"start": round(turn.start, 3), "end": round(turn.end, 3), "speaker": speaker}
            for turn, _, speaker in annotation.itertracks(yield_label=True)
        ]
        return JSONResponse({"model": PYANNOTE_MODEL, "segments": segments})
    finally:
        for p in (tmp_path, wav_path):
            if p is not None:
                with suppress(OSError):
                    os.remove(p)
