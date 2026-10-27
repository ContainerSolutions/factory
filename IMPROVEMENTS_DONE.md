# Factory improvements: done

The record of what was decided and shipped. **Open work is in [IMPROVEMENTS.md](IMPROVEMENTS.md)**;
read that one when planning. This file is history: the why behind past changes, and PR numbers to
find them. Kept on the `improvements` branch with IMPROVEMENTS.md (never merged to main).

## The two streams of 2026-10-21

| Stream | Spec | State |
|---|---|---|
| **P1. Each task runs in a VM workspace** | `design/proposals/P1-agent-workspaces.md` | The Mac track (phases 1–8) shipped by 2026-10-24, and the Linux track (phase 9) by 2026-10-26. Phase 6 was skipped for the PoC; phase 10 is open. |
| **P2. The dispatcher keeps its state in a file** | `design/proposals/P2-dispatcher-state-file.md` | Shipped: `internal/store/filestore`, `serve --state-dir`, `factory state`. |

What P1 replaced:
- **A, "a hard sandbox per agent":** one VM per task, with the harness's own sandbox off, and
  credentials held by a gateway outside the VM.
- **B, "per-task environments in the TaskSpec":** `workspace.environment`, run by the in-VM
  runtime, with versions resolved at compile time.

## What the first live runs taught us (2026-10-20/21)

| # | Symptom | Stopgap | Real fix |
|---|---|---|---|
| 1 | codex's sandbox denied writes to `.git` | #81 | P1: no harness sandbox in the VM |
| 2 | a hook planted in `.git` could run on the host | #82 | P1: git runs in the VM |
| 3 | plan-epic's read-only sandbox blocked `$FACTORY_RESULT` | #87 | P1 |
| 4 | codex's MCP approval prompt in a headless run | #87 | P1 |
| 5–6 | no yarn, so checks and docs builds never ran | #85 (`not-run`) | P1: a toolchain in the base VM |
| 7–8 | UC2 had no stack for screenshots; STYLE.md required local-dev | #88 (temporary API/UI) | P1 §4: environments |
| 9 | skills called `factory-agent stack env`, which isn't on a run's PATH | #90, #91 (closed) | ccf-skills v0.9.0 reads `FACTORY_STACK_*` |
| 10 | local-dev TTY error when driven headlessly | local-dev#127 | — |
| 11 | dispatcher port clash with local-dev | #78/#79 (port 8800) | P1: own network per VM |
| 12 | an environment failure reported as `blocked` | #80 | kept |
| 13 | claude rejected `--json-schema` with an `allOf` root | #84 | kept |
| 14 | review didn't start: created PR not linked | #83 | kept |
| 15 | item stuck in Running | #86 | kept |
| 16 | redispatch reused the old prompt | #89 (recompile) | kept |
| 16a | state lost on restart | — | P2 |
| 16b | the harness held `FACTORY_TOKEN` and `GH_TOKEN` | — | P1 gateway |
| 18 | a review waited 10 min with one agent | — | #137 |

Row 17, missing App installations, is still open: IMPROVEMENTS.md, group 1 (f).

## Stopgap PRs

| PR | Keep? |
|---|---|
| #80, #83, #86, #89 | yes: general fixes |
| #84 | yes, while claude takes `--json-schema` |
| #81, #82, #85, #87, #88 | no: removed in P1 phase 10 (listed there) |
| #90, #91 | closed, superseded by P1 |

## P1 Mac track (2026-10-22 → 2026-10-24)

| Phase | PRs |
|---|---|
| 1 contracts | #100 (`workspace.environment`), #101 (lease credentials, `runToken`), #102 (enrollment) |
| 2 host init and base VM | #103, #104, #130 (codex's code-mode host) |
| 3 `factory-agent run` in the VM | #105–#116, #129 (envrun) |
| 4 gateway | #117–#123 |
| 5 supervise and the tart driver | #125, #126, #127 |
| 6 run tokens and enrollment | skipped: the gateway injects the agent token |
| 7 environment in the catalog | #131 (web: closed), #132 (compiler), #133 (catalog), #134 (GIF tools), ccf-skills #24 (v0.9.0) |
| 8 first real runs | fixes: #136 (bundle off noexec `/run`), #137 (anti-affinity per harness, `--anti-affinity-wait`), #138 (collect only the outcome files), #139 (claude structured-output retries), #140 (the clean-slate environment) |

Phase 8 results:
- **UC2:**
  - plan-epic `single`, then build-single, implement, docs-review and fix, on mock-docs#14, #18
    and #21;
  - the GIF case worked after #138 and #139;
  - two lost outcomes were replaced by hand-written events.
- **UC1:** train workflows#137 → mock-docs#22. release-docs ran with the showcase environment at
  api 0.21.1 / agent 0.9.2, then docs-review-verify approved it against its own stack.
- **Verified by the runs:**
  - claude's and codex's placeholder logins through the gateway;
  - the name-constrained CA accepted by claude, codex, git, Node and Python;
  - the model-path allow-lists, with no refusals seen.

Decisions taken on the way:
- **The gateway holds only the factory's own credentials and the model logins,** never
  environment secrets (2026-10-23).
- **The environment is local-dev's plugin-less clean slate** (`make demo-up-slate`); agents
  configure `slate-agent` through overlays (#140).
- **The reviewer App had to be installed on `compliance-framework/local-dev`,** for
  docs-review-verify's read token.

## Group 1: operability (2026-10-26)

One stack, merged:
- #163: the contract;
- #164: event replay (only rules that never fired act; 409 `replay-history-gone`);
- #166: close and delete a work item;
- #167: App installation checks, `reasonDetail`;
- #169: the environment log;
- #162: binary-only base VM updates;
- #165: ChatGPT refresh, with at most one 401-driven refresh a minute;
- #168: flaky tests;
- #170: web for all of it.

Then two fixes from the first runs after it:
- #172: `NODE_USE_ENV_PROXY`, so corepack and yarn reach the registry through the gateway;
- #173: implement ships ccf-review, and codex links each plugin's `bin/` so lisa finds `ccfr`.

Verified on real runs (2026-10-26, from the Mac's supervisor records):
- **#162:** the base VM got the new binary without a rebuild. `basevm.json`'s `builtAt` predates
  the binary, but its `agentSHA256` matches it.
- **#173:** codex implement runs ran `ccfr detect` from the linked `bin/`.
- **#172:** corepack's yarn 1.22.22 download works through the gateway, and claude-code docs
  reviews run normally with `NODE_USE_ENV_PROXY=1`.

## P1 phase 9: the Linux track (kata), 2026-10-26

- **#174:** the kata driver's LLD (`workspace-kata.md`) and the Q8 dry run. A warm Docker disk
  replaces P1's registry mirror.
- **#175:** the per-OS state dir and the platform step.
- **#176:** base.sh for amd64, and the image's build context.
- **#177, #178:** host init's install steps (nerdctl-full, Kata) and its services (containerd,
  buildkitd, the workspace firewall).
- **#179, #181, #182:** the kata root helper and its verbs.
- **#180:** `vmexec`, the drivers' shared exec layer.
- **#183, #184:** the kata workspace driver and its integration tests.
- **#185, #186:** host init's `kataimage` and `katawarm` steps.
- **#187:** `supervise --driver kata`, with concurrency above 1.
- **#188, #189:** IPv4-only image builds, and login on Linux.
- **#190:** the `local` Pool lets an agent run three tasks.

## P1 phase 10, stack 1: the common part (2026-10-26)

Merged before the phase 9 follow-ups started, because both touch `cmd/factory-agent` and
`internal/agent/config`:
- **#192:** supervise is the only runtime. `worker` and `--runtime host` are gone; the shared
  lease loop moved to `agentloop.go`.
- **#193:** the host stack, `localDev` and the label probes are gone. The config decode is
  strict, so a leftover `localDev` key in `agent.yaml` now fails.
- **#194:** the worker and local-stack LLDs fold into supervise and workspace-run.
- **#195:** the prompts lift "never install" (#85's stopgap): agents install what the repo's
  checks need through its package manager, never system tools, and never change lockfiles.
  Every TaskSpec pins ccf-skills `v0.10.0`.
- **ccf-skills#25 (`v0.10.0`):** HEADLESS.md rule 9, and docs-release runs `yarn install
  --frozen-lockfile` before building.

## P1 phase 9 follow-ups, stack 3 (2026-10-27)

Fixes a–h from the kata track, one stack as `ccf-lisa[bot]`, reviewed with ccf-review, merged
by Gustavo:
- **#197 (a):** the runner's gateway listens on `workspace.gatewayPort`, the port the pf anchor
  and the nft table open, not a hard-coded 3128.
- **#199 (b):** tart sizes its VMs from `workspace.size`, as kata does.
- **#200 (c):** SIGHUP re-reads physical memory for the kata concurrency cap, keeping the last
  good value when the read fails.
- **#202 (d):** an interrupted `host init` stops the helper: a cancelled `sudo -n` gets SIGTERM,
  which sudo relays even from factory-agent's own process group, then SIGKILL after 30 s. The
  helper handles signals for every verb and kills nerdctl's group; image builds take a flock and
  sweep stale contexts. Helper protocol 7.
- **#204 (e):** a declined privileges prompt skips `kataimage` and `katawarm` with a warning and
  records nothing, so the next `host init` asks again.
- **#205 (f):** the warm build saves each kind image single-platform
  (`docker save --platform linux/<arch>`) and makes the archive 0644. Test T4 found it: the
  digest-to-tag step worked, but kind's `ctr import` needs every manifest an index names, and
  `up` runs as `admin`. T4 passed again with the fix.
- **#206 (h):** test T5 passed on its first run, and `make e2e-run-container` runs in CI as an
  advisory job ("e2e container (advisory)"). It stays advisory: it runs the in-VM binary in a
  container, which can't prove tart or kata.
- **#207, #208 (g):** a TaskSpec's `limits.cpus`/`limits.memory` size its workspace, hard-capped
  at the agent's `workspace.size` (a larger request is capped with a warning). By design there's
  no budget beyond `workspace.size`: a task that needs more runs on an agent whose
  `workspace.size` is that big.

Decided not to do: a build-recipe version in the warm records (`katawarm.json`,
`warmbase.json`). A warm disk built before #205 keeps its old archives until its record is
deleted, which is fine while Gustavo is the only user.

## Dropped at the closing phase (2026-10-27)

When the factory entered its closing phase, the backlog was cut to the next workstreams
(IMPROVEMENTS.md). Everything below was dropped: none of it stops the PoC working or leaks
credentials. Kept from this list and moved to IMPROVEMENTS.md: the egress wiring, phase 6 and the org move.
**Implement's environment** was dropped too (Gustavo, 2026-10-27): this catalog is for the PoC,
and #88's throwaway API/UI environment is fine for its screenshots, so its instructions stay. Also dropped: ccf-review's
"`npm ci` (ask first)" (never seen to matter), a codex run without the MCP approval key,
`base.sh` line 135's `worker.md` mention, and the kata LLD's accepted risks (they stay in
`workspace-kata.md`).

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

## Other items closed

- **C. Anti-affinity:** kept, but only for the same harness, with a configurable wait (#137).
- **A work item outliving its closed issue or PR:** `close-on-primary-closed` and `closed-gate`
  (#133), and Done on the board (#131).
- **`build-single`** moved from the owner's local catalog to main (#133).
- **UI, the owner's list of 2026-10-24:**
  - catalog tabs and a detail pane (#141–#143, #146, #153);
  - work-item Graph, LLM tasks and Timeline tabs (#144);
  - exact effect → event links (#147–#149);
  - graph fixes and layout (#151, #152, #154, #156, #159–#161).
- **codex's second writable checkout getting only its `.git`:** moot, there's no codex sandbox in
  the VM.
