-- Migration 061: manual (operator-owned) deployment mode
--
-- Some models are deployed by the operator OUTSIDE NexusLLM — a plain
-- `docker compose up` of a vLLM container, a systemd unit, a container on a
-- machine that runs no node agent at all. For those, NexusLLM must behave as a
-- gateway only: route traffic, enforce policy, probe health, report the
-- endpoint as unhealthy when the container is down — and never try to create,
-- recreate, stop, evict, or preempt the container.
--
-- Before this column there was no way to express that. Every runtime-creation
-- path (proxy cold-start activator, HA reconciler, stuck-runtime sweeper,
-- idle-manager restore, admin Start/Restart/Upgrade/Rollback) treated a
-- registered model with an unhealthy endpoint as "needs a container started",
-- so an operator-managed container would be duplicated on a different port —
-- or, when the model name collided with the container name, removed with
-- `docker rm -f` and replaced.
--
-- deployment_mode values:
--   managed — NexusLLM owns the container lifecycle (default; unchanged behavior)
--   manual  — the operator owns the container lifecycle; NexusLLM only routes,
--             enforces policy, and reports health
--
-- The predicate lives in Go in internal/modelguard (ManagedByNexus /
-- SQLManagedCondition) so every path shares one definition.

BEGIN;

ALTER TABLE models
    ADD COLUMN IF NOT EXISTS deployment_mode VARCHAR(10) NOT NULL DEFAULT 'managed'
        CHECK (deployment_mode IN ('managed', 'manual'));

COMMENT ON COLUMN models.deployment_mode IS
    'Who owns this model''s container lifecycle. managed = NexusLLM starts/stops/recovers it (default). manual = the operator deployed it themselves (docker compose, systemd, another orchestrator); NexusLLM routes to it and health-checks it but never starts, stops, evicts, preempts, or recreates its container. Enforced through internal/modelguard.ManagedByNexus / SQLManagedCondition.';

-- ── Backfill ─────────────────────────────────────────────────────────────────
-- A model that NexusLLM deployed always has a model_runtime_configs row (the
-- deploy handler writes it) and normally an agent_runtimes row. A local model
-- with a registered endpoint but NEITHER of those was registered through
-- POST /admin/v1/models/register — the endpoint-only path whose own API
-- response already promises "NexusLLM will not manage its container
-- lifecycle". Those rows are exactly the manual deployments, so make the
-- stored value match the promise instead of leaving them to be cold-started.
--
-- Remote/provider-backed models are excluded: they have no container either
-- way, and their routing is decided by backend_type, so flipping their mode
-- would only obscure why they never start.
UPDATE models m
SET deployment_mode = 'manual'
WHERE m.deployment_mode = 'managed'
  AND COALESCE(m.provider_is_external, FALSE) = FALSE
  AND m.backend_type NOT LIKE '%\_provider'
  AND EXISTS (SELECT 1 FROM model_endpoints me WHERE me.model_id = m.id)
  AND NOT EXISTS (SELECT 1 FROM model_runtime_configs mrc WHERE mrc.model_id = m.id)
  AND NOT EXISTS (SELECT 1 FROM agent_runtimes ar WHERE ar.model_id = m.id);

COMMIT;
