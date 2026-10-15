# ADR-0006: Central configuration (catalog and repo bindings) lives in git in this repo

- **Status:** proposed
- **Date:** 2026-10-15

## Context
Providers, rules, task specs, queues, pools and per-repo onboarding must be defined centrally (P1, P6). Target repos should need no factory files. The UI must show what the factory is configured to do.

## Decision
- `catalog/` in this repo holds `providers/`, `rulesets/`, `rules/`, `taskspecs/`, `prompts/`, `queues/`, `pools/` and `harnesses/` (see ADR-0007), validated by JSON Schemas.
- Changes go through PRs with `CODEOWNERS`. CI validates schemas, checks harness configs against the adapter capability descriptors, and runs the reconciler in dry-run mode against recorded events.
- The dispatcher loads the catalog at a git SHA, reloads it when `main` changes, and records the SHA in every Task.
- The UI displays the catalog **read-only**. Any future editor in the UI would open PRs, not write directly.
- Onboarding a repo means adding a RuleSet scoped to it (usually `include`-ing shared rule sets) and installing the Provider's GitHub App on that repo.

## Consequences
- One review and audit path for every behaviour change, and it's easy to diff and roll back.
- Config changes need a PR and a deploy-free reload, not a click. That's acceptable at our scale.
- Secrets never live in the catalog. It only references them by name.

## Alternatives considered
- **Config in each target repo** (`.github/factory.yml`): spreads behaviour across ~60 repos and needs files in repos that might not want the factory.
- **UI- or DB-managed config:** faster onboarding, but a second change path with weaker review. Revisit if onboarding volume grows.
