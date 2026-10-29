# The reference catalog

This directory is the factory's **reference catalog**: the factory's own tests and fixtures use it, and it is the example to copy when you start a catalog of your own. No dispatcher deploys it: `factory serve` has no default catalog and needs `--catalog-repo` or `--catalog-dir`.

Its prod RuleSets (`rulesets/poc-*.yaml`) name compliance-framework repositories as examples, and its Providers carry placeholder App IDs. Don't point a dispatcher with real GitHub Apps at it: copy it first.

## What is checked

- `make catalog` (part of `make reviewable`, so CI) runs `factory validate --dir catalog` (the schema and the cross-references) and `factory rules test --catalog catalog --coverage`: each case under `testdata/` replays its event against the rules and compares the decisions with `expected.json`, renders the prompts and Templates its decisions use against the goldens in `rendered/`, and `--coverage` fails on a selected rule no case fires. `go run ./cmd/factory rules test --catalog catalog --update` rewrites the goldens.
- `catalog_test.go` (`make test`) pins what the reference itself promises: every object is there, the dev RuleSets never target `compliance-framework/docs`, every prompt and Template renders, the result schemas accept what the skills write, and untrusted text in the fixtures stays fenced.

## Start a catalog of your own

1. Copy this directory into a repository of yours (as `catalog/`, or set `--catalog-path`). Leave `catalog_test.go` behind: it tests the reference, and it needs this repository's Go module.
2. Set the real App IDs in `providers/*.yaml`, and the RuleSets' scopes and targets to your repositories.
3. In that repository's CI, with a pinned `factory` release (the `factory-<os>-<arch>` assets, `deploy/poc/README.md` §12), run `factory validate --dir catalog` and `factory rules test --catalog catalog --coverage`.
4. Start the dispatcher on it: `factory serve --catalog-repo https://github.com/<owner>/<repo>`, plus `--catalog-app-id <App ID> --catalog-app-key-ref <name>/<key>` when the repository is private (`deploy/poc/README.md` §4).
