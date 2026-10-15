# LLD — `ccf-docs` plugin: docs-style, docs-release, docs-gif, docs-verify (SKL)

> Workstream: [SKL](../../definitions.md#skl-ccf-skills-bump) · Tasks: SKL-S2-T01…T03 · Repo: `ccf-skills`, new plugin dir `ccf-docs/`
> Product rules: [10 §Versioned docs per repository](../../../10-poc.md#versioned-docs-per-repository) · Docs site: [docs-revamp LLD](../../docs-revamp/LLD/docs-revamp.md)

## Packaging

```text
ccf-docs/
├── .claude-plugin/plugin.json           # name ccf-docs, version 0.8.0
├── skills/
│   ├── docs-style/SKILL.md   (+ references/style-checklist.md)
│   ├── docs-release/SKILL.md (+ scripts/train.py, scripts/diff_config.py, scripts/cut_version.sh, references/sections.md)
│   ├── docs-gif/SKILL.md     (+ scripts/record_ui.ts, scripts/to_gif.sh, tapes/*.tape)
│   └── docs-verify/SKILL.md  (+ scripts/claims.py)
└── README.md
```

Marketplace entry added to `.claude-plugin/marketplace.json`. Every skill is **headless-capable** (HEADLESS.md) and Codex-portable; deterministic work lives in scripts (Python 3 stdlib only, Node for Playwright).

## docs-style

Reads the docs repo's `STYLE.md` and applies it: voice, page skeletons per section type (release notes, config reference, guide, upgrade), "prefer a GIF over a paragraph", GIF limits, link rules. Exposes `references/style-checklist.md` used by `docs-release` (self-check) and `docs-review` (findings).

## docs-release

**Input:** `train` ref (or `repo@tag`), `depthPolicy`. **Output:** one branch + one PR in the docs repo; result `release-docs` schema.

```text
1. scripts/train.py <train-ref>          → repos[]{repo, from (previous final), to, bump} (reads <!-- train-state --> JSON; previous final via GitHub releases)
2. for each repo in train order:
     inst := instances.yaml lookup (repo → instance id/kind; policy repos → owning plugin instance)
     depth := depthPolicy[bump]
     a. Release notes   — from GitHub release body + train digest; highlights first      (all)
     b. API reference   — copy <repo>/docs/swagger.yaml@to → openapi/<inst>/<minor>.yaml (rendered at build; nothing generated)  (api; if changed)
     c. Config reference— scripts/diff_config.py <repo> from..to → new/changed CCF_* with defaults   (full)
     d. Feature guides  — code diff + authz-roles diff → guide pages; call docs-gif for flows        (full)
     e. Upgrade guide   — breaking changes, renamed flags, migrations                          (full, minor/major)
     f. Catalog rows    — plugin config/evidence; policies catalog row (bundle, version, controls, compatible lines) (plugins/policies)
     g. Version         — new minor/major: scripts/cut_version.sh <inst> <major.minor> (docusaurus docs:version:<inst>); patch: edit the line in place
3. docs-style self-check; yarn build must pass
4. factory push; factory pr open (title "docs: release train <month>", body = per-repo summary table)
5. result JSON
```

Rules: never touch other instances' released lines except the patched minor line; plugin instances missing in `instances.yaml` are added (new plugin onboarding); policy repos map to their plugin via `instances.yaml`.

## docs-gif

```text
record UI:  scripts/record_ui.ts --flow flows/<name>.ts --base $FACTORY_STACK_URL --out out/<name>.webm   (Playwright, 1280x800, seeded showcase login)
convert:    scripts/to_gif.sh out/<name>.webm static/img/<inst>/<line>/<name>.gif   (ffmpeg → frames → gifski, fps 12, ≤ 4 MB, ≤ 20 s; retries with lower fps/width)
CLI:        vhs tapes/<name>.tape → gif (same limits)
```

Flow files are written by the agent per feature (small Playwright scripts: navigate, click, type, wait); the skill provides helpers (`login()`, `goto(path)`, `highlight(selector)`).

## docs-verify

**Input:** a docs diff (PR) + `verifyAgainst` (`{train}` or `{repo, tag}`) + running stack. **Output:** per-claim verdicts → review threads.

| Claim type | Extracted from | Verified against |
|---|---|---|
| env var / config key + default | config reference tables, code blocks | `diff_config.py` at the tag; and the running container's effective config where exposed |
| endpoint + method + params | API guide text, examples | OpenAPI at the tag; a live call on the stack (read-only endpoints) |
| permission / role | guide text | `authz-roles.yaml` at the tag |
| UI path / screen | guide text, GIFs | stack: navigate the path (Playwright) and screenshot |
| CLI flag | code blocks | `--help` of the binary at the tag (container) |

Each failed claim → `{file, line, claim, expected, evidence}` → returned to `ccf-review` as must-fix findings (rule `DOCS-ACC-001`, see review-rules LLD).
