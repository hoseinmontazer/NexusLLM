#!/bin/sh
# NexusLLM's deploy code (internal/controller/docker_driver.go,
# internal/nodeagent/executor.go) auto-appends "--port <N>" as a CMD argument
# to any image whose name isn't on its hardcoded env-var-port allowlist
# (faster-whisper, whisper-server, kokoro) — ours isn't. `docker run <image>
# ARG...` replaces CMD entirely, so if this were a bare CMD, that appended arg
# would silently take over and crash-loop the container instead of running
# uvicorn at all.
#
# Using this script as ENTRYPOINT instead of CMD sidesteps that: whatever
# NexusLLM (or anyone else) appends after the image name arrives here as "$@"
# and is deliberately ignored — this always launches uvicorn on
# ${UVICORN_PORT:-8000}, regardless of what's appended.
exec uvicorn app:app --host 0.0.0.0 --port "${UVICORN_PORT:-8000}" --workers 1
