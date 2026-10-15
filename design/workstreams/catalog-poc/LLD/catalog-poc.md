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
│   └── shared/escalate-needs-human.yaml         # labels: {pack: shared}
├── rulesets/
│   ├── poc-release-docs.yaml  poc-docs-epic.yaml  poc-docs-human-review.yaml     # → docs
│   └── dev-release-docs.yaml  dev-docs-epic.yaml                                 # → mock-docs
└── testdata/<case>/{event.json,workitem.json,expected.json}
```

`ccf-skills` ref in every TaskSpec: the SKL release tag (`v0.8.0`). Required check name: `ci / required` once DWF lands (until then `test-build`) — a `param` on merge rules, `requiredCheck`.

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
| `implement` | `mode: new` → implement the sub-issue on a branch based on `inputs.base` (`factory/stack/<wi>/<n>`), open PR with base `inputs.base`; `mode: address-review` → address every unresolved thread (`resolve` skill), push, reply/resolve threads; `target: stack` → use `lisa` stack mode one-shot |
| `docs-review` | use `ccf-review:review` with the `docs-site` pack; with `verifyAgainst` use `docs-verify` against the running versions; one review per PR (stack: per PR, plus a summary comment on the top PR); approve only when no must-fix findings remain; Questions → outcome `needs-human` |
| `merge` | re-verify gates (approval by reviewer at head, `requiredCheck` green, no unresolved threads, no `do-not-merge`); stack: merge bottom-up, retarget the next PR, rebase, wait for checks, continue; review-required block → add merge label (Q5) |

Result schemas extend the base Result (`status`, `outcome`, `summary`) with `data`:

| Schema | `data` fields |
|---|---|
| `release-docs` | `train`, `branch`, `pr`, `repos[]{repo, from, to, bump, depth, sections[], versionCut, gifs[]}` |
| `plan-epic` | `plan[]{issue, title, dependsOn[]}` (also mirrored at top-level `plan` for rules: `run.result.plan`) |
| `implement` | `branch`, `pr`, `threadsResolved`, `stack[]{pr, branch}` |
| `review` | `findings{mustFix, shouldFix, question}`, `prs[]{pr, event}` |
| `merge` | `merged[]{pr, sha}`, `blockedBy` |

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
| `dev-release-docs` | `workflows`, `mock-docs`, `mock-*` | `trainManifest: repos.mock.yaml`, `allowDryRun: true` | `docsRepo: compliance-framework/mock-docs` |
| `poc-docs-epic` / `dev-docs-epic` | `docs` / `mock-docs` | — | same repo |
| `poc-docs-human-review` | `docs` | — | — |

So `docs-on-release-train` declares params `trainManifest`, `allowDryRun`, `docsRepo`, and its `where`:

```yaml
where: >
  event.repo == 'compliance-framework/workflows' && has(event.train) &&
  event.train.manifest == params.trainManifest &&
  ('train:done' in event.issue.labels || (params.allowDryRun && 'train:dry-run' in event.issue.labels))
```

All `merge-*` rules are in every RuleSet's `rules.exclude` until RUN-S4-T01.

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
| `deploy-succeeded-after-merge` | `ptbr-live` notify + close |
| `human-pr-opened` / `human-pr-draft` / `bot-pr` | review / nothing / nothing |
| `factory-stop-label` | everything suppressed by rails |
| `hostile-verdict` (future triage pack) | — (not in PoC) |

CI: `make catalog` (validate + rules test + `--coverage`, failing on uncovered PoC rules).
