# LLD — `ccf-factory` plugin: plan-epic and merge (SKL)

> Workstream: [SKL](../../definitions.md#skl-ccf-skills-bump) · Task: SKL-S2-T04 · Repo: `ccf-skills`, new plugin dir `ccf-factory/`

## plan-epic

**Input:** the epic issue (context, fenced). **Output:** sub-issues + plan comment; result `plan-epic` schema.

1. Read the repo (read-only) enough to size the work; read `STYLE.md`/`CONTRIBUTING.md` when present.
2. Decide `single` (one PR is enough → outcome `single`, no sub-issues) vs `decomposed`.
3. Decompose into **3–8** sub-issues, ordered so each PR can stack on the previous and be reviewed alone; each sub-issue: title, scope, acceptance criteria, files likely touched, `dependsOn`.
4. `factory issue create` per sub-issue (with `--key plan-<n>` so retries don't duplicate), then `factory issue sub-add --parent <epic> --child <n>`.
5. Comment the plan (`factory comment --key plan-summary`).
6. Result: `{status: done, outcome: decomposed, plan: [{issue, title, dependsOn}], data: {…}}`.

Needs-info: if the epic is ambiguous on a decision only a human can make (e.g. "which locales?") → `needs_input` with the question.

## merge

**Input:** `target: pr | stack`, required check name (param), merge label (repo config). **Output:** merged PRs or a block reason; result `merge` schema.

```text
gates(pr): approvedBy reviewer at head SHA ∧ required check success ∧ unresolvedThreads == 0
           ∧ no hold label: do-not-merge, factory:hold, factory:stop       (explicit signals only; comment wording is never parsed)
           ∧ no human CHANGES_REQUESTED review newer than the factory approval
pr:    if !gates → outcome blocked{reason}; else factory pr merge (fallback merge label → labelled-for-merge)
stack: for pr in bottom→top:
         if !gates(pr) → blocked{pr, reason}; stop
         factory pr merge pr
         next := pr above; factory pr retarget next --base main; factory stack rebase; factory stack push
         if rebase had conflicts → outcome rebased-with-conflicts (rules send it back to review); stop
         wait for next's required check (poll via observe, ≤ 20 min) before continuing
```

A maintainer who wants the factory to wait adds `factory:hold` (documented in the `epic-plan` and `needs-human` templates and in the `docs` CONTRIBUTING.md). Never force-merges; never bypasses rulesets; never merges a PR the factory didn't author (human PRs are review-only).
