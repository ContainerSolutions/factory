# LLD — Catalog loading and validation (CORE)

> Workstream: [CORE](../../definitions.md#core-dispatcher-core) · Task: CORE-S1-T03 · Package: `internal/catalog` (pure) · CLI: `factory validate`

## Snapshot

```go
type Catalog struct {
    SHA        string                         // git commit, or "dir:<hash>" for local dirs
    Providers  map[string]*v1alpha1.Provider
    Groups     map[string]*v1alpha1.Group
    Templates  map[string]*v1alpha1.Template
    Rules      map[string]*v1alpha1.Rule      // standalone + inline (name-qualified "ruleset/rule" for inline)
    RuleSets   map[string]*CompiledRuleSet    // effective rules after selection/exclude/overrides/defaults
    TaskSpecs  map[string]*CompiledTaskSpec   // with digest, compiled "=" fields, input schema
    Queues     map[string]*v1alpha1.Queue
    Pools      map[string]*v1alpha1.Pool
    Programs   cel.ProgramIndex               // compiled where/"=" programs
    Sources    map[ObjectKey]SourcePos        // file:line for every object (errors, UI)
    Assets     Assets                         // prompts (parsed templates) and result schemas (compiled), keyed by relative path
}
```

Immutable after `Load`; the runtime swaps pointers atomically.

```go
type CompiledRuleSet struct {
    Spec      *v1alpha1.RuleSet
    Providers []string                       // provider instances matched by spec.providerSelector
    Scope     CompiledScope                  // compiled globs for repos/exclude/channels
    Rules     []*EffectiveRule               // ordered by priority desc, then name
}
type EffectiveRule struct {
    Name      string                         // rule name
    Rule      *v1alpha1.Rule                 // after {{ params }} substitution with this RuleSet's params
    Params    map[string]any                 // rule params ← ruleset defaults ← ruleset overrides[rule]
    Where     *cel.Program
    Actions   []CompiledAction               // "=" leaves pre-compiled
    Providers []string                       // ruleset providers ∩ rule.providerSelector (or ["factory"] for factory-only rules)
}
```

## Loading

```go
func LoadDir(fsys fs.FS, opts Options) (*Catalog, []Error)
func LoadGit(ctx context.Context, repoURL, ref string, opts Options) (*Catalog, []Error) // shallow clone to temp, then LoadDir
type Options struct { Capabilities []v1alpha1.AdapterCapabilities; KnownModels map[string][]string; Strict bool }
```

1. **Object discovery:** walk every `*.yaml`/`*.yml` file **except** under `prompts/`, `schemas/` and `testdata/` (directory names are otherwise conventions; **kind** decides). **Assets** are loaded separately and never decoded as objects: `prompts/**` → `Catalog.Assets.Prompts` (raw templates, parsed with `text/template` at load), `schemas/**/*.json` → `Catalog.Assets.Schemas` (compiled JSON Schemas). A YAML document without `apiVersion`/`kind` outside those dirs is error `CAT002`.
2. Split multi-document YAML; decode **strictly** with `v1alpha1.Decode` (YAML 1.2, unknown fields are errors) into the kind's type; record `file:line` per document.
3. Duplicate `(kind, name)` → error `CAT001`.

## Validation phases (all errors collected, not fail-fast)

| # | Phase | Checks | Code |
|---|---|---|---|
| 1 | Schema | JSON Schema validation of each document (from `schemas/`) | `CAT100` |
| 2 | References | Rule→TaskSpec/Queue/Template/Group; Queue→Pool; RuleSet refs/exclude/overrides name existing rules; Trust/Group→Provider; TaskSpec→prompt/output schema files exist | `CAT200`–`CAT209` |
| 3 | Selection | RuleSet providerSelector matches ≥1 Provider; rule selector matches ≥1 rule; a rule's `providerSelector` intersects the RuleSet's providers unless `on` is all `factory.*` | `CAT300`–`CAT303` |
| 4 | Event vocabulary | every `on` type is emittable by the effective providers' types (`v1alpha1` map); `factory.*` only from the built-in provider; `schedule` only with `factory.schedule.tick` | `CAT400`–`CAT402` |
| 5 | Params | overrides/defaults keys are declared `params` of the rule (defaults ignored when undeclared); `{{ params.x }}` references declared params | `CAT500`–`CAT502` |
| 6 | CEL | `where` compiles to bool; every `=` leaf compiles; field allow-list check; cost estimate under limit | `CAT600`–`CAT603` |
| 7 | Dispatch | `inputs` keys ⊆ TaskSpec `inputs`; required inputs provided (literal or `=`); literal types/enums valid; `requires` merge valid | `CAT700`–`CAT703` |
| 8 | Harness | request patterns compile; request ⊆ TaskSpec constraint (via `patterns.Subsumes` with `KnownModels`); effort values ∈ normalised set; adapter capability features required by TaskSpec exist | `CAT800`–`CAT803` |
| 9 | Effects | every op in TaskSpec `effects.allow` is a known op; `effect` actions' ops are in the vocabulary; review ops only with `identity: reviewer`; `push-branch`/`stack.*` only with a `rw` workspace repo | `CAT900`–`CAT903` |
| 10 | State | `set` keys ∈ RuleSet `stateKeys` when declared; `item.state.<k>` references in `where` ∈ `stateKeys` (warning) | `CAT1000`–`CAT1001` |
| 11 | Workspace | at most one `rw` repo per TaskSpec; `=` refs only reference `inputs`/`task` | `CAT1100`–`CAT1101` |
| 12 | Overlap (warning) | two RuleSets select the same rule with overlapping scope for the same providers | `CAT1200` |

```go
type Error struct { Code, Message string; File string; Line int; Object ObjectKey; Severity Severity /* error|warning */ }
```

## `factory validate`

```text
factory validate [--dir catalog] [--git URL --ref REF] [--capabilities caps.json] [--known-models models.json] [--format text|json]
```

- Exit `0` no errors (warnings allowed), `1` errors, `2` usage/IO.
- Text output: `catalog/rules/release-docs/docs-on-release-train.yaml:7: CAT700 dispatch.inputs.train: required input missing`.
- Default capabilities/known models are embedded (from AGT adapters' static descriptors); flags override.

## Tests

- `testdata/poc/` = the [10](../../../10-poc.md) catalog → zero errors.
- One failing fixture per code above with golden error output.
- Determinism: loading the same tree twice yields equal `SHA` (dir hash over sorted file contents).
