# Git & Version Control Principles

## 1. Minimal Scope
- Keep commits focused and atomic.
- Do not mix refactoring with new feature development or bug fixes unless required by the implementation.
- Avoid modifying formatting or whitespace of unrelated code blocks.

## 2. Commit & Artifact Integrity
- Ensure code builds and passes relevant tests before committing.
- Do not commit scratch scripts, temporary binary files, or raw credentials (`credentials.json`, secrets).
- Ensure `.gitignore` rules are respected across all subprojects (`TimeWave-backend`, `TimeWaveFront`, `TimeWaveNative`).
