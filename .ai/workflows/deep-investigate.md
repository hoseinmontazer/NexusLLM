# Workflow: Deep Investigate

## Purpose
Systematically investigate a system component, feature, bug, or architectural pattern before proposing or making changes.

## Workflow Steps

1. **Invoke Skill**: Use `deep-investigation`.
2. **Execute Multi-File Search**:
   - Locate definitions across `TimeWave-backend`, `TimeWaveFront`, and `TimeWaveNative`.
   - Trace callers, API endpoints, backend views, serializers, models, and frontend client calls.
3. **Analyze Execution Paths**:
   - Trace control flow and data transformations end-to-end.
4. **Inspect Tests & Constraints**:
   - Read test suites and configuration files to identify hidden assumptions.
5. **Document Investigation Summary**:
   - Summarize discovered architecture, key components, dependencies, and risks.
