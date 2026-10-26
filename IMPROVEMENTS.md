# Factory improvements: open work

Everything the factory still needs, and nothing else. Done work and the reasons behind past
changes are in [IMPROVEMENTS_DONE.md](IMPROVEMENTS_DONE.md). Local only, never pushed.

Each item becomes its own PRs, through lisa and reviewed with ccf-review. **Before starting a
phase or group,** say what you'll design, what's in and what goes back here as deferred, and
wait for Gustavo.

## Now

### 1. Operability, one PR stack (started 2026-10-26)

Things that cost time in phase 8 and will again.

| # | Item | Notes |
|---|---|---|
| a | **Replay an event through the rules:** `POST /events/{id}/replay`, a `factory` CLI command, a UI button next to each event | Re-evaluates a stored event against the current catalog: a run's `factory.run.completed`, a provider event, any event. Admin only, and recorded in the timeline as a replay with its new decisions. Idempotent: dispatches dedupe against tasks the first evaluation created, and effects already done aren't repeated. In phase 8 two lost outcomes were replaced by hand-written events (`deploy/poc/events/wi_9760-*.json`). |
| b | **Close, mark won't-do or delete a work item:** `POST /workitems/{id}/close` with a reason (`done`, `wont-do`, `duplicate`), a real delete, the CLI and UI buttons | Close moves the card to Done, suppresses further runs (like `lifecycle: closed`) and stays in the timeline. Stale card: `wi_c97ee34bbf1a22d8fee1` (mock-docs#4). Shares a contract PR with (a). |
| c | **Collect the VM's `environment.log`** with the outcome, store it with the run, show it on the run page | Since #138 only `outcome.json` and the transcript come back, so an environment failure leaves only the run's summary. |
| d | **Update only the agent binary in the base VM** | The base VM records the linux `factory-agent`'s sha256, so any agent change reprovisions everything: apt, Docker, the gifski build, the image pulls. Copy the new binary into the base and the warm base instead, and reprovision only when the script or the image changes. |
| e | **ChatGPT token refresh in the gateway** | Codex runs fail with 401 once the access token expires, until the owner runs `factory-agent login codex` again. The full design is in `gateway.md` §ChatGPT refresh: a single-flight refresher per supervisor, write-back, codex's `oauth_provenance`. Its endpoint details are UNVERIFIED. |
| f | **Fail loudly on missing App installations** | Two parts. A `factory serve` startup check that each App (author, reviewer) is installed on every repo the catalog's TaskSpecs and RuleSets can touch, ro and skill repos included. And a `git-credential` pending reason that names the App, the repo and the fix, shown in the UI. Seen with local-dev for the reviewer, and earlier ccf-skills. |
| g | **Fix the flaky tests** | `TestRunFailures` (claudecode), `TestRunRateLimitStopsTheRun` (claudecode and codex), `TestRunWallClockLimit` (pipeline) and codex `TestRunCancelKillsTheProcessTree` all time out under load: inject clocks or widen margins. `TestStackGit` (effector): a background `git gc` races the temp dir cleanup; set `gc.auto=0`. `TestRunSpecSchemaRepair` (pipeline, Linux CI): ETXTBSY when exec'ing a script just written. `TestReadConcurrent` (filestore) was fixed in #129; watch it. Also seen on #162's CI (2026-10-26): `TestCascadeRunningControllers` (controllers, `rule_test.go:79` "timed out waiting for the run" after 10 s); #168 doesn't cover it. |

### 2. P1 phase 9: the Linux track (`kata`)

Gustavo is preparing the Linux workstation (64 GiB). It runs 3 workspaces and reaches the
dispatcher on the Mac through the tunnel. From P1's Phases:
1. **The Q8 dry run:** the spike's Kata provisioning on the bare host. See
   `design/proposals/P1-phase0-report.md`, Q8.
2. **`host init --driver kata`:**
   - containerd, nerdctl, and Kata with the rendered Cloud Hypervisor config;
   - CNI, iptables (the Linux counterpart of Softnet and the pf anchor);
   - the registry mirror.
3. **The kata driver:** `internal/agent/workspace`'s `Driver` (create, start, put, exec, get,
   destroy, list, stop), like `workspace/tart`. Get takes the file list (#138).
4. **The base image's kata form,** built by `host init`. The provisioning script
   (`internal/agent/host/provision/base.sh`) already has it; CI only checks the script.
5. **Supervise with more than one workspace:** concurrency above 1 is deferred today, and
   `maxConcurrent: 3` needs it. Then the workstation joins the pool.

Also open for this track:
- VM size from the TaskSpec's `limits` (today a fixed 8 GiB / 6 vCPU);
- the opt-in container test layer (#116, `make e2e-run-container`), which has never run.

Fixes found while building and running it (stack #174–#189, 2026-10-26):

| # | Fix | Notes |
|---|---|---|
| a | **The gateway port is hard-coded** | The runner listens on 3128 (`workspace.GatewayPort`, `runner.go`), while the pf anchor (tart) and the nft table (kata) open `workspace.gatewayPort`. Any other port opens the wrong one. Pass the config's port to the runner. Since phase 5 on tart. |
| b | **tart doesn't follow `workspace.size`** | supervise gives tart `Spec{}` (6 vCPUs, 8 GiB) whatever the config says; kata uses `workspace.size` (#187). |
| c | **supervise doesn't re-read physical memory on SIGHUP** | The concurrency cap's memory is read once at start (#187); SIGHUP only re-applies the cap to the reloaded `maxConcurrent`. |
| d | **An interrupted `host init` leaves `warm-load` running** | The Runner kills sudo with SIGKILL, which sudo can't relay, so the helper's `warm-load` runs on until its own bound (30 min). Same for a killed `image-build`, which also leaves its context copy (≤ 16 MiB) in `/var/lib/factory-agent/build`. |
| e | **A declined privileges prompt fails the image steps** | `kataimage` and `katawarm` then fail with sudo's "a password is required" instead of being skipped with a warning naming the declined step. |
| f | **Digest-pinned kind images are unverified on a real host** | The warm build's `docker tag <ref> <tag>` after `nerdctl save` and `docker load` in the workspace is only tested against fakes. If it doesn't resolve, tag on the host before saving. |
| g | **The `local` Pool caps every agent at one task** | `catalog/pools/local.yaml` has `lease.maxTasksPerAgent: 1`, and the scheduler takes the smaller of that and the agent's `lending.maxConcurrent`, so the workstation's 3 parallel tasks wait as `agent-busy`. Fixed in #190 (`maxTasksPerAgent: 3`). |

### 3. P1 phase 10: removal

After phase 9, since it removes the host runtime that Linux uses until the kata driver exists.
From P1's "What gets removed":

| Area | Remove |
|---|---|
| codex adapter | sandbox flags, `writable_roots`, `gitDirs` (#81), `workDirFor` (#87), the MCP approval key, the `CODEX_HOME` auth symlink, the login probe, the `UNVERIFIED` sandbox flags |
| claude adapter | `sandbox` settings, `Edit`/`WebFetch` rules, denied reads of the owner's HOME, the sandbox-support probe (`sandbox-exec`, `bwrap`/`socat`), Keychain/`USER` handling, `CLAUDE_CONFIG_DIR` as the owner's dir |
| pipeline (host) | owner `PATH`/`DOCKER_*` passthrough, the credential-helper socket, the `factory` shim's host path, `gitsafe` (#82) |
| agent | `internal/agent/stack`, the `localDev` config, the `has-local-stack`/`gif-tooling` probes and label code, `verifyAgainst` as a stack trigger, the device flow, the join-token file, `worker` (`--runtime host`) |
| catalog and prompts | `workspace.hosts` (replaced by `environment.egress`), the "never install" rules and `not-run` stopgaps (#85), the temporary-API/UI stopgap (#88) |
| docs | the LLDs (worker → supervise, run-pipeline, harness-adapters, local-stack → environment), LOCAL-GUIDE |

Removing the device flow needs enrollment first: phase 6, under Later.

## Later

### Deferred from group 1 (#162–#169)
- **Replay (#164):**
  - A replay of an event that hasn't been processed yet can race its first reconcile, which
    checks the processed mark before taking the lock. That duplicates decisions, not effects.
  - Decisions past the 200-decision bound are gone, so replay re-applies those rules, deduped by
    key only.
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
- **Flaky tests (#168):**
  - claudecode/codex fake scripts are still written with a plain `os.WriteFile` (possible
    ETXTBSY on Linux).
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
- **Deferred review findings** (should-fix and nits) are in each PR's
  `~/.ccf-review/sessions/compliance-framework-factory-<N>/triaged.json`:
  - #118: test gaps in the gateway policy;
  - #121: `Load` doesn't check the CA's name constraints against the intercepted hosts;
  - #123: `%p` on an `AnthropicAuth`/`ChatGPTLogin` value prints the token.
- **`refs/notes/commits`** has 156 notes commits under the owner's identity, from before the bot
  identity rule. Rewriting them is the owner's call.
- **Review rule candidates** wait in `~/.ccf-review/proposals.jsonl` for Gustavo's
  `/ccf-review:rules propose`.
