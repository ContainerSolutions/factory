# LLD — Harness config patterns and resolution (CORE)

> Workstream: [CORE](../../definitions.md#core-dispatcher-core) · Task: CORE-S1-T01 · Package: `internal/patterns` (pure)
> Model: [03 §Patterns](../../../03-domain-model.md#patterns-in-harness-config)

## Syntax

| Form | Example | Compiles to |
|---|---|---|
| exact | `claude-sonnet-5-5` | string equality |
| glob | `claude-sonnet-5-*`, `gpt-5?-codex` | an anchored RE2 regex: `*` → any run of characters (no separators), `?` → one character, `\` escapes the next character |
| glob alternation | `claude-{sonnet,opus}-5-*` | same: `{a,b}` → `(?:a\|b)`; alternatives may nest; unbalanced braces are a `PatternError` |
| regex | `re:^gpt-5(\.\d+)?-codex$` | `regexp.Compile` (anchored as written); a compile error is returned as a typed `PatternError`, never a panic |
| any | `*` | always true |
| list | `[low, medium]` (YAML list) | any element matches; **order = preference** |

```go
type Pattern interface { Match(s string) bool; String() string }
func Compile(v any) (Pattern, error)        // string, []string or v1alpha1.StringOrList; returns typed error for bad glob/regex
```

Globs use a small standard-library matcher: the glob is translated to an RE2 regex anchored at both ends, so it matches the whole string. Square brackets are literal: `[...]` is **not** a character class. Why not `gobwas/glob`: it would be an extra dependency for a few lines of code, and it is unmaintained. Matching is case-sensitive. Empty strings, empty lists and invalid UTF-8 are `PatternError`s too.

## Inputs to resolution

```go
type Constraint struct { Harness string; Models Pattern; Effort Pattern }         // from TaskSpec.harness.allowed (one per harness)
type Request    struct { Harness string; Model Pattern; Effort Pattern; Prefer []string } // rule/ruleset/TaskSpec default
type Offer      struct { Harness string; Models []string; Efforts []string; DefaultModel string } // agent probe
type Resolved   struct { Harness, Model, Effort string }

func Resolve(cs []Constraint, req Request, off Offer) (Resolved, error) // ErrNoMatch with reason
```

## Algorithm

1. `req.Harness == off.Harness`, and some `c` in `cs` has `c.Harness == req.Harness` (the first such `c` is used); else `ErrNoMatch{reason:"harness"}`. Empty `cs` is therefore `ErrNoMatch{reason:"harness"}`.
2. `models = [m in off.Models | c.Models.Match(m) && req.Model.Match(m)]`; empty → `ErrNoMatch{reason:"model"}`.
3. Choose model:
   1. first of `req.Prefer` present in `models` (`prefer` entries are exact ids, not patterns);
   2. else, if `req.Model` is a list, keep only the `models` that match its earliest element matching any of them (list order is preference);
   3. then `off.DefaultModel` if it is still in `models`;
   4. else **highest version**: `versionKey(m)` is every all-digit group delimited by `-`, `.` or the ends of the id (`claude-sonnet-5-5` → `[5,5]`, `gpt-5.1-codex` → `[5,1]`, `gpt-4o` → `[]`). Keys compare element by element, numerically (leading zeros ignored, no overflow), and a missing element sorts lowest (`gpt-5` < `gpt-5.1`). Ties go to the lexicographically smallest id.
4. Effort: `efforts = [e in off.Efforts | c.Effort.Match(e) && req.Effort.Match(e)]`, in offer order; empty → `ErrNoMatch{reason:"effort"}`, which includes an offer with no efforts. If `req.Effort` is a list, keep only the `efforts` matching its earliest element that matches any of them. Then pick the adapter default: `medium` if still in `efforts`, else the first of `efforts`. A single pattern (`*` or any other) goes straight to the default rule.

   Resolve works on **normalised** efforts (`low`, `medium`, …); the adapter maps the resolved value to the harness's native one through `AdapterCapabilities.EffortMapping`.
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

- `Subsumes(c Constraint, req Request, known []string) (ok bool, witness string)` — best-effort static check: true if at least one string in the adapter's **known model list** satisfies both, returned as `witness`; used to flag requests that can never fit their TaskSpec constraint. `known` is the known model ids, from the adapter capabilities / `KnownModels` (`factory validate --known-models`).
- `Validate(v any) error` — `Compile` for callers that only check the catalog.
- Duplicate harness names in one `TaskSpec.harness.allowed` are a catalog validation error ([catalog.md](catalog.md#validation-phases-all-errors-collected-not-fail-fast), phase 8), since Resolve only uses the first.
