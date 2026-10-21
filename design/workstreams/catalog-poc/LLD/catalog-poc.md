# LLD — PoC catalog (CAT)

> Workstream: [CAT](../../definitions.md#cat-poc-catalog) · Tasks: CAT-S1-T01…T04 · Directory: `catalog/`
> Source of truth for the content: [10 §PoC catalog](../../../10-poc.md#poc-catalog). This LLD adds what 10 leaves open: prompts, result schemas, dev/prod split, fixtures.

## Inventory

```text
catalog/
├── providers/github-public.yaml
├── groups/maintainers.yaml                      # orgRole admin
├── queues/poc.yaml
├── pools/local.yaml
├── taskspecs/{release-docs,plan-epic,implement,docs-review,merge}.yaml
├── prompts/{release-docs,plan-epic,implement,docs-review,merge}.md.tmpl
├── schemas/results/{release-docs,plan-epic,implement,review,merge}.json
├── templates/{release-docs-published,epic-plan,ptbr-live,needs-human}.yaml
├── rules/
│   ├── release-docs/*.yaml                      # pack: release-docs
│   ├── docs-epic/*.yaml                         # pack: docs-epic
│   ├── review-human/*.yaml                      # pack: review-human
│   └── shared/escalate-needs-human.yaml         # labels: {pack: shared} — selected by every RuleSet (below)
├── rulesets/
│   ├── poc-release-docs.yaml  poc-docs-epic.yaml  poc-docs-human-review.yaml     # → docs
│   └── dev-release-docs.yaml  dev-docs-epic.yaml                                 # → mock-docs
└── testdata/<case>/{event.json,workitem.json,expected.json}
```

`ccf-skills` ref in every TaskSpec: the SKL release tag (`v0.8.0`). Required check name: `ci / required` once DWF lands (until then `build`, from docs' push-only "Test Build" workflow). It's a `requiredCheck` **param** on the merge rules, passed to the `merge` TaskSpec as the **input** of the same name (default `ci / required`); the rules' `where` checks the rollup state, the merge task checks that specific check.

## TaskSpecs, prompts and result schemas

TaskSpec YAML is as in [10 §TaskSpecs](../../../10-poc.md#taskspecs). Prompts are Go `text/template` with `.inputs`, `.item` (summary), `.task`; each prompt has the same skeleton:

```text
# Task: {{ .task.spec }} for {{ .task.target.repo }}
## Goal            — one paragraph
## Inputs          — rendered inputs (no untrusted text; context arrives separately)
## Use these skills — e.g. "Use the docs-release skill (ccf-docs) for the procedure."
## Rules           — task-specific constraints (below)
## Done when       — checklist the agent must satisfy before writing the result
## Result          — "Write JSON to $FACTORY_RESULT matching the schema; outcome ∈ {…}"
```

| Prompt | Task-specific rules (summary) |
|---|---|
| `release-docs` | read the train via context; per repo apply `depthPolicy`; follow `STYLE.md` and `instances.yaml`; one version per minor line; GIFs only from the local stack; one branch `factory/release-docs/<train-month>`; open **one** PR; never edit other instances' released versions except the minor line being patched |
| `plan-epic` | 3–8 sub-issues, each independently reviewable and mergeable in order; each with acceptance criteria; create them with `factory issue create` + `factory issue sub-add`; comment the plan |
| `implement` | `mode: new` → implement the sub-issue on a branch based on `inputs.base` (`factory/stack/<wi>/<n>`), open PR with base `inputs.base`; `mode: address-review` → address every unresolved thread (`resolve` skill), push, reply/resolve threads; `target: stack` → use `lisa` stack mode one-shot; all modes: run the repo's checks only when their toolchain and dependencies are already there (never install), a check that can't run is `not-run` in `data.checks` and the summary and is left to the PR's CI, a check that runs and fails is fixed or ends `blocked` |
| `docs-review` | use `ccf-review:review` with the `docs-site` pack; with `verifyAgainst` use `docs-verify` against the running versions; one review per PR (stack: per PR, plus a summary comment on the top PR); approve only when no must-fix findings remain; Questions → outcome `needs-human` |
| `merge` | re-verify gates (approval by reviewer at head, `requiredCheck` green, no unresolved threads, no hold label: `do-not-merge`, `factory:hold` or `factory:stop`, no human `CHANGES_REQUESTED` review newer than the approval); stack, one pass: merge what's ready bottom-up, retarget the next PR, rebase, push; next PR not ready yet (check pending, approval at an old head) → outcome `pending` with `waitingOn`, never poll; review-required block → add merge label (Q5). Outcomes: `merged`, `labelled-for-merge`, `rebased-with-conflicts`, `pending` (non-escalating), `blocked`, `needs-human` |

Result schemas extend the base Result (`status`, `outcome`, `summary`) with `data`:

| Schema | `data` fields |
|---|---|
| `release-docs` | `train`, `branch`, `pr`, `repos[]{repo, from, to, bump, depth, sections[], versionCut, gifs[]}` |
| `plan-epic` | `plan[]{issue, title, dependsOn[]}` (also mirrored at top-level `plan` for rules: `run.result.plan`) |
| `implement` | `branch`, `pr`, `threadsResolved`, `checks{<name>: pass\|fail\|not-run}`, `stack[]{pr, branch}` |
| `review` | `findings{mustFix, shouldFix, question}`, `prs[]{pr, event}` |
| `merge` | `merged[]{pr, sha}`, `blockedBy{pr, reason}` (required on `blocked`, `rebased-with-conflicts`), `waitingOn{pr, reason}` (required on `pending`) |

## Templates

| Template | GitHub body (summary) |
|---|---|
| `release-docs-published` | "📚 Docs for {{train.month}} are live: per-repo list with version lines + links; PR link; factory UI link" |
| `epic-plan` | numbered sub-issues with links, the stacking order, how to stop (`factory:stop`) |
| `ptbr-live` | "PT-BR is live: https://compliance-framework.github.io/docs/pt-BR/ — stack: #… #…" |
| `needs-human` | what blocked, last run link, which rule escalated, how to resume |

## Dev and prod RuleSets

Same rules, different targets — selected by RuleSet, never by editing rules.

| RuleSet | Scope | Trigger difference (via params/overrides) | Target |
|---|---|---|---|
| `poc-release-docs` | `workflows`, `docs` | `docs-on-release-train.where` includes `event.train.manifest == 'repos.yaml' && !event.train.dryRun` (param `trainManifest: repos.yaml`) | `docsRepo: compliance-framework/docs` |
| `dev-release-docs` | `workflows`, `mock-docs`, `mock-*` | `trainManifest: repos.mock.yaml` (`docs-on-release-train`, `announce-release-docs`), `allowDryRun: true` | `docsRepo: compliance-framework/mock-docs` |
| `poc-docs-epic` / `dev-docs-epic` | `docs` / `mock-docs` | — | same repo |
| `poc-docs-human-review` | `docs` | — | — |

So `docs-on-release-train` declares params `trainManifest`, `allowDryRun`, `docsRepo`, and its `where`:

```yaml
where: >
  event.repo == 'compliance-framework/workflows' && has(event.train) &&
  event.train.manifest == params.trainManifest &&
  (('train:done' in event.issue.labels && !event.train.dryRun) ||
   (params.allowDryRun && 'train:dry-run' in event.issue.labels))
```

Both RuleSets scope `workflows`, so `factory.*` events on a train item would reach both. `docs-on-release-train` therefore sets `manifest: "=event.train.manifest"` (a state key of both RuleSets), and every `factory.*` rule of the pack (`announce-release-docs` and the `record-release-*` rules on `factory.run.completed`) declares the same `trainManifest` param and requires `item.state.manifest == params.trainManifest`. The pack's provider-event rules need no such guard: they require `event.repo == params.docsRepo`, and each RuleSet scopes only its own docs repo.

Every RuleSet selects its own pack **plus the `shared` pack**, so `escalate-needs-human` applies everywhere:

```yaml
rules:
  selector: { matchExpressions: [{ key: pack, operator: In, values: [release-docs, shared] }] }   # docs-epic / review-human likewise
  exclude: [merge-release-docs]
```

All `merge-*` rules are in every RuleSet's `rules.exclude` until RUN-S4-T01.

## The release-docs pack

| Rule | On | Does |
|---|---|---|
| `docs-on-release-train` | `issue.closed` (a finished train in `workflows`) | dispatch `release-docs` to `docsRepo`; set `release: drafting`, `train`, `manifest` |
| `record-release-docs-repos` | `factory.run.completed` (`release-docs`, priority 100) | set `repos` from the run's `data.repos`, for the announcement's per-repo table |
| `record-release-docs-outcome` | `factory.run.completed` (`release-docs` ending `pr-opened` or `no-docs-change`, priority 100) | set `release` to the outcome |
| `review-release-docs` | `pr.opened`, `pr.updated` (the factory's docs PR) | dispatch `docs-review` on the PR; set `review: running` |
| `record-release-review-outcome` | `factory.run.completed` (`docs-review`, priority 100) | set `review` to the outcome |
| `fix-release-docs-review` | `pr.review.submitted` (changes requested by the reviewer identity or a maintainer) | dispatch `implement address-review` on the PR (at most 3) |
| `merge-release-docs` | `pr.review.submitted`, `pr.checks.completed` | dispatch `merge` on the PR. Excluded from every RuleSet (Q10) |
| `record-release-docs-merged` | `pr.merged` on `docsRepo` (by anyone, a maintainer's manual merge included) | set `release: merged`, unless it's already `published` |
| `announce-release-docs` | `factory.run.completed` (`merge` ending `merged`) | notify `release-docs-published`; set `release: published` |

The `release` state key, which places the work item in the web board's lanes (`web/src/components/workitem/board.ts`):

```text
drafting ──release-docs run──▶ pr-opened ──docs PR merged (anyone)──▶ merged
   │                              │
   │                              └──factory merge run──▶ published   (announce-release-docs; merged → published too)
   └──release-docs run──▶ no-docs-change
```

`drafting` is a Running value, `pr-opened` an open one (the review's state decides between Running and Waiting), and `merged`, `published` and `no-docs-change` are Done. A `blocked` or `needs-human` release-docs run leaves `release: drafting`; the shared `escalate-needs-human` rule handles it. While `merge-release-docs` is excluded, a release ends `merged` when a maintainer merges the docs PR, with no announcement; once it's included, `announce-release-docs` sets `published`, and `record-release-docs-merged` never overwrites it, whichever of the two events comes first.

## Fixtures (CAT-S1-T04)

| Case | Expect |
|---|---|
| `train-mixed-prod` (api minor, ui patch, plugin patch) | dispatch `release-docs{train}` to docs; state `release: drafting` |
| `train-mock-dev` | dispatch to mock-docs via dev RuleSet; prod RuleSet silent |
| `train-dryrun-prod` | nothing |
| `release-docs-pr-opened` (factory) | `docs-review` with `verifyAgainst.train`, `requires has-local-stack` |
| `review-changes-requested-1` / `-3` | `implement address-review` / escalate `needs-human` |
| `epic-opened-maintainer` / `epic-opened-anyone` | `plan-epic` / nothing (no triage pack in PoC) |
| `plan-decomposed-5` | state plan + stackNext 0 + dispatch implement #1 base main |
| `implement-pr-opened-k` (k=1..5) | next layer / at k=5 `docs-review target: stack` |
| `stack-review-approved` + `checks-green` | (merge excluded) no merge decision; after RUN-S4 golden update: `merge target: stack` |
| `deploy-succeeded-after-merge` | `ptbr-live` notify + close + `announced` |
| `deploy-succeeded-before-merge-recorded` (race) | nothing on the deploy event; then on `factory.run.completed` (merge merged, `item.deploy` success) → `ptbr-live` once |
| `human-pr-opened` / `human-pr-draft` / `bot-pr` | review / nothing / nothing |
| `factory-stop-label` | everything suppressed by rails |
| `merge-pending-no-escalation` | `record-merge-outcome` sets `merge: pending`; `escalate-needs-human` doesn't fire |
| `release-docs-run-pr-opened` / `dev-release-docs-run-pr-opened` | `repos` recorded; `release: pr-opened` |
| `release-docs-run-no-docs-change` | `release: no-docs-change` |
| `dev-release-docs-human-merged` / `-drafting` (a maintainer merges the mock-docs PR; `release: pr-opened` / `drafting`) | `release: merged`; no merge run (`merge-release-docs` excluded), no announcement |
| `release-docs-merged-after-published` | nothing: `published` stays |
| `release-docs-merged-other-repo` (a PR merged in `workflows`) | nothing |
| `hostile-verdict` (future triage pack) | — (not in PoC) |

CI: `make catalog` (validate + rules test + `--coverage`, failing on uncovered PoC rules).
