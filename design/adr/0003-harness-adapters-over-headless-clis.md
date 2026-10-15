# ADR-0003: Harness-agnostic via thin adapters over headless CLIs (Claude Code and Codex first)

- **Status:** proposed
- **Date:** 2026-10-15

## Context
The agent layer must support at least Claude Code and Codex from the start. `ccf-skills` is a Claude Code plugin marketplace whose skills are `SKILL.md` folders.

## Decision
- One Go `Adapter` interface (`Prepare`, `Run`) with `claude-code` and `codex` implementations driving `claude -p` and `codex exec` headless.
- **Harness config travels both ways.**
  - *Down:* the Task carries `harness.config`: model, normalised `reasoningEffort` (`minimal|low|medium|high|max`) and a validated harness-specific `extra` passthrough. Values resolve TaskSpec default → RuleSet default → Rule `dispatch.harness` → command override.
  - *Up:* the Result carries `effectiveConfig` (resolved model id, effort) plus usage (tokens, cost, turns, wall-clock) and the harness version.
- Each adapter publishes a **capability descriptor** (models, effort levels and their native mapping, features). The catalog is validated against it in CI.
- Adapters map the Task's skills, effects, limits, output schema and config onto each harness's flags and config files.
- Skills stay single-source in `ccf-skills`. Adapters materialise them per harness. Portable skills avoid Claude-only plugin features and put deterministic logic in CLIs.

## Consequences
- Same Task, different harness. This enables A/B evaluation per role, and the effective config in each Result makes the comparison exact.
- Lowest common denominator for skills: hooks and slash commands are Claude-only extras, not something skills can depend on.
- CLI flags change over time, so pin harness versions in the agent image and cover them with adapter contract tests.

## Alternatives considered
- Claude Agent SDK for Claude: richer, but not Go and asymmetric with Codex. It stays an option behind the same interface.
- Vendor GitHub Actions (`claude-code-action`, `codex-action`): fuse dispatch, runtime and harness, and would duplicate role definitions per vendor.
