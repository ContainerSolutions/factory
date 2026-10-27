# Factory improvements: open work

The factory's remaining work, and nothing else. Done work, dropped items and the reasons behind
past changes are in [IMPROVEMENTS_DONE.md](IMPROVEMENTS_DONE.md). Both live on the
`improvements` branch, which is never merged to main.

> **We are in the closing phase (decided 2026-10-27).** Finish each workstream below; don't
> defer work into this file. A finding in a PR is either fixed in that PR (or the next one of
> its stack) or dropped, with the reason in the PR body. Add an item here only when Gustavo
> asks for it. **Other agents push to this file too:** `git pull` right before editing, keep
> edits to your own workstream's section, and push straight away.

How to work: read AGENTS.md first. PRs go through lisa as `ccf-lisa[bot]` with
`LISA_REPO=ContainerSolutions/factory` (the repo moved; lisa's App auth doesn't follow the old
name's redirect) and are reviewed with ccf-review. Gustavo merges. When a workstream ships,
record it in IMPROVEMENTS_DONE.md with its PR numbers and delete its section here.

## 1. Close P1 phase 10 (the Mac session)

Stacks 1 (#192–#195, merged) and 2 removed the host runtime. **Stack 2 is approved, waiting to
merge:** #198 → #201 → #203 → #209 (gh stack #210). Then one last stack on top of it, with no
deferrals:
- **PR A, catalog and code:** the catalog's docs TaskSpecs and `compile.go` stop setting
  `workspace.hosts`; drop `config.Harness.home`/`configDir` (and fix `agent.yaml.example`; the
  owner's `agent.yaml` files lose the keys before restarting supervise); the `harness.Isolation`
  type; the dead `clirun` helpers; the credentials step in gateway mode; the stale
  `RepoWritable` comment; `runStackPush`'s always-true assertion; and the doc leftovers
  (run-pipeline.md's old Tests paragraph and its duplicate "Environment" row, harness-adapters'
  historical decision text, `has-local-stack` examples in the design docs); `implement.md.tmpl`'s
  dead "use the factory's local stack" opening (the throwaway-environment steps stay).
- **PR B, contract:** remove `workspace.hosts` and `Task.spec.hosts` (v1alpha1, schemas,
  openapi, the web regen, v1alpha1's `FACTORY_STACK_*` reserved names, 07-security's allow-list
  text). Check first that the file store tolerates stored Tasks that still have `hosts`.

## 2. Real-host checks (Gustavo)

Shipped work that only fakes have tested. These are runs, not PRs; Gustavo runs them after
phase 10 merges, from a runbook the Mac session hands him. Record each result in
IMPROVEMENTS_DONE.md.

| # | What | How | Pass when |
|---|---|---|---|
| T1 | **ChatGPT refresh (#165)**, on the Mac or the workstation | **1.** Stop `supervise`, then `cp -p <state>/logins/codex.json{,.bak}`. **2.** Find the key paths without printing values: `jq -r 'paths(scalars)\|map(tostring)\|join(".")' <state>/logins/codex.json`. Set only the access token to an invalid string with `jq` (keep the refresh token), keeping mode 0600. **3.** Start `supervise` and redispatch any codex task. **4.** Afterwards run `codex login` on the host, because the old refresh token in `~/.codex/auth.json` is spent. | The task succeeds with no re-import. The run's egress log shows `chatgpt.com` 401, then the resent 200. `jq .lastRefresh` on the file shows a new time. `stat` shows 0600. The new access token is a JWT with `exp`. |
| T2 | **`yarn install` and `yarn build` in a VM (#172)** | After phase 10 lifts "never install": a release-docs or implement run on mock-docs. | The run's checks say `build: pass`, not `not-run`. Its transcript (the run page, or `GET /runs/<id>/transcript`) has `factory.egress` lines for `registry.yarnpkg.com` and `registry.npmjs.org`. |
| T3 | **The egress log records what the gateway forwards** | Any recent run's transcript in the UI, filtered for `factory.egress`. The VM's local copy doesn't have these lines: the host adds them. | The model host (`api.anthropic.com` or `chatgpt.com`), `github.com` and `api.github.com` appear, with the credential column filled and no token values. |
| T6 | **An agent-only change on kata** | Rebuild `factory-agent` and restart `supervise`. The kata driver puts the binary into each workspace at start (`AgentBinary`), so nothing needs to be rebuilt. | `host check` stays ok, and the next workspace runs the new binary. |

T4 and T5 passed (IMPROVEMENTS_DONE.md). T2 runs last, after stack 2 is merged.

Real-host checks from stack 3's PRs, which the PR bodies list (Gustavo, with sudo):
- **#202, workstation:** reinstall the protocol 7 helper (`host init`), then Ctrl-C during
  `katawarm` and during `kataimage`: no helper or nerdctl process is left, and the build dir is
  empty. A full `host init` also fixes `containerd: wrong` (`buildkitd.toml`, stale since #188).
- **#204, workstation:** answer N to the privileges prompt on a host without the helper.
- **#208, workstation:** a task with `limits.memory: 4Gi` gets a 4 GiB VM (`kata-helper
  inspect`).
- **Mac:** #197's pf half (a non-default `workspace.gatewayPort`), #199 (tart follows
  `workspace.size`), #205's tart path (a kind image loads offline on arm64).

Real-host checks from stack 4's PRs (#212–#217, merged 2026-10-27):
- **Workstation, one `host init`:** it reinstalls the helper (the installed one still answers
  protocol 6, from before #202) and rebuilds the kata image and warm disk (#217 changes the
  image). Then, in a workspace (`kata-helper exec`): `hostname; getent hosts "$(hostname)";
  sudo -n true` prints no "unable to resolve host" (#217).
- **#215, workstation:** Ctrl-C `supervise` while a workspace starts: `pgrep -af kata-helper`
  prints nothing. Optionally Ctrl-C at `host init`'s sudo password prompt: the terminal still
  echoes.
- **#214, workstation:** with the helper removed, `factory-agent host init --only kataimage`
  fails at once with the message and runs no sudo.
- **#216:** a `host init` with tart on the Mac (its archives are extracted through `execfile`),
  and any kata run here.
- **#217, Mac:** the PR's `tart exec` one-liner shows the tart guest's hostname resolves.

Not testable without a catalog change: #173 for an implement run under claude, because
implement allows only codex. The claude side of the `ccfr` lookup, the plugin-dir fallback, is
exercised by every docs-review run.
- **Mac, also:** Softnet's per-VM anti-spoofing, which the gateway's client pin relies on.

## 3. Apply `environment.egress` in the gateway

The gateway already implements deny-by-default, `AllowHosts` and the tunnel SNI check, but
nothing applies a TaskSpec's `environment.egress` yet. Wire the field from the Task through
the run bundle into the workspace's gateway policy. This is the network half of P1's hard
sandbox.

## 4. Phase 6: run tokens and enrollment

- the run token (#101's `Lease.runToken`) and the lease credential bundle;
- enrollment (#102), replacing the join token and the device flow. Decide how an agent's owner
  is attributed when no person logs in. Then remove the device flow and the join-token file.

Today the gateway injects the agent token on the dispatcher's effects route.

## 5. The org move to `ContainerSolutions/factory`

The Go module path is still `github.com/compliance-framework/factory`, and
`DefaultCatalogRepo` (`internal/controllers/catalogsource.go`), the POC files and the docs
still name the old org; clones work only through GitHub's redirect. ccf-review's default owner
is still `compliance-framework`, so pass it full `ContainerSolutions/factory#N` refs.
