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
| **2. Remaining phase 10** | the Mac session | Everything else in "2. P1 phase 10: removal" below. | **next:** stack 1 is merged; its design goes to Gustavo first |
| **3. Phase 9 follow-ups** | **the Linux box** | Fixes a–h and tests T1–T6 in "1. Phase 9 follow-ups" below. | **now:** stack 1 is merged (`git log origin/main` has #192–#195) |

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
- **T1 and T3 run on either machine.** T2 can run now: stack 1 lifted "never install". T4, T5 and T6
  are Linux only.
- **When an item ships,** record it in IMPROVEMENTS_DONE.md with its PR numbers, and remove it
  here.

Stack 1 status (2026-10-26): **merged.** The PRs are listed in IMPROVEMENTS_DONE.md; what it
left for stack 2 is under "Left over from stack 1, for stack 2" below.

Stack 3 status (2026-10-26): **design agreed with Gustavo; starting.** One stack on main,
`fx/p9f-<n>-<slug>`, as `ccf-lisa[bot]`:

| PR | Branch | Fix |
|---|---|---|
| 1 | `fx/p9f-1-gateway-port` | a: `RunnerOptions.GatewayPort` from `workspace.gatewayPort` |
| 2 | `fx/p9f-2-tart-size` | b: tart's `Spec` from `workspace.size` (real check on the Mac) |
| 3 | `fx/p9f-3-sighup-memory` | c: SIGHUP re-reads `/proc/meminfo` and recomputes the cap |
| 4 | `fx/p9f-4-sudo-signals` | d: `sudo -n` in its own process group, SIGTERM then grace; the helper handles signals for every verb and kills nerdctl's group; stale build contexts swept; a host-side bound on `image-build` |
| 5 | `fx/p9f-5-declined-privileges` | e: kataimage/katawarm skipped with a warning; asked again on the next init |
| 6 | `fx/p9f-6-kind-archives` | f: T4 failed (below): the warm build saves each kind image with `docker save --platform linux/<arch>` and makes the archive readable (0644), on kata and tart |
| 7 | `fx/p9f-7-e2e-container` | h: T5 passed (below); add it to CI as a job that isn't required |
| 8 | `fx/p9f-8-limits-size` | g, contract: `Limits.cpus` and `Limits.memory` |
| 9 | `fx/p9f-9-task-size` | g: the runner sizes each VM from the task's limits, capped at `workspace.size` with a warning |

T2 runs last, after stack 2 is merged.

Results on the workstation (2026-10-27, main at #195):
- **T5 passed** on its first run: `make e2e-run-container`, claude-code and codex, 0.5 s.
- **T4 failed**, with the warm disk built from `busybox:1.37.0@sha256:bdf57e52…` and
  `kindest/node:v1.35.0`. The digest-to-tag step works: Docker 29's containerd store loads the
  image as `busybox:1.37.0@sha256:…`, and `docker tag` and `docker save` succeed, with
  `RepoTags: ["busybox:1.37.0"]`. Two other things break `kind load image-archive`:
  - the archive is `root:root 0600`, so `up`, which runs as `admin`, gets "permission denied";
  - the archive's index names every platform but holds only amd64's content, so kind's
    `ctr import` fails ("content digest … not found") even as root, and the pod then pulls.

  `docker save --platform linux/amd64` with `chmod 0644` fixes both: the pod started offline
  ("already present on machine"). The tart path saves the same way, so it likely has both
  problems on arm64.
- Also seen: every `sudo` in a workspace prints "unable to resolve host
  ws-tskfactory-warm-build": the guest's hostname isn't in its `/etc/hosts`.

### 1. Phase 9 follow-ups

#### Fixes

Found while building and running the kata track (#174–#189):

| # | Fix | Notes |
|---|---|---|
| a | **The gateway port is hard-coded** | The runner listens on 3128 (`workspace.GatewayPort`, `runner.go`), while the pf anchor (tart) and the nft table (kata) open `workspace.gatewayPort`. Any other port opens the wrong one. Pass the config's port to the runner. Broken on tart since phase 5. |
| b | **tart doesn't follow `workspace.size`** | supervise gives tart `Spec{}` (6 vCPUs, 8 GiB) whatever the config says. kata uses `workspace.size` (#187). |
| c | **supervise doesn't re-read physical memory on SIGHUP** | The concurrency cap's memory is read once at start (#187). SIGHUP only re-applies the cap to the reloaded `maxConcurrent`. |
| d | **An interrupted `host init` leaves `warm-load` running** | The Runner kills sudo with SIGKILL, which sudo can't relay, so the helper's `warm-load` runs on until its own bound (30 min). A killed `image-build` does the same, and also leaves its context copy (≤ 16 MiB) in `/var/lib/factory-agent/build`. |
| e | **A declined privileges prompt fails the image steps** | `kataimage` and `katawarm` then fail with sudo's "a password is required", instead of being skipped with a warning that names the declined step. |
| f | **Digest-pinned kind images are unverified on a real host** | The warm build's `docker tag <ref> <tag>` after `nerdctl save` and `docker load` in the workspace has only been tested against fakes. If it doesn't resolve, tag on the host before saving. |
| g | **VM size from the TaskSpec's `limits`** | Both drivers use the agent's `workspace.size`, never the task's. |
| h | **The opt-in container test layer** (#116, `make e2e-run-container`) | It has never run. |

The kata LLD's risks stay as they are (`design/workstreams/agent/LLD/workspace-kata.md`,
"Risks and open questions"):
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
| T4 | **Digest-pinned kind images on kata** (fix f) | On the workstation, set `workspace.warm.kindImages` to a `name:tag@sha256:…` ref, run `host init --driver kata`, then a task that runs `kind create cluster` with that image. | kind finds the image without pulling it. |
| T5 | **The container test layer (#116)** | On the workstation: `make e2e-run-container`, about 300 MB of pulls. | It passes. Fix what it finds, or drop the layer (fix h). |
| T6 | **An agent-only change on kata** | Rebuild `factory-agent` and restart `supervise`. The kata driver puts the binary into each workspace at start (`AgentBinary`), so nothing needs to be rebuilt. | `host check` stays ok, and the next workspace runs the new binary. |

Not testable without a catalog change: #173 for an implement run under claude, because
implement allows only codex. The claude side of the `ccfr` lookup, the plugin-dir fallback, is
exercised by every docs-review run.

### 2. P1 phase 10: removal

Phase 9 has shipped (#174–#190), so the host runtime can go. From P1's "What gets removed":

| Area | Remove |
|---|---|
| codex adapter | sandbox flags, `writable_roots`, `gitDirs` (#81), `workDirFor` (#87), the MCP approval key, the `CODEX_HOME` auth symlink, the login probe, the `UNVERIFIED` sandbox flags |
| claude adapter | `sandbox` settings, `Edit`/`WebFetch` rules, denied reads of the owner's HOME, the sandbox-support probe (`sandbox-exec`, `bwrap`/`socat`), Keychain/`USER` handling, `CLAUDE_CONFIG_DIR` as the owner's dir |
| pipeline (host) | owner `PATH`/`DOCKER_*` passthrough, the credential-helper socket, the `factory` shim's host path, `gitsafe` (#82) |
| agent | `internal/agent/stack`, the `localDev` config, the `has-local-stack`/`gif-tooling` probes and label code, `verifyAgainst` as a stack trigger, the device flow, the join-token file, `worker` (`--runtime host`) |
| catalog and prompts | `workspace.hosts` (replaced by `environment.egress`), the "never install" rules and `not-run` stopgaps (#85), the temporary-API/UI stopgap (#88) |
| docs | the LLDs (worker → supervise, run-pipeline, harness-adapters, local-stack → environment), LOCAL-GUIDE |

Removing the device flow needs enrollment first: phase 6, under Later.

Left over from stack 1, for stack 2 (from #192–#194's "Deferred" lists):
- **`runner.Stub`, and the pipeline's stack step:** `pipeline.Options.Stack`,
  `stackUpIfNeeded` with its `has-local-stack` gate (its error still says "configure
  localDev"), and `stackURLVars`. Also the claude adapter's local-stack sandbox host.
- **The host-only pipeline:**
  - `Pipeline.Run` on the host;
  - the credential-helper socket (`fx git-credential`);
  - the `PATH`/`DOCKER_*` passthrough;
  - the `factory` shim;
  - `gitsafe`;
  - the Effector's host runtime (`FACTORY_RUNTIME` unset).
- **The host side of the adapters and the CLI:** the adapters' host sandbox setup,
  `factory-agent probe`'s host probes, and `make smoke-codex`/`make smoke-claude`, which drive
  the host adapters.
- **The scheduler's `rowLocalStack`.**
- **Stale docs:**
  - links into the deleted `local-stack.md` (`run-pipeline.md` lines 17 and 70) and comments
    citing it (`pipeline.go`, `env.go`);
  - `worker.md` named in `base.sh` line 135. Edit it with the next `base.sh` change: any edit
    rebuilds the base VM;
  - the worker and host-stack wording in 02-architecture, 05-agent-runtime §149, 08-roadmap,
    10-poc and rollout.md.
- **`has-local-stack` in `api/examples` and the web mocks:** arbitrary label strings, which can
  stay.
- **`ccf-review`'s `verify.md` says "`npm ci` (ask first)".** Under HEADLESS.md rule 9,
  headless reviews could run it without asking. That's a ccf-skills change.

Stack 1 (above) takes `worker` / `--runtime host`, the `localDev` and label config, the LLD
renames and the "never install" lift. Stack 2 takes the rest. Test T2 needs the "never install"
rule gone:
in VM runs `yarn` now reaches the registry (#172), but agents still don't run `yarn install`,
so docs builds stay `not-run`.

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
    ETXTBSY on Linux).
  - Also seen: pipeline `TestRunEnvironmentWallClockLimit` under load ("the environment didn't
    start" at 32 s). #168 widened its bound; watch it.

### P1 leftovers
- **A memory budget for tasks bigger than `workspace.size`** (stack 3 caps a task's
  `limits.cpus`/`limits.memory` at `workspace.size`, since `kataConcurrency` assumes it).
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
