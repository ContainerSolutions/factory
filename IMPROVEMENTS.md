# Factory improvements: open work

Everything the factory still needs, and nothing else. Done work and the reasons behind past
changes are in [IMPROVEMENTS_DONE.md](IMPROVEMENTS_DONE.md). Both live on the `improvements` branch, which is never merged to main; pull it before planning, and push updates to it.

Each item becomes its own PRs, through lisa and reviewed with ccf-review. **Before starting a
phase or group,** say what you'll design, what's in and what goes back here as deferred, and
wait for Gustavo.

## Now

### Order (decided 2026-10-26)

Three PR stacks, in this order:

| Stack | Where | Contents | Starts |
|---|---|---|---|
| **1. Common phase 10** | the Mac session | The phase 10 work that touches files the phase 9 follow-ups also touch: **(1)** remove `worker` and `--runtime host` from `cmd/factory-agent` (supervise is the only runtime); **(2)** remove the `localDev` and label config from `internal/agent/config`; **(3)** the LLD renames (worker → supervise, run-pipeline, harness-adapters, local-stack → environment). Plus **(4)** lifting the "never install" rule and the `not-run` stopgap (#85) in the prompts: not shared code, but test T2 needs it. | **merged 2026-10-26** (#192–#195, ccf-skills#25; IMPROVEMENTS_DONE.md) |
| **2. Remaining phase 10** | the Mac session | Everything else in "2. P1 phase 10: removal" below. | **approved, waiting to merge:** #198 → #201 → #203 → #209 (stack #210) |
| **3. Phase 9 follow-ups** | **the Linux box** | Fixes a–h and tests T1–T6 in "1. Phase 9 follow-ups" below. | **merged 2026-10-27** (#197–#208; IMPROVEMENTS_DONE.md). Stack 4 closes what it left (below). |

Notes for the Linux box agent:
- **Read AGENTS.md first.** Use lisa as `ccf-lisa[bot]`, review with ccf-review, and keep one
  stack. The repo is now **`ContainerSolutions/factory`**: set `LISA_REPO=ContainerSolutions/factory`
  (lisa's App auth doesn't follow the old name's redirect); the App is installed there. Before starting, tell Gustavo the design, what's in, and what goes back here as
  deferred, then wait.
- **Start from main after stack 1.** `internal/agent/config` and `cmd/factory-agent` have
  changed: `localDev` and `worker` are gone. The config decode is strict, so delete any
  `localDev` key from the Linux box's `agent.yaml` before restarting supervise.
- **The tart-only parts can't be verified on Linux.** That's fix b and fix a's pf-anchor half.
  Write them against the existing fake tart, and leave the real check to Gustavo on the Mac
  (say so in the PR body).
- **T1 and T3 run on either machine.** T2 runs after stack 2. T6
  are Linux only.
- **When an item ships,** record it in IMPROVEMENTS_DONE.md with its PR numbers, and remove it
  here.

Stack 1 status (2026-10-26): **merged.** The PRs are listed in IMPROVEMENTS_DONE.md; what it
left for stack 2 is under "Left over from stack 1, for stack 2" below.

Stack 2 status (2026-10-27): **all 4 PRs approved** with ccf-review (no blocking findings, two
Nits deferred), CI green, pushed as `ccf-lisa[bot]`. Merge bottom-up: #198 → #201 → #203 → #209.
What it leaves is under "2. P1 phase 10: removal" below. It overlaps stack 3 only in
`internal/agent/workspace` tests and `vmrun`; whichever merges second rebases.

Stack 3 status (2026-10-27): **merged** (#197–#208, in IMPROVEMENTS_DONE.md with T4 and T5's
results).

Stack 4 status (2026-10-27): **in progress.** It closes everything stack 3 deferred, as
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

### 1. Phase 9 follow-ups

Fixes a–h shipped in stack 3 (IMPROVEMENTS_DONE.md). The kata LLD's risks stay as they are
(`design/workstreams/agent/LLD/workspace-kata.md`, "Risks and open questions"):
- Docker's ~5 s embedded-DNS wait;
- the 1.7 GB image, rebuilt on every `base.sh` change;
- `no-aaaa` covering glibc lookups only.

#### Tests on real workspaces

Shipped work that has only been tested against fakes. The owner runs these: they need tart, a
codex login or the Linux machine. Each records its result here, then moves to
IMPROVEMENTS_DONE.md.

Already confirmed from the Mac's run records (2026-10-26), now in IMPROVEMENTS_DONE.md:
- #162 on tart;
- #173 under codex;
- #172's corepack download;
- claude-code working with `NODE_USE_ENV_PROXY`.

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

### 2. P1 phase 10: removal

Stack 1 (#192–#195, merged) and stack 2 (#198, #201, #203, #209) remove the host runtime:
`worker`, `localDev` and the label probes, the local-stack step, the adapters' host sandboxes
and probes, `make smoke-*`, `gitsafe`, the host side of `Pipeline.Run` and the Effector's host
runtime. Kept on purpose: gitcred and `fx git-credential`, because supervise's `PrepareBundle`
fetches private skills on the host with them.

Left after stack 2:
- **`workspace.hosts` and `Task.spec.hosts`** (replaced by `environment.egress`; now ignored):
  a contract removal (v1alpha1, schemas, openapi, the web regen), after the catalog's docs
  TaskSpecs and `compile.go` stop setting it. Check first whether the file store strictly
  decodes stored Tasks with `hosts`. 07-security's allow-list text goes with it.
- **`config.Harness.home`/`configDir`:** unused since stack 2, but the config decode is strict
  and existing `agent.yaml` files (and `deploy/poc/agent.yaml.example`) set them. Drop with a
  warning period, or keep.
- **The device flow and the join-token file:** need enrollment first (phase 6, under Later).
- **The #88 temporary-API/UI stopgap** in `implement.md.tmpl`: stays until implement gets an
  environment.
- **The MCP approval key** in codex's VM config: `danger-full-access` with `never` should
  auto-approve without it, but no VM run has checked. Try a codex run without it.
- **Code leftovers:** the `harness.Isolation` type (only VM remains); the credentials step still
  runs in gateway mode; dead `clirun` helpers; a stale `RepoWritable` comment; the Effector's
  `runStackPush` type assertion that's now always true; v1alpha1's `FACTORY_STACK_*` reserved
  names.
- **Doc leftovers:** `base.sh` line 135 names `worker.md` (edit it with the next `base.sh`
  change: any edit rebuilds the base VM); run-pipeline.md's old Tests paragraph and its two
  "Environment" rows; harness-adapters.md's historical decision text; `has-local-stack` examples
  in design docs (and in `api/examples` and the web mocks, where they're arbitrary labels and can
  stay); LOCAL-GUIDE, which lives in local-dev.
- **`ccf-review`'s `verify.md` says "`npm ci` (ask first)":** under HEADLESS.md rule 9 headless
  reviews could run it without asking. A ccf-skills change.

## Later

### Deferred from group 1 (#162–#173)
- **Replay (#164):**
  - A replay of an event that hasn't been processed yet can race its first reconcile, which
    checks the processed mark before taking the lock. That duplicates decisions, not effects.
  - A replay of an event processed before #164 that left no records on its item is refused
    (409 `replay-history-gone`), because its mark has no record count.
  - A rule that fired but was suppressed by a rail at the time stays unapplied on replay.
  - The web's Replay button (#170) should show the 409 `replay-history-gone` problem in plain
    words.
- **Close and delete (#166):**
  - Close emits no `factory.command.close` event.
  - Delete lists every Event, EventStatus, Effect and Debounce, since none is indexed by work
    item.
  - Delete leaves the item's kill switch, approvals and its `wi/<id>` lock record.
  - A forced delete removes a running task from under its agent, which learns only at its next
    heartbeat.
- **App installations (#167):**
  - Only literal repos are checked: glob scopes name none.
  - The check after a catalog swap can run before the provider reconnects.
  - A failed check shows no gaps until the next swap or restart; there's no periodic re-check.
- **Environment log (#169):**
  - It's collected right after the outcome event, so `down`'s output can be missing.
  - It shares the transcripts' total cap, so a large log can evict an old transcript.
- **ChatGPT refresh (#165):**
  - No runner-level test of the `ChatGPTUnauthorized` wiring.
  - Nothing reads the refresher's `Err()`: supervise should stop leasing codex tasks on a
    permanent failure.
  - A re-import that lands between a refresh's POST and its save is overwritten.
  - Go's default redirect following would resend the refresh body on a 307/308 from the
    issuer.
  - After a gateway refresh, the owner's own `~/.codex/auth.json` holds a spent refresh token,
    so codex on the Mac needs `codex login` again.
- **Node and `NO_PROXY` (#172):** Node's `NO_PROXY` has no CIDR form, so the private ranges in
  `DefaultNoProxy` don't match Node clients. A Node client that reaches a private address by IP
  now goes through the gateway, which denies it. Add Node's range form if one needs to.
- **lisa's self-review finds `ccfr` by a relative path (#173):** codex now links the plugin's
  `bin/`. In ccf-skills, lisa's reviewer role could take `ccfr` from `PATH`, as HEADLESS.md asks
  headless skills to.
- **Flaky tests (#168):**
  - controllers `TestCascadeRunningControllers` (`rule_test.go:79`, "timed out waiting for the
    run" after 10 s, seen on #162's CI), which #168 doesn't cover;
  - claudecode/codex fake scripts are still written with a plain `os.WriteFile` (possible
    ETXTBSY on Linux): stack 4's PR 4.
  - Also seen: pipeline `TestRunEnvironmentWallClockLimit` under load ("the environment didn't
    start" at 32 s). #168 widened its bound; watch it.

### P1 leftovers
- **Phase 6, run tokens and enrollment:**
  - the run token (#101's `Lease.runToken`) and the lease credential bundle;
  - enrollment (#102) replacing the join token and device flow. Decide how an agent's owner is
    attributed when no person logs in.

  Today the gateway injects the agent token on the dispatcher's effects route.
- **Wire `environment.egress` into the gateway.** The gateway already implements
  deny-by-default, `AllowHosts` and the tunnel SNI check; nothing applies the TaskSpec's field
  yet.
- **Gateway:**
  - the CA lifecycle: a `gatewayca` host step and `host init --rotate-ca`;
  - a conformance suite shared by the fake and real gateways (`gatewaycases`);
  - limits: an egress log cap, idle timeouts on tunnels and WebSockets;
  - `gateway.md` §Delivery is stale.
- **Gateway, accepted gaps:**
  - the VM's DNS goes through the host resolver;
  - the model-path allow-lists (`policy/models.go`) refuse unknown harness calls with
    `model-path`. Add paths as they appear.
- **Supervise:**
  - a readiness signal from the gateway's `Serve`;
  - probe the base VM's harness binaries instead of trusting stored logins;
  - probe labels instead of advertising them from config;
  - `host check` honouring `--state-dir`;
  - rebuild the warm base automatically when `warm.images` changes;
  - `lockedWriter` is duplicated between `host` and the tart driver.
- **envrun:**
  - a first SIGTERM during `down` doesn't shorten it;
  - processes left by `up` keep writing to the log;
  - the process-group helpers are duplicated between envrun and clirun.
- **`FACTORY_RUNTIME` isn't a reserved env name.** That's a contract change.
- **Unverified on the Mac:** Softnet's per-VM anti-spoofing, which the gateway's client pin
  relies on, and the pf anchor under a real `host init`.

### Catalog, skills and run quality
- **UC2 accuracy:**
  - nothing tells implement or plan-epic to take facts from the documented component's source
    at its release, so a clone or `gh api` read relies on the agent;
  - the epic review (`checks: [i18n, versioning, style, build]`) never checks facts against the
    source.

  The fix is prompt only: a source-only accuracy check for `docs-review` without
  `verifyAgainst`.
- **Version sources `latest` and `train`** in the compiler. Only `from: inputs` resolves today.
  The train rule builds the map from `event.train.repos[].versions['.']` and drops a
  multi-module repo's other keys.
- **`implement` has no environment,** so a review fix that needs a new GIF or screenshot uses the
  #88 temporary-environment stopgap.
- **local-dev's Phase 0 issues** (the showcase `build` and the `create-user` race) are fixed in
  local-dev's own repo, so `up` can drop its retry.
- **slate-agent's plugins in the VM:** unverified that the agent container can download an
  overlay's plugin from ghcr.io (#140's open question).
- **Reopening a closed item:** `lifecycle: closed` is permanent, and there's no
  `issue.reopened` event type.
- **A single-PR epic whose PR says "Closes #N"** closes the item on merge, and `closed-gate` then
  suppresses `record-merge-outcome`/`announce-ptbr-live`. No effect while `merge-whole-stack` is
  excluded.
- **The versions map isn't capped:** a train with more than 50 released repos fails the
  release-docs dispatch.
- **The environment block is duplicated** in release-docs and docs-review-verify;
  `catalog_test.go` keeps them equal.

### Dispatcher and rules
- **A push to an already-open, unlinked PR is dropped:** `pr.updated` fires before the link
  exists. Resolve it through the PR body's `factory:effect` marker.
- **Stale `needs-human` labels** are never removed. A rule could clear it when a later run
  succeeds.

### UI
- **Pending reasons with an explanation and an ETA,** for example "anti-affinity: leases to your
  agent at 08:32". The `git-credential` reason is group 1 (f).
- **A failed run leads with its Result summary,** above the transcript.
- **The run's workspace on the run page:** the VM name, the harness and model, the resolved
  `spec.versions`, the environment's exposed URLs, and `environment.log` (group 1, c).
- **Inject an event from the UI,** with the dry-run preview first (decisions, targets, and a
  warning when a target is the prod docs repo). Today it's `curl … | jq` and `factory dispatch
  --event`.
- **Graph:**
  - notify comments have no Effect record, so they get no exact link (match their
    `evt/<event>/<rule>/<i>` key);
  - notify, set, cancel and link show only their rail verdict;
  - approvals are matched to `requestApproval` in order, because ApprovalRef has no event or
    rule.
- **Smaller UI items:**
  - `api/examples/*.json` still use bare kinds in `Decision.actions` (a contract PR);
  - a RuleSet's picked rule isn't kept in the URL;
  - check how a `lifecycle: closed` card reads on its work-item page.

### Harness adapters
- **Smoke tests should use a real TaskSpec schema** (review.json): `make smoke-claude` once
  passed while every real claude task failed.

### Housekeeping
- **The org move to `ContainerSolutions/factory`:** the Go module path is still
  `github.com/compliance-framework/factory`, and `DefaultCatalogRepo`
  (`internal/controllers/catalogsource.go`), the POC files and the docs still name the old org;
  clones work only through GitHub's redirect. ccf-review's default owner is still
  `compliance-framework`, so pass it full `ContainerSolutions/factory#N` refs.
- **Deferred review findings** (should-fix and nits) are in each PR's
  `~/.ccf-review/sessions/compliance-framework-factory-<N>/triaged.json`:
  - #118: test gaps in the gateway policy;
  - #121: `Load` doesn't check the CA's name constraints against the intercepted hosts;
  - #123: `%p` on an `AnthropicAuth`/`ChatGPTLogin` value prints the token.
- **`refs/notes/commits`** has 156 notes commits under the owner's identity, from before the bot
  identity rule. Rewriting them is the owner's call.
- **Review rule candidates** wait in `~/.ccf-review/proposals.jsonl` for Gustavo's
  `/ccf-review:rules propose`.
- **`host/provision`'s `TestBaseScriptRuns` fails on macOS** (no `/etc/os-release`), so a local
  `make test` on the Mac always has one failure; CI runs it on Linux. Skip it off Linux.
- **The installed ccf-review plugin is stale** (`ccfr version`: 0.2.0 at 992d25d, behind
  ccf-skills main); stack 2 was reviewed with it. Update the plugin.
