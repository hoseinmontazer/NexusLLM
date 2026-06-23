-- NexusLLM Migration 013 — Add START_MODEL to agent_tasks task_type constraint
--
-- Root cause: the activator enqueues TaskStartModel = "START_MODEL" which was
-- introduced as the unified startup task type, but the CHECK constraint on
-- agent_tasks.task_type (migration 007) only listed DEPLOY_RUNTIME and other
-- legacy types.  This caused every lazy-load cold start to fail with:
--
--   pq: new row for relation "agent_tasks" violates check constraint
--        "agent_tasks_task_type_check"
--
-- Architecture decision: START_MODEL is the single unified task type for all
-- startup scenarios (initial deploy, cold start, re-deploy, crash recovery,
-- lazy load).  DEPLOY_RUNTIME, WARM_RUNTIME, and RESTART_RUNTIME are kept in
-- the constraint for backward compatibility with in-flight tasks only.
BEGIN;

ALTER TABLE agent_tasks DROP CONSTRAINT IF EXISTS agent_tasks_task_type_check;

ALTER TABLE agent_tasks ADD CONSTRAINT agent_tasks_task_type_check
    CHECK (task_type IN (
        -- Unified startup pipeline (current)
        'START_MODEL',
        -- Runtime lifecycle (legacy — accepted for backward compat)
        'DEPLOY_RUNTIME',
        'STOP_RUNTIME',
        'RESTART_RUNTIME',
        'DELETE_RUNTIME',
        'WARM_RUNTIME',
        'UNLOAD_RUNTIME',
        -- Model management
        'PULL_MODEL',
        'DELETE_MODEL',
        'VERIFY_MODEL',
        -- Observability
        'COLLECT_INVENTORY',
        'HEALTH_CHECK'
    ));

COMMIT;
