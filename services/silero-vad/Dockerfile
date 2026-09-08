# Silero VAD v5 (runanywhere/silero-vad-v5 weights, via silero-vad-notorch).
# CPU-only, single image — no GPU variant needed. Silero's own docs: a 30ms+
# chunk takes <1ms on a single CPU thread; GPU adds nothing here.
FROM python:3.11-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ffmpeg libsndfile1 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py entrypoint.sh .
RUN chmod +x entrypoint.sh

EXPOSE 8000
ENTRYPOINT ["./entrypoint.sh"]
