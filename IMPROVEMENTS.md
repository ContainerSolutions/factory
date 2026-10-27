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
  historical decision text, `has-local-stack` examples in the design docs).
- **PR B, contract:** remove `workspace.hosts` and `Task.spec.hosts` (v1alpha1, schemas,
  openapi, the web regen, v1alpha1's `FACTORY_STACK_*` reserved names, 07-security's allow-list
  text). Check first that the file store tolerates stored Tasks that still have `hosts`.

## 2. Phase 9 follow-ups, stack 4 (the Linux box)

**In progress (the Linux box session owns it).** It closes everything stack 3 deferred, as
Gustavo decided on 2026-10-27. Six PRs on main as `ccf-lisa[bot]`, independent of each other,
each reviewed with ccf-review:

| PR | Branch | What |
|---|---|---|
| 1 | `fx/p9c-1-sudo-sigterm` | every sudo factory-agent runs (the kata driver's helper calls in supervise, host init's root script, `vmrun`) gets SIGTERM before SIGKILL, as #202 did for host init's helper calls |
| 2 | `fx/p9c-2-helper-required` | `host init` fails up front, changing nothing, when a selected step needs the kata helper and `privileges` isn't ok and isn't selected (`--only kataimage`) |
| 3 | `fx/p9c-3-workspace-hostname` | a workspace's hostname resolves: no "sudo: unable to resolve host ws-tsk…" |
| 4 | `fx/p9c-4-harness-etxtbsy` | the claudecode/codex fake scripts are written under `syscall.ForkLock`, as pipeline's are: no more "text file busy" |
| 5 | `fx/p9c-5-limits-docs` | contract docs: `limits.cpus`/`limits.memory` are hard-capped at `workspace.size`, and small values are taken as given |
| 6 | `fx/p9c-6-web-limits` | the web catalog shows `cpus` and `memory` limits |

Dropped by decision, with no PR:
- a build-recipe version in the warm records: not needed while Gustavo is the only user;
- making the advisory `e2e container` job required: it stays advisory, since a container can't
  prove tart or kata. Its log upload on a timeout is skipped too;
- a memory budget beyond `workspace.size`: limits are hard-capped by design, and a bigger task
  runs on a host whose `workspace.size` is bigger.

## 3. Real-host checks (Gustavo)

Shipped work that only fakes have tested. These are runs, not PRs; record each result in
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

Not testable without a catalog change: #173 for an implement run under claude, because
implement allows only codex. The claude side of the `ccfr` lookup, the plugin-dir fallback, is
exercised by every docs-review run.
- **Mac, also:** Softnet's per-VM anti-spoofing, which the gateway's client pin relies on.

## 4. Apply `environment.egress` in the gateway

The gateway already implements deny-by-default, `AllowHosts` and the tunnel SNI check, but
nothing applies a TaskSpec's `environment.egress` yet. Wire the field from the Task through
the run bundle into the workspace's gateway policy. This is the network half of P1's hard
sandbox.

## 5. Give `implement` an environment (replaces the #88 stopgap)

`implement` declares no environment, so a UC2 run that needs screenshots or GIFs follows #88's
prompt bullets: clone `api` and `ui`, run them on random ports with fake data, take the shots
with Playwright, and tear everything down. That path is slow and fragile, and isn't local-dev's
seeded showcase. The prompt's first line (use the local stack when `FACTORY_STACK_URL` is set)
is dead since phase 10 removed the local stack.
- `catalog/taskspecs/implement.yaml` gets the environment block `docs-review-verify` has
  (local-dev's clean slate, local-dev as a read repo), with `versions: { from: latest }`: the
  compiler resolves nothing for it (`compile.go:569`), so local-dev's own pins apply. Keep the
  three blocks equal in `catalog_test.go`.
- `implement.md.tmpl`'s #88 bullets become "the environment is up at its URLs".
- The author App needs an installation on `compliance-framework/local-dev` (private).
- Decide: every implement run would then start the stack (minutes), even when it needs no
  screenshots. Environments can't be conditional today.

## 6. Phase 6: run tokens and enrollment

- the run token (#101's `Lease.runToken`) and the lease credential bundle;
- enrollment (#102), replacing the join token and the device flow. Decide how an agent's owner
  is attributed when no person logs in. Then remove the device flow and the join-token file.

Today the gateway injects the agent token on the dispatcher's effects route.

## 7. The org move to `ContainerSolutions/factory`

The Go module path is still `github.com/compliance-framework/factory`, and
`DefaultCatalogRepo` (`internal/controllers/catalogsource.go`), the POC files and the docs
still name the old org; clones work only through GitHub's redirect. ccf-review's default owner
is still `compliance-framework`, so pass it full `ContainerSolutions/factory#N` refs.
