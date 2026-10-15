# LLD — Rule engine (CORE)

> Workstream: [CORE](../../definitions.md#core-dispatcher-core) · Tasks: CORE-S1-T04, CORE-S1-T05 · Package: `internal/rules` (pure)
> Semantics: [03 §Evaluation semantics](../../../03-domain-model.md#evaluation-semantics), [04](../../../04-flows.md)

## API

```go
type Engine struct{ Clock clock.Clock }

type WorkItemView struct {          // read-only snapshot the runtime builds (store + latest observation)
    ID        string
    Links     []v1alpha1.Link
    State     map[string]any
    Counters  map[string]int
    Reporter  v1alpha1.Actor
    PR        *PRView               // primary PR observation, if any
    PRs       []PRView              // all linked PRs (stacks)
    RunsBySpec map[string][]RunSummary
}

type Rails interface {              // implemented by the runtime; pure fake in tests
    Check(a ActionIntent) RailVerdict   // kill switches, budgets, per-item cap, single active run, dedupe
}

type Evaluation struct {
    Event      string
    WorkItem   string
    Matches    []Match             // every candidate rule with result (matched / not / error / suppressed-by-final)
    Decisions  []Decision          // ordered actions to apply
    StateAfter map[string]any      // projected state after all set actions
}

type Decision struct {
    RuleSet, Rule string
    Action        ActionIntent      // Dispatch{TaskSpec, Queue, Inputs, Harness, Requires, Context, Target} | Set{Key, Value} | Notify{...} | Cancel{TaskSpec} | Link{Ref} | Effect{Op, Args} | Debounce{Key, Until, Then ActionIntent}
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
       if seen[(ev.ID, r.Name)]: record Match{duplicate}; continue      // dedupe across RuleSets
       seen[(ev.ID, r.Name)] = true                                      // mark first, so every exit below counts
       act := activation(ev, wi with state, r.Params)
       ok, err := r.Where.EvalBool(act)       // empty where = true
       if err: record Match{error}; continue
       if !ok: record Match{not-matched}; continue
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
       if r.Final: stop = true
4. return Evaluation{..., StateAfter: state}
```

Notes:

- **Atomicity:** a rule's actions are all-or-nothing at evaluation time: if any action fails to materialise, none of that rule's decisions are emitted (matching the CEL error contract). Rail verdicts are per action at commit time; a suppressed `set` is not applied to `StateAfter`, but later actions in the same rule were already materialised against the staged overlay (documented; fixtures cover it).
- **Counters** (`item.count`) come from `wi.RunsBySpec`, so a dispatch in the same evaluation does **not** increment them (it's not a run yet).
- **Debounce:** a rule with `debounce{window, key}` yields `Debounce{Key: eval(key), Until: now+window, Then: <first action>}`; the controller holds one pending intent per key and fires at `Until` (subsequent matches extend nothing — first wins, documented).
- **requestApproval** in the PoC yields `Unsupported` decision (Q18) and a catalog warning.
- **Cancel** before dispatch in the same rule lets `review-*` rules supersede stale reviews.

## Rails contract (implemented in RT, faked here)

| Rail | Verdict when tripped |
|---|---|
| kill switch (global / provider / queue / item `factory:stop`) | `Suppressed{kill-switch}` |
| per-item run cap (default 20) | `Suppressed{run-cap}` |
| single active run per (item, TaskSpec) unless `dispatch.allowConcurrent` | `Suppressed{active-run}` |
| budget exceeded (queue/ruleset/global) | `Deferred{budget}` (task created as Pending with reason) |

## Fixtures and `factory rules test`

```text
catalog/testdata/<case>/
  event.json           # a v1alpha1.Event (normalised) — or webhook.json + provider.json (normalised via the GH normaliser when available)
  workitem.json        # WorkItemView before
  rails.json           # optional: tripped rails
  expected.json        # golden Evaluation (Decisions + StateAfter), generated with --update
```

```text
factory rules test --catalog catalog --fixtures catalog/testdata [--update] [--base-catalog path] [--coverage]
```

- Runs every case; diffs `Decisions` and `StateAfter` (stable ordering, inputs redacted to keys).
- `--base-catalog`: evaluates each case with both catalogs and prints a **decision diff** (for catalog PRs).
- `--coverage`: lists rules never matched by any fixture (CI fails on uncovered PoC rules — CAT-S1-T04).

## Tests (CORE-S1-T04)

| Case | Expectation |
|---|---|
| [04 cascade](../../../04-flows.md#how-rules-cascade) (4 events) | exact decisions per step |
| `final` + priority (`autoimplement` vs `request-implement-approval`) | lower rule suppressed |
| same rule via two RuleSets | one decision, one `duplicate` match |
| factory event scoping via links | rules from the docs RuleSet fire for a run linked to `docs#57` |
| `where` runtime error | not matched + error recorded; the rule is still marked seen (a second RuleSet selecting it records `duplicate`) |
| action materialise error after a valid `set` | **no** decisions from that rule; state unchanged; `action-error` match recorded |
| `set` then `dispatch` reading new state (`build-stack-next`) | dispatch input uses incremented index |
| `record-review-outcome` priority 100 before `merge-whole-stack` | merge sees `review == approved` |
| rails suppress / defer | verdicts recorded, state not updated on suppressed `set` |
| debounce | one `Debounce` decision with computed key |
