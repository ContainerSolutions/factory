# LLD — Harness config patterns and resolution (CORE)

> Workstream: [CORE](../../definitions.md#core-dispatcher-core) · Task: CORE-S1-T01 · Package: `internal/patterns` (pure)
> Model: [03 §Patterns](../../../03-domain-model.md#patterns-in-harness-config)

## Syntax

| Form | Example | Compiles to |
|---|---|---|
| exact | `claude-sonnet-5-5` | string equality |
| glob | `claude-sonnet-5-*`, `gpt-5?-codex` | `gobwas/glob` with no separators |
| glob alternation | `claude-{sonnet,opus}-5-*` | same (gobwas supports `{a,b}`) |
| regex | `re:^gpt-5(\.\d+)?-codex$` | `regexp.MustCompile` (anchored as written) |
| any | `*` | always true |
| list | `[low, medium]` (YAML list) | any element matches; **order = preference** |

```go
type Pattern interface { Match(s string) bool; String() string }
func Compile(v any) (Pattern, error)        // string or []string; returns typed error for bad glob/regex
```

## Inputs to resolution

```go
type Constraint struct { Harness string; Models Pattern; Effort Pattern }         // from TaskSpec.harness.allowed (one per harness)
type Request    struct { Harness string; Model Pattern; Effort Pattern; Prefer []string } // rule/ruleset/TaskSpec default
type Offer      struct { Harness string; Models []string; Efforts []string; DefaultModel string } // agent probe
type Resolved   struct { Harness, Model, Effort string }

func Resolve(cs []Constraint, req Request, off Offer) (Resolved, error) // ErrNoMatch with reason
```

## Algorithm

1. `req.Harness == off.Harness`, and some `c` in `cs` has `c.Harness == req.Harness`; else `ErrNoMatch{reason:"harness"}`.
2. `models = [m in off.Models | c.Models.Match(m) && req.Model.Match(m)]`; empty → `ErrNoMatch{reason:"model"}`.
3. Choose model:
   1. first of `req.Prefer` present in `models`;
   2. else `off.DefaultModel` if in `models`;
   3. else **highest version**: sort by `versionKey(m)` — extract trailing dotted/dashed numeric groups (`claude-sonnet-5-5` → `[5,5]`, `gpt-5.1-codex` → `[5,1]`), compare lexicographically, ties broken by string order.
4. Effort: `efforts = [e in off.Efforts | c.Effort.Match(e) && req.Effort.Match(e)]`; if `req.Effort` is a list, pick the first list element present in `efforts`; if `*`, pick the adapter default (`medium` if offered, else first offered); empty → `ErrNoMatch{reason:"effort"}`.
5. Return `Resolved`.

## Table tests (must exist)

| Constraint | Request | Offer | Result |
|---|---|---|---|
| `claude-*-5-*` | `claude-{sonnet,opus}-5-*` | `[sonnet-5-5, opus-5-5, haiku-5-5]` (+prefix) | `claude-opus-5-5` (highest; tie → string order) — unless default is sonnet → sonnet |
| `gpt-5*` | `gpt-5*` + `prefer:[gpt-5-codex]` | `[gpt-5-codex, gpt-5.1-codex]` | `gpt-5-codex` |
| `claude-{sonnet,haiku}-5-*` | `claude-opus-5-*` | any | `ErrNoMatch(model)` |
| effort `[low, medium]` | `high` | `[low, medium, high]` | `ErrNoMatch(effort)` |
| effort `*` | `*` | `[low, high]` | `low` (no medium → first offered) |
| harness `codex` only | `claude-code` | — | `ErrNoMatch(harness)` |
| `re:^gpt-5(\.\d+)?-codex$` | `*` | `[gpt-5-codex, gpt-5-mini]` | `gpt-5-codex` |

## Validation helpers (used by catalog validation)

- `Subsumes(constraint, request) (ok bool, witness string)` — best-effort static check: true if at least one string in the adapter's **known model list** (capabilities file) satisfies both; used to flag requests that can never fit their TaskSpec constraint.
