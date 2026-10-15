# ADR-0004: All external side effects go through the Effector with per-role tokens and separate identities

- **Status:** proposed
- **Date:** 2026-10-15

## Context
Agents read untrusted input (public issues, Slack) and hold write credentials. Review and merge only mean something if they're done by an identity other than the author's.

## Decision
- Agents change GitHub and Slack **only** via the Effector (the `lisa` CLI evolved into `factory …`, also exposed as an MCP server).
- The Effector sends **effect requests to the Factory API**. The dispatcher checks each request against the Task's effects allow-list and label globs, records it on the work item, and applies it through the **provider adapter**.
- Provider credentials (App private keys, the Slack bot token) never leave the dispatcher.
- For git operations inside the workspace (clone, push), each run gets a **GitHub App installation token** scoped to one repo and the role's permissions, with a TTL of ≤1h. Read-only roles get a read-only token.
- Two Apps, declared as Provider identities: `ccf-factory` (`author`: release-docs, plan-epic, implement) and `ccf-factory-reviewer` (`reviewer`: docs-review, merge). Each TaskSpec names its identity. `ccf-release-bot` and `ccf-repo-admin` are never available to agents.

## Consequences
- Defence in depth: prompt rules, then Effector policy, then token scope, then repo rulesets.
- Requires installing and maintaining two more GitHub Apps (or repurposing `ccf-lisa`).
- Harnesses that bypass the Effector (raw `gh` or `curl`) are still capped by the token scope.

## Alternatives considered
- One bot identity for everything: approvals become meaningless, and rulesets can't tell author from reviewer.
- PATs: long-lived, user-bound, over-scoped.
