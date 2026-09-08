#!/bin/sh
# See services/pyannote-diarization/entrypoint.sh for why this exists:
# NexusLLM's deploy code auto-appends "--port <N>" as a CMD arg for images
# not on its hardcoded allowlist. Using this as ENTRYPOINT (not CMD) means
# any such appended arg lands in "$@" here and is ignored, instead of
# replacing the command and crash-looping the container.
exec uvicorn app:app --host 0.0.0.0 --port "${UVICORN_PORT:-8000}" --workers 1
