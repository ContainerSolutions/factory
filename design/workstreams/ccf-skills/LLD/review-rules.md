# LLD — ccf-review rule packs for the factory and docs repos (SKL)

> Workstream: [SKL](../../definitions.md#skl-ccf-skills-bump) · Task: SKL-S3-T01 · Repo: `ccf-skills/ccf-review`
> Mechanism: `config/packs.json` (detection), `rules/<pack>/`, `rules/repo/<repo>/`, `config/contracts.json` (cross-repo edges)

## Detection (`config/packs.json`)

```json
"packs": {
  "docs-site": { "prefix": "DOCS", "when": [{ "file": "docusaurus.config.*" }] }
},
"repos": {
  "factory": { "prefix": "FACTORY" }
}
```

`vue-app` detection only checks the repo-root `package.json`, so add `{ "contains": { "web/package.json": "\"vue\"" } }` to `vue-app.when` for the factory's `web/` app.

## `docs-site` pack (initial rules)

| ID | Title | Severity |
|---|---|---|
| `DOCS-ACC-001` | Claim doesn't match the documented version (flag, default, endpoint, permission, UI path) | must-fix |
| `DOCS-VER-001` | Change edits a released version line other than the one being patched | must-fix |
| `DOCS-VER-002` | New minor/major release without a version cut, or a patch release that cuts a new version | must-fix |
| `DOCS-INST-001` | Repo/plugin not mapped in `instances.yaml`, or policy repo not mapped to its plugin | must-fix |
| `DOCS-STYLE-001` | Page doesn't follow the `STYLE.md` skeleton for its section type | should-fix |
| `DOCS-MEDIA-001` | GIF over size/length limits, not from the seeded stack, or prose where a GIF was expected for a UI flow | should-fix |
| `DOCS-LINK-001` | Broken internal link or link to an unversioned page from a versioned one | must-fix |
| `DOCS-I18N-001` | Translated page out of sync with its source (missing sections) or untranslated UI strings | should-fix |

## `repo/factory` pack (initial rules)

| ID | Title | Severity |
|---|---|---|
| `FACTORY-PURE-001` | I/O (network, store, clock) in a pure package (`internal/rules`, `cel`, `patterns`, `catalog`) | must-fix |
| `FACTORY-CRED-001` | A token/credential written to disk, logged, stored in the store, or an unscoped installation token | must-fix |
| `FACTORY-UNTRUST-001` | Untrusted provider text inlined into prompts/CEL activations instead of passed by reference/fenced | must-fix |
| `FACTORY-CONTRACT-001` | `api/openapi.yaml` or `internal/apis` changed together with consumers in the same PR, or generated code stale | must-fix |
| `FACTORY-EFFECT-001` | A GitHub write outside the Effector API / provider `Apply` (e.g. direct `gh`/REST write in agent code) | must-fix |
| `FACTORY-IDEM-001` | Effect or reconcile path not idempotent (no idempotency key, no re-read before act) | must-fix |
| `FACTORY-CAT-001` | Catalog change without `factory rules test` fixtures/golden updates, or `merge-*` rule enabled without two approvals noted | must-fix |
| `FACTORY-WEB-001` | Web code bypasses the generated API client or renders untrusted text with `v-html` | must-fix |

## Contract edges (`config/contracts.json`)

| id | producer → consumer | paths | expect |
|---|---|---|---|
| `factory-result-schemas` | factory → ccf-skills | `catalog/schemas/results/*` | matching change in `ccf-docs`/`ccf-factory` skills' result output |
| `docs-instances` | docs → ccf-skills | `instances.yaml`, `STYLE.md` | `docs-release`/`docs-style` still match |
| `api-openapi-docs` | api → docs | `docs/swagger.yaml` | handled by the factory's release-docs (informational) |
| `factory-api` | factory → factory/web | `api/openapi.yaml` | contract PR merged first; web regenerates client |

## Calibration

Add two frozen cases under `ccf-review/calibration/`: one factory Go PR (pure-package violation seeded) and one docs PR (wrong default seeded), and run `ccf-review:rules calibrate`.
