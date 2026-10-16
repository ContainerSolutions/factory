# CCF Factory

An event-driven, LLM-powered software factory for the [compliance-framework](https://github.com/compliance-framework) org, starting with the docs.

GitHub events (a finished release train, an issue, a pull request) go to a central **dispatcher**, where declarative **rules** decide which **task spec** runs, on which **harness** (Codex or Claude Code, loaded with [`ccf-skills`](https://github.com/compliance-framework/ccf-skills)) and on which **pool** of agents.

- **Design:** [`design/`](design/README.md), starting with [10 — Proof of concept](design/10-poc.md).
- **Work plan:** [`design/workstreams/definitions.md`](design/workstreams/definitions.md). Tasks are built with `lisa` and reviewed with `ccf-review`.

## Layout

| Path | What |
|---|---|
| `cmd/factory` | The dispatcher binary |
| `cmd/factory-agent` | The agent binary (registered workers) |
| `internal/` | Go packages (see [repo-layout LLD](design/workstreams/foundation/LLD/repo-layout.md)) |
| `web/` | The Factory UI (Vue), built separately and embedded with `go:embed` |
| `catalog/` | The factory's configuration: providers, rules, task specs, queues, pools |
| `deploy/poc/` | PoC run kit (secrets live in the gitignored `deploy/poc/.env`) |

## Build

```sh
make build           # bin/factory, bin/factory-agent with a placeholder UI (no Node needed)
make test            # go test -race
make reviewable      # what CI runs for Go
make build-release   # builds web/ and embeds it (-tags webui); needs web/ (WEB-S0-T01)
```

Requires Go 1.26 and golangci-lint v2. The UI needs Node 20. CI runs `ci / required` (shared `compliance-framework/workflows` Go CI plus a local web job).

## Licence

[AGPL-3.0](LICENSE).
