# LLD — Rule engine (CORE)

> Workstream: [CORE](../../definitions.md#core-dispatcher-core) · Tasks: CORE-S1-T04, CORE-S1-T05 · Package: `internal/rules` (pure)
> Semantics: [03 §Evaluation semantics](../../../03-domain-model.md#evaluation-semantics), [04](../../../04-flows.md)

## API

```go
type Clock interface{ Now() time.Time }
type Engine struct{ Clock Clock }   // nil Clock: now = the event's time

type WorkItemView struct {          // read-only snapshot the runtime builds (store + latest observation)
    ID        string
    Links     []v1alpha1.Link
    State     map[string]any
    Counters  map[string]int
    Reporter  v1alpha1.Actor
    PR        *PRView               // primary PR observation, if any
    PRs       []PRView              // all linked PRs (stacks)
    Deploy    *DeployView           // latest deployment observation (item.deploy)
    RunsBySpec map[string][]RunSummary
}

type Rails interface {              // implemented by the runtime; pure fake in tests
    Check(a ActionIntent) RailVerdict   // kill switches, budgets, per-item cap, single active run, dedupe
}

type Evaluation struct {
    Event      string
    WorkItem   string
    Matches    []Match             // every (RuleSet, rule) candidate with result (matched / not / error / suppressed-by-final / duplicate{firedVia})
    Decisions  []Decision          // ordered actions to apply
    StateAfter map[string]any      // projected state after all set actions
}

type Decision struct {
    RuleSet, Rule string            // RuleSet: the one the rule fired through (its params, its where)
    Action        ActionIntent      // Dispatch{TaskSpec, Queue, Inputs, Harness, Requires, Context, Target} | Set{Key, Value} | Notify{...} | Cancel{TaskSpec} | Link{Ref} | Effect{Op, Args} | Debounce{Key, Until, Then []ActionIntent}
    Rail          RailVerdict       // Allowed | Suppressed{reason} | Deferred{reason}
    Inputs        map[string]any    // CEL inputs snapshot for the "why" view (bounded)
}

func (e *Engine) Evaluate(ctx context.Context, c *catalog.Catalog, ev v1alpha1.Event, wi WorkItemView, rails Rails) (Evaluation, error)
```

The engine **never** performs I/O: applying decisions (creating Tasks, writing state, sending notifications) is the rule controller's job (RT).

## Algorithm

```text
1. scope:
   if ev.Source == "factory":
       sets = RuleSets where any link in wi.Links matches set.Scope (provider ∈ set.Providers)
   else:
       sets = RuleSets where ev.Source ∈ set.Providers and ev.Data.Object/Repo matches set.Scope (and not Exclude)
2. candidates = for each set, for each EffectiveRule r in set.Rules (already priority-ordered):
       keep if ev.Type ∈ r.On and ev.Source ∈ r.Providers (or factory)
   merge all candidates across sets → order by (priority desc, ruleset name, rule name)
3. state := copy(wi.State); stop := false
   for r in candidates:
       if stop: record Match{suppressed-by-final}; continue
       if fired[(ev.ID, r.Name)] != "": record Match{duplicate, firedVia: fired[(ev.ID, r.Name)]}; continue   // dedupe on firing
       act := activation(ev, wi with state, r.Params)   // this RuleSet's merged params
       ok, err := r.Where.EvalBool(act)       // empty where = true
       if err: record Match{error}; continue           // doesn't consume the dedupe
       if !ok: record Match{not-matched}; continue     // doesn't consume the dedupe: a later RuleSet may still fire it
       fired[(ev.ID, r.Name)] = set.Name               // the where passed: this RuleSet fires the rule, no other will
       if r.Final: stop = true                         // final fails closed: set even if the actions fail below
       // stage: materialise ALL actions before committing any (atomic per rule)
       staged := []; overlay := copy(state)
       for a in r.Actions:                     // ordered; later actions see the staged overlay
           intent, err := materialise(a, act with item.state = overlay)   // evaluates "=" leaves
           if err: staged = nil; record Match{action-error, index, err}; break
           if intent is Set: overlay[k] = v
           staged = append(staged, intent)
       if staged == nil: continue             // nothing from this rule is committed
       // commit
       for intent in staged:
           verdict := rails.Check(intent)
           append Decision{set, r, intent, verdict}
           if intent is Set and verdict.Allowed: state[k] = v
4. return Evaluation{..., StateAfter: state}
```

Notes:

- **Dedupe on firing:** each RuleSet that selects a rule evaluates its `where` with that RuleSet's merged params; the rule **fires** at most once per (event, rule), through the first RuleSet (by name, within the priority order) whose `where` passes. A RuleSet whose `where` fails or errors doesn't consume the dedupe. The Decisions carry the RuleSet the rule fired through; the later RuleSets' Matches record `duplicate{firedVia}`. A rule whose `where` passed but whose actions then fail to materialise still counts as fired (`action-error`), so another RuleSet's params can't fire it as a fallback.
- **Atomicity:** a rule's actions are all-or-nothing at evaluation time: if any action fails to materialise, none of that rule's decisions are emitted (matching the CEL error contract). Rail verdicts are per action at commit time; a suppressed `set` is not applied to `StateAfter`, but later actions in the same rule were already materialised against the staged overlay (documented; fixtures cover it).
- **Counters** (`item.count`) come from `wi.RunsBySpec`, so a dispatch in the same evaluation does **not** increment them (it's not a run yet).
- **Debounce:** a rule with `debounce{window, key}` yields one `Debounce{Key: "<rule>/" + eval(key), Until: now+window, Then: <all the rule's staged actions>}`; the controller holds one pending intent per key and fires at `Until` (subsequent matches extend nothing — first wins, documented).
- **requestApproval** in the PoC yields `Unsupported` decision (Q18) and a catalog warning.
- **Cancel** before dispatch in the same rule lets `review-*` rules supersede stale reviews.

### Decisions taken in CORE-S1-T04

Where the sketch above left room, the engine does this:

- **Clock:** `rules.Clock` (`Now() time.Time`, `ClockFunc` adapter) is defined in the package (there's no shared `clock` package). With no Clock, `now` is the event's time, so replays and fixtures are deterministic.
- **`Debounce.Then` is `[]ActionIntent`:** all the rule's staged actions, in order, so a debounced rule with several actions loses none. Its `set`s are not in `StateAfter`; they apply when the debounce fires.
- **Debounce key:** `"<rule>/<value of debounce.key>"` (strings as they are, other values printed), or `"<rule>/<work item id>"` when the rule has no key, so two rules never share a pending intent. The key is evaluated with the state before the rule's actions.
- **`WorkItemView.Deploy`** (`DeployView{State, SHA, URL, ContainsMerge}`) backs `item.deploy` (`announce-ptbr-live`); the controllers build it over the item's merged PRs (`containsMerge` only when every one is deployed, [controllers.md](../../dispatcher-runtime/LLD/controllers.md#decisions-taken-in-rt-s2-t01)).
- **Counters:** `item.counters[spec]` (read by `item.count`) is `max(wi.Counters[spec], len(wi.RunsBySpec[spec]))`: a work item rebuilt from its marker had counts but no run summaries (the rebuild is gone since [P3](../../../proposals/P3-dispatcher-state-in-object-storage.md#no-rebuild-from-markers), and the max stays).
- **`run` and `approval`** come from `event.data.facts.run` and `event.data.facts.approval` on `factory.*` events. Every fact is also under `event.*`; the envelope fields (`id`, `type`, `source`, `subject`, `time`, `repo`, `object`) win over facts of the same name.
- **`requestApproval`** yields an `Unsupported{Action, Reason, RequestApproval}` decision with the verdict `unsupported` (rail `engine`); the rails are not consulted, and the rule's other actions are unaffected.
- **Scope:** a provider event's repo is `data.repo`, else the repo of `data.object.ref` (`owner/name#n`); its branch and channel come from `facts.branch` and `facts.channel`. A repo object matches `scope.repos` (minus `exclude`) and, when the event names a branch, `scope.branches`; a channel object matches `scope.channels`; a scope that names only channels holds no repo object and vice versa. `factory.*` events match links by the repo of their ref.
- **`link.from`** must be a `"=CEL"` value (giving an object ref `{provider, kind, ref}` or a ref string) or a literal ref string; a bare expression is not evaluated.
- **Final fails closed:** a final rule stops the later candidates once its `where` passes, even when its actions then fail to materialise (`action-error`), consistent with such a rule counting as fired for the dedupe.
- **Materialisation checks:** the TaskSpec, queue and template must exist, the effect op must be known, and dispatch inputs must pass the TaskSpec's declared inputs (`CompiledTaskSpec.ValidateInputs`, defaults not filled in); a harness that evaluates to a string is an error. Each failure is an `action-error`.
- **`Rails.Check`** receives the intent only; replay dedupe per (event id, rule) is the runtime's (task provenance). A `set` yields one `Set` decision per key, in key order, each with its own verdict.
- **Errors:** `Evaluate` returns an error only for a nil catalog or rails, or a done context; everything else is recorded in `Matches`. `Evaluation.Records(at)` turns matches and decisions into the work item's `v1alpha1.Decision` records.

## Rails contract (implemented in RT, faked here)

| Rail | Verdict when tripped |
|---|---|
| kill switch (global / provider / queue / item `factory:stop`) | `Suppressed{kill-switch}` |
| per-item run cap (default 20) | `Suppressed{run-cap}` |
| single active run per (item, TaskSpec) unless `dispatch.allowConcurrent` | `Suppressed{active-run}` |
| budget exceeded (queue/ruleset/global) | `Deferred{budget}` (task created as Pending with reason) |

## Fixtures and `factory rules test`

```text
catalog/testdata/<case>/   # a case is any directory holding an event.json, at any depth
  event.json           # a v1alpha1.Event (normalised) — or webhook.json + provider.json (normalised via the GH normaliser when available)
  workitem.json        # WorkItemView before
  rails.json           # optional: tripped rails (FakeRails); none = every action allowed
  expected.json        # golden Evaluation (Matches, Decisions, StateAfter), generated with --update
```

```text
factory rules test [--catalog catalog] [--fixtures <catalog>/testdata] [--update]
                   [--base-catalog dir [--fail-on-diff]] [--coverage] [--format text|json]
```

- Runs every case; diffs `Matches`, `Decisions` and `StateAfter` (stable ordering, inputs redacted to keys).
- Cases replay with an Engine without a Clock, so `now` is the event's time.
- The runner is `internal/rules` (`LoadFixtures`/`ReadFixture` over an `fs.FS`, `Fixture.Evaluate`, `Golden`/`ParseGolden`, `DiffGolden`). The engine's own tests (`internal/rules/testdata/cases/<catalog>/<case>`) use the same code and format.
- A mismatch prints a **decision diff** per case, rule by rule and decision by decision (`- expected, + now`), then exits 1:

  ```text
  FAIL review-human-prs-draft: decisions differ from expected.json (- expected, + now)
    ~ rule poc-docs-human-review/review-human-prs: not-matched -> matched
        inputs: [actor item] -> [actor]
    + decision poc-docs-human-review/review-human-prs: cancel docs-review [allowed]
        {"cancel":{"taskSpec":"docs-review"}}
    + decision poc-docs-human-review/review-human-prs: dispatch docs-review → poc [allowed]
        {"dispatch":{"taskSpec":"docs-review","queue":"poc","inputs":{"checks":["style","build"],"target":"pr"}}}
  ```

  Rules pair by (RuleSet, rule). Decisions pair by (RuleSet, rule, kind, the TaskSpec/key/template/ref/op they name) and occurrence, so a changed input or rail verdict reads as `~` with the changed fields (`dispatch.inputs.checks: [...] -> [...]`, `[allowed] -> [suppressed kill-switch: …]`). A reorder is one `~ decision order` line. State changes are per top-level key. A missing `expected.json` is a mismatch too.
- `--update`: rewrites the goldens that differ (or are missing), prints what changed, and exits 0.
- `--base-catalog`: evaluates each case with both catalogs, and prints the **decision diff** from base to `--catalog` (`- base, + head`) and "N of M cases change their decisions" (for catalog PRs). It doesn't read the goldens, so it doesn't combine with `--update`. It exits 0 unless `--fail-on-diff` is given.
- `--coverage`: lists rules never matched by any fixture (CI fails on uncovered PoC rules — CAT-S1-T04). Only the rules some RuleSet selects count. The others (e.g. the excluded `merge-*` rules) are listed as not selected. Exit 1 when a selected rule never fires.
- `--format json`: the report (cases with status `ok|differs|missing|updated|changed` and their structured changes, totals, coverage) on stdout.
- **Exit codes**, as for `factory validate`: 0 OK; 1 a mismatch or missing golden, a base/head change with `--fail-on-diff`, or an uncovered rule with `--coverage`; 2 a usage error, an unreadable fixture, or a catalog that doesn't load. A catalog that doesn't load prints its diagnostics in `validate`'s format on stderr. When the default `<catalog>/testdata` doesn't exist, it warns and exits 0, so a catalog without fixtures yet doesn't break `make catalog`. An explicit `--fixtures` that doesn't exist is exit 2.

## Tests (CORE-S1-T04)

| Case | Expectation |
|---|---|
| [04 cascade](../../../04-flows.md#how-rules-cascade) (4 events) | exact decisions per step |
| `final` + priority (`autoimplement` vs `request-implement-approval`) | lower rule suppressed |
| same rule via two RuleSets, both `where`s pass | decisions through the first RuleSet by name only; one `duplicate{firedVia}` match |
| same rule via two RuleSets, the first's `where` fails (dev/prod params, e.g. `dev-release-docs` and `poc-release-docs` on a prod train) | `not-matched` for the first, decisions through the second |
| factory event scoping via links | rules from the docs RuleSet fire for a run linked to `docs#57` |
| `where` runtime error | not matched + error recorded; the dedupe isn't consumed (a second RuleSet whose `where` passes fires it) |
| action materialise error after a valid `set` | **no** decisions from that rule; state unchanged; `action-error` match recorded |
| `set` then `dispatch` reading new state (`build-stack-next`) | dispatch input uses incremented index |
| `record-review-outcome` priority 100 before `merge-whole-stack` | merge sees `review == approved` |
| rails suppress / defer | verdicts recorded, state not updated on suppressed `set` |
| debounce | one `Debounce` decision with computed key |
