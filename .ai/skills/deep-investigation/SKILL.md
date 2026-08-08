# Skill: Deep Investigation

## Purpose
Thoroughly explore and build an authoritative mental model of a subsystem, feature, bug, or architectural component before making any modifications.

## Execution Steps

1. **Broad Symbol & Reference Search**:
   - Locate definitions, interfaces, models, schemas, and endpoints related to the target topic.
   - Do not stop at the first file found. Search callers, consumers, imports, and usages across `TimeWave-backend`, `TimeWaveFront`, and `TimeWaveNative`.

2. **Trace Control & Data Flow**:
   - Trace requests from API endpoint -> Serializer/Form -> View/Controller -> Service/Engine -> Model/Database.
   - Trace frontend components -> service client (`scheduleService.js`, etc.) -> API endpoints -> response handling.

3. **Check Related Infrastructure & Config**:
   - Inspect database models, migrations, environment settings, permissions, background tasks (Celery/redis), and active signals/middleware.

4. **Review Existing Tests & Documentation**:
   - Read relevant unit and integration test files (e.g., `tests*.py`, test scripts) to understand expected contracts and edge cases.

5. **Formulate Investigation Findings**:
   - Document key files, dependencies, contracts, and potential architectural risks.
   - Never conclude "not found" without performing multi-level pattern searches across all services.
