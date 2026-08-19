# Manual Deployments — Containers You Run Yourself

Some models are not deployed through NexusLLM. You bring the container up with
`docker compose up`, a systemd unit, or another orchestrator — possibly on a
host that runs no node agent at all — and you want NexusLLM to act as the
gateway in front of it: routing, auth, policy, quotas, usage, health.

That is what `deployment_mode = manual` is for.

| | `managed` (default) | `manual` |
|---|---|---|
| Who starts the container | NexusLLM | **You** |
| Cold start on first request | Yes | **Never** |
| Idle eviction | Yes (lazy_load) | **Never** |
| HA replacement / crash recovery | Yes | **Never** |
| Preemption under GPU pressure | Yes | **Never** |
| Stuck-runtime recovery sweep | Yes | **Never** |
| Admin start / stop / restart | Yes | **409 Conflict** |
| Health checks | Yes | **Yes** |
| Routing, policy, quotas, usage | Yes | **Yes** |

A manual model whose container is down is reported as **unhealthy** and requests
for it get `503 manual_runtime_unhealthy`. Nothing tries to bring it up. When you
start the container again, the next health probe marks the endpoint healthy and
traffic flows — no admin action needed.

---

## Registering a container you already run

Example: a vLLM container started from your own compose file, serving
`--served-model-name qwen` on port 8000 of the GPU host.

```yaml
# /docker/qwen3-8/docker-compose.yml — yours, NexusLLM never touches it
services:
  qwen3-8-27b:
    image: vllm/vllm-openai:latest
    container_name: nexus-qwen3.8-27B
    restart: unless-stopped
    ports: ["8000:8000"]
    command: [/models/Qwen3.8-27B, --served-model-name, qwen, ...]
```

Register it as a manual deployment:

```bash
curl -X POST http://localhost:8081/admin/v1/models \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
        "name":            "qwen",
        "display_name":    "Qwen3.8 27B (manual)",
        "backend_type":    "vllm",
        "service_type":    "CHAT",
        "host":            "10.0.0.5",
        "port":            8000,
        "max_context":     131072,
        "deployment_mode": "manual"
      }'
```

`POST /admin/v1/models` defaults to `manual` — it registers an endpoint and never
creates a runtime config or a container. Pass `"deployment_mode": "managed"`
explicitly if you intend to hand the lifecycle to NexusLLM later.

`POST /admin/v1/models/deploy` (the full deploy path, and the **Deploy** wizard in
the web UI) also accepts `"deployment_mode": "manual"`. There it means: record the
model with all its runtime metadata, point the endpoint at `host:port`, and skip
the `START_MODEL` dispatch. `host` and `port` are then required — they must point
at the container that is already listening.

Grant the model to a team as usual (`POST /admin/v1/teams/:id/models`); manual
deployments go through the same permission and policy pipeline as everything else.

---

## Switching an existing model

A model NexusLLM has been trying to cold-start can be handed over at any time:

```bash
curl -X PUT http://localhost:8081/admin/v1/models/$MODEL_ID/deployment-mode \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"deployment_mode":"manual"}'
```

In the web UI: **Models** → expand the model → **mark as manual deployment**. The
model then carries a `🔧 MANUAL` badge and its start/stop/restart controls are
replaced by a note, since the API rejects those calls.

Switching to `manual` **stops managing** the container — it does not stop the
container. If NexusLLM had already started one, the response reports how many
runtimes are still running so you can stop them on the host yourself. Stop them
before switching if you want a clean handover; otherwise NexusLLM keeps routing
to whatever is listening on that host and port.

`{"deployment_mode":"managed"}` takes the lifecycle back. Make sure no container
of yours is still bound to the endpoint's port first — NexusLLM will start its own.

---

## Container name safety

NexusLLM names the containers it creates `nexus-<model>` and, before starting one,
removes any container already holding that name. Operators frequently name their
own containers `nexus-something` too, so two protections keep your container safe:

- Every container NexusLLM creates is labelled `nexus.managed=true`. The node
  agent's cleanup of exited containers only ever removes labelled ones.
- Before removing a container by name, the agent inspects it. If it is not
  labelled `nexus.managed=true` and *is* labelled with a docker-compose project
  (`com.docker.compose.project`), the start **fails with an explanatory error**
  instead of deleting your container. Rename one of the two, or register the
  model as a manual deployment.

Containers created by NexusLLM versions before these labels existed are treated
as NexusLLM's, exactly as before.

---

## Where the rule is enforced

`models.deployment_mode` (migration 061) is checked through one shared predicate,
`internal/modelguard` — `ManagedByNexus()` in Go and `SQLManagedCondition` in SQL —
used by every path that could touch a container:

| Path | File |
|---|---|
| Proxy cold start (503, no start) | `internal/proxy/handler.go` → `handleColdStart` |
| Cold-start activator (all start triggers) | `internal/runtimemgr/activator.go` |
| Idle eviction & always-running restore | `internal/runtimemgr/idle_manager.go` |
| HA reconciler (replacement, draining) | `internal/ha/reconciler.go` |
| Preemption engine | `internal/preemption/engine.go` |
| Stuck-runtime sweeper | `cmd/admin/main.go` |
| Admin start / stop / restart / upgrade / rollback | `internal/admin/handlers/controller.go` |
| Container cleanup on the node | `internal/nodeagent/executor.go` |

---

## Troubleshooting

**Requests return `503 manual_runtime_unhealthy`.** The container is not
answering its health check. Check it on the host (`docker ps`, `docker logs`), and
confirm the registered `host:port` matches where it actually listens
(`GET /admin/v1/models/:id/health` shows the endpoint address, its health status
and `deployment_mode`).

**A model I run myself keeps getting a second container.** It is still
`managed` — switch it to `manual` as shown above. Confirm with
`GET /admin/v1/models`, which now reports `deployment_mode` per model.

**Admin start returns 409.** Expected for a manual deployment: start the
container on the host, or switch the model back to `managed` first.

**The endpoint stays unhealthy after the container is up.** The registry picks it
up on the next watcher tick. If it does not, reset the failure counters with
`POST /admin/v1/models/:id/reset-health` — that only clears NexusLLM's own
counters and never touches your container.
