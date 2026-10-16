# LLD — CEL environment and runtime expressions (CORE)

> Workstream: [CORE](../../definitions.md#core-dispatcher-core) · Task: CORE-S1-T02 · Package: `internal/cel` (pure)

## Two expression mechanisms

| Mechanism | Syntax | When | Where allowed |
|---|---|---|---|
| **Params substitution** | `{{ params.name }}` (whole string, or embedded) | **catalog load**, after RuleSet overrides/defaults are merged | any string in a Rule's `actions` |
| **Runtime CEL** | a string whose first char is `=` (`"=event.release.tag"`) | **evaluation time** | `where` (always CEL, no `=`), action values, TaskSpec fields that depend on `inputs`/`task` (`workspace.repos[].ref`) |

A literal string starting with `=` is written `"\\=..."` (escaped). `{{ }}` substitution produces strings, or the original typed value when the whole string is a single placeholder (so `harness: "{{ params.harness }}"` yields the object).

## Environment

Declared once with `cel.NewEnv`, all variables as `map(string, dyn)`-backed objects with documented fields (dyn keeps the PoC flexible; field typos are caught by a **static field allow-list** check at load).

| Variable | Fields (documented allow-list) |
|---|---|
| `event` | `id, type, source, subject, time, repo, object{kind, ref, number}, labels, text, issue{labels, body, title}, release{tag, previousTag, semver{major,minor,patch,prerelease,bump}, conclusion, train}, review{state, commit}, label, workflow{name, conclusion}, train{month, manifest, dryRun, repos[]{name, versions, bump}} (CCF train issues, via the GitHub adapter's `ccf-train` extractor)`. Not `raw`: the provider payload is never shown to rules (`v1alpha1.EventData.Raw`) |
| `actor` | `id, tier, isBot, isFactory, identity` |
| `item` | `id, state (map), counters (map), runs (map: TaskSpec → Run summaries, oldest first, from `WorkItemView.RunsBySpec`), links, reporter{id, tier}, pr{author{isFactory}, draft, head, labels, checks{state, settled}, approvedBy[]{identity, commit}, unresolvedThreads}, prs[], deploy{state, sha, url, containsMerge}` + functions below |
| `run` | `id, spec, outcome, status, inputs (map), result (map), agent, harness{name, model}` |
| `approval` | `name, phase, by` (future) |
| `provider` | `name, type, labels` |
| `params` | resolved params (map) |
| `inputs`, `task` | only in TaskSpec field expressions: `inputs` (map), `task{id, target{repo}}` |
| `now` | timestamp |

### Custom functions

| Function | Meaning |
|---|---|
| `item.count(spec string) int` | number of Runs of TaskSpec `spec` on this work item (derived counters) |
| `item.lastRun(spec string) map` | last Run summary of `spec` (the last element of `item.runs[spec]`) or `null` |
| `hash(s string) int` | stable FNV-1a 32-bit → for sampling (`hash(item.id) % 10 == 0`) |
| `semver.bump(a, b string) string` | `major\|minor\|patch\|none` |
| `glob(pattern, s string) bool` | `internal/patterns` glob |
| standard: `has()`, `size()`, `exists`, `all`, `matches`, `startsWith`, `contains` | CEL built-ins |

## Compilation and limits

- `Compile(expr string, kind ExprKind) (*Program, error)` — `ExprKind` is `Where` (must return bool) or `Value` (any).
- Programs are compiled **once at catalog load** and cached in the `Catalog` snapshot (keyed by file+path).
- `cel.CostLimit(10_000)` and `cel.InterruptCheckFrequency`; evaluation timeout 50 ms via context.
- Determinism: no `now` in `where` unless the rule declares a `schedule` (lint warning otherwise).

## Evaluation

```go
type Activation struct { Event, Actor, Item, Run, Approval, Provider, Params, Inputs, Task map[string]any; Now time.Time }
func (p *Program) EvalBool(ctx context.Context, a Activation) (bool, error)
func (p *Program) EvalValue(ctx context.Context, a Activation) (any, error)
func EvalTree(ctx context.Context, v any, a Activation, progs ProgramIndex) (any, error) // walks maps/lists, evaluates "=" leaves
```

Error semantics (never panic):

| Situation | Result |
|---|---|
| `where` errors at runtime (missing key, type error) | rule **does not match**; decision records `error: <msg>`; metric `rule_eval_errors_total{rule}` |
| action value errors | the **whole rule's actions are skipped**; decision records the error |
| cost/timeout exceeded | same as error, code `cel.limit` |

## Tests

- Every `where` and `=` expression of the PoC catalog compiles (fixture copy).
- Escaping, whole-placeholder typing, nested `=` in lists/maps.
- Error-path tables for each row above.
