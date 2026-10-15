# LLD — Docs onto the shared workflows (DWF)

> Workstream: [DWF](../../definitions.md#dwf-docs-onto-shared-workflows) · Tasks: DWF-S1-T01…T03, DWF-S2-T01…T04 · Repos: `workflows`, `mock-docs`, `docs`
> Conventions: `compliance-framework/workflows` README and `docs/{renovate,ccf-bump}.md`. **Release train excluded.**

## What "onboarded" means for docs

| Capability | How | Owner |
|---|---|---|
| Shared CI | `ci.yml` caller → `ci-docs.yml` (new) → ends in `ci / required` | workflows |
| Failure notifications | `notify-failure.yml` caller | workflows |
| Third-party deps | **Renovate** (central self-hosted run over the manifest; repo needs no onboarding PR; optional `renovate.json` overrides only) | workflows |
| Internal pins | **ccf-bump** moves shared-workflow pins (`uses: compliance-framework/workflows/...@<sha> # vX.Y.Z`); docs has no Go/image deps, so its bump PRs are `ci(deps)` (no release) | workflows |
| Repo settings | `repo-settings`: rulesets `ccf-review` (one approval) and `ccf-required` (requires `ci / required`), branch deletion on merge, etc. | workflows (`[human]` apply) |
| Release train | **no** — `release: false` | — |
| Pages deploy | stays repo-local `deploy-pages.yml`, its actions upgraded from `@v3` to current majors and SHA-pinned | docs |

## The caller workflow (in `docs` and `mock-docs`)

The check name `ci / required` comes from the caller's workflow `name` plus the job id that calls `ci-docs.yml`:

```yaml
# .github/workflows/ci.yml
name: ci                                   # → check names "ci / …"
on:
  pull_request: { types: [opened, edited, synchronize, reopened, labeled, unlabeled, closed] }
  push: { branches: [main] }
permissions: { contents: read }
jobs:
  ci:
    uses: compliance-framework/workflows/.github/workflows/ci-docs.yml@<sha> # vX.Y.Z
    permissions: { actions: read, contents: read, pull-requests: read, security-events: write }
```

The reusable workflow's aggregate job is `required`, so GitHub reports **`ci / required`** (the same convention as the other repos' callers; check the exact rendered name on the first PR and match the ruleset to it).

## workflows changes (DWF-S1-T01)

1. `internal/manifest`: add `KindDocs Kind = "docs"` to the kinds and `validKinds`; validation: `release` must be `false` for `docs` (error otherwise). Train planning ignores the kind (it already skips `release: false`; add a test).
2. `ci-docs.yml` (reusable):

   ```yaml
   name: ci-docs
   on:
     workflow_call:
       inputs:
         node-version: { type: string, default: "20" }      # used only without .nvmrc
         build-command: { type: string, default: "yarn build" }
         extra-command: { type: string, default: "" }       # e.g. link check / instances.yaml validation
   permissions: {}
   jobs:
     common:   # uses ./.github/workflows/ci-common.yml (same as ci-ui), same permissions block
     docs:     # checkout (persist-credentials: false) → setup-node (cache yarn) → yarn install --frozen-lockfile → build → extra-command
     required: # needs [common, docs]; if: always(); fails on any failure (same pattern as ci-ui)
   ```

3. `repo-settings`: map `docs` kind to the same ruleset template as `ui` (required check `ci / required`).
4. `ccf-bump`: `docs` kind → only shared-workflow pins (same code path as `workflows` pins; no module/image scanning).
5. Renovate preset: nothing kind-specific needed (npm manager covers `package.json`/`yarn.lock`); confirm the Docusaurus packages group into the non-major PR.
6. Docs: README kinds table, `docs/ccf-bump.md`, `docs/renovate.md` mention `docs`.

## Mock first (DWF-S1-T02, T03, S2-T01)

Following the workflows rule *"prove on the mocks before touching a product repo"*:

1. `repos.mock.yaml` += `{name: mock-docs, kind: docs, release: false}`.
2. `mock-docs` adopts `ci-docs.yml` (pinned to the DWF stack's top **branch name** while unreleased — never an unmerged SHA), SHA-pins `deploy-pages.yml`, deletes Dependabot config.
3. Dry runs: `ccf-bump sync --manifest repos.mock.yaml --repos mock-docs --dry-run`, Renovate `dryRun` over mock-docs, `repo-settings` dry run → then `[human]` apply and a `workflows` release.

## Product (DWF-S2-T02…T04)

1. `docs` adopts `ci-docs.yml` pinned `@<release sha> # vX.Y.Z`; `test-build.yml` deleted (it was push-only, check `build`); `deploy-pages.yml` actions upgraded and pinned; no `dependabot.yml` exists, and repo-level Dependabot security PRs stay disabled.
2. `repos.yaml` += `{name: docs, kind: docs, release: false}`.
3. `[human]` `repo-settings` apply for docs; branch protection now requires **`ci / required`** (the factory's merge rules use this name via the `requiredCheck` param — see [catalog-poc](../../catalog-poc/LLD/catalog-poc.md)).

## Done-when evidence (per PR body)

- `actionlint` clean; manifest tests; a green `ci / required` on the adopting PR.
- `ccf-bump list` output showing the repo; Renovate dry-run log line for the repo.
- Train plan output **without** docs/mock-docs.
