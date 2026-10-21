# P1: Agents run each task in a microVM workspace (Kata + Cloud Hypervisor)

> Status: **proposal**, for Gustavo's review (2026-10-21). Workstreams: AGT (agent), RT
> (dispatcher), FND (contracts), CAT (catalog), with a ccf-skills release. Supersedes the
> host-side harness sandboxes and the CCF-specific local stack (IMPROVEMENTS rows 1–11).
>
> Decisions so far (Gustavo, 2026-10-21):
> - **microVMs only.** No container-or-VM hybrid; get one thing working first.
> - **Kata Containers with Cloud Hypervisor** as the VM layer.
> - **First target is Gustavo's M3+ Mac,** with his Linux workstation alongside: 1 + 3
>   workspaces.
> - The full list is under **Decisions** at the end.

## Why

On 20–21 Oct every live failure but four came from running the harness **on the owner's host,
inside the harness's own sandbox**:

- **Per-path carve-outs broke.** Codex's `.git` was read-only, read-only mode killed the Result
  and the network, and MCP tools needed approval.
- **The host's toolchain is the run's toolchain.** yarn was missing, so nothing was ever built,
  and headless runs aren't allowed to install anything.
- **The environment is CCF-specific Go code tied to the owner's local-dev.** No stack runs
  without a train, local-dev's scripts failed headless, ports clashed, and skills call a
  `factory-agent` CLI that isn't on the run's PATH.
- **Credentials sit inside the agent's reach:** the full agent token (`FACTORY_TOKEN`), the
  task's `GH_TOKEN`, and the owner's Claude and Codex logins.
- **Registration needs a person at every start** (the GitHub device flow).

CCF development needs Docker inside the workspace: local-dev is docker compose plus kind, and
the API's tests use testcontainers. A hardened container can't host dockerd without
`--privileged`, which undoes the hardening. A **microVM** can: it has its own kernel, and a
privileged container inside it is privileged only inside that VM.

## Goal

- **`factory-agent` is a supervisor.** It keeps the connection to the dispatcher and creates,
  runs and destroys one **microVM workspace** per task.
- **Each workspace is a Kata microVM** (Cloud Hypervisor) with:
  - its own kernel and its own **dockerd**, so docker, compose, kind, testcontainers and local-dev
    run unchanged;
  - the repos the TaskSpec asks for;
  - the tools its image brings; the LLM may install more (yarn, an apt package, a Playwright
    browser), since it's root in its own VM;
  - the network only through a **gateway** that holds every credential.
- **No credential enters a workspace.** The gateway adds Anthropic, OpenAI, GitHub and the
  dispatcher's tokens to the requests that need them, much as the Effector's MCP server speaks to
  the dispatcher on the agent's behalf today. Subscription logins stay on the owner's agent host
  and never reach the dispatcher (§5, ADR-0008).
- **Four workspaces at once across two agent hosts** (1 on the Mac, 3 on the Linux workstation;
  see Decisions), sharing Gustavo's Claude subscription through their gateways.
- **The TaskSpec declares the environment:** image, repos, setup, and what it exposes. The
  factory core knows nothing about CCF, trains or local-dev.
- **No device flow.** An agent enrolls with a token an admin issues.

## Where it runs

Kata needs Linux with KVM, and it doesn't run on macOS. So **the supervisor always runs on
Linux:**

```
 Gustavo's Mac (M3+, macOS 15+)                         later: Linux workstation
 ┌──────────────────────────────────────────────┐      ┌───────────────────────────┐
 │ factory serve (dispatcher), ngrok             │      │ (same as the agent host   │
 │                                               │      │  VM, on bare metal)       │
 │ ┌───── agent host VM (Linux, nested KVM) ────┐│      │ factory-agent supervise   │
 │ │ created once with `tart run --nested`,     ││      │ containerd + Kata + CH    │
 │ │ always on; sized for N workspaces          ││      │ gateway, registry mirror  │
 │ │                                            ││      └───────────────────────────┘
 │ │ factory-agent supervise                    ││
 │ │ containerd + nerdctl + Kata (CH)           ││
 │ │ gateway, registry mirror, caches           ││
 │ │   ┌ microVM ws-1 ┐ ┌ ws-2 ┐ … ┌ ws-5 ┐     ││
 │ └────────────────────────────────────────────┘│
 └──────────────────────────────────────────────┘
```

- **The agent host VM is the only Mac-specific piece.** It's a Debian or Ubuntu VM created once,
  with nested virtualisation, which Apple's Virtualization.framework offers on M3+ under macOS 15.
  Tart's `--nested` flag uses it, and Tart is the proposed tool: a small, maintained CLI that
  distributes VMs as OCI images. The VM runs all the time and hosts everything else. A
  `factory-agent host init` script (or a published Tart image) provisions it:
  - containerd, nerdctl and Kata with Cloud Hypervisor;
  - CNI and iptables;
  - the registry mirror;
  - `factory-agent`.
- **On the Linux workstation** the same provisioning runs on the host itself. Nothing else
  changes.
- **The dispatcher stays on the Mac.** The agent host VM reaches it at the Mac's address on the
  VM network, or through the ngrok URL. Nothing in the agent depends on which.

## Architecture

```
  dispatcher ◄── leases, heartbeats, effects, transcripts ── supervisor (agent host)
      │                                                          │
      └─ credentials per lease (short-lived) ──────────────► gateway (one per workspace)
                                                                 ▲ only route out
  ┌────────────────────── microVM workspace (Kata, CH) ──────────┴───────────────┐
  │ OCI image: factory-workspace (from the TaskSpec)                             │
  │ factory-agent run   (the in-VM runtime: pipeline, harness, Result)           │
  │ dockerd (privileged inside the VM) → local-dev, kind, testcontainers         │
  │ /work/repo (rw clone)   /work/ro/<name> (clones of the read repos)           │
  │ claude / codex with their own sandboxes off: the VM is the boundary          │
  │ HTTPS_PROXY = gateway, the per-task CA trusted; no credential on disk or env │
  └──────────────────────────────────────────────────────────────────────────────┘
```

### 1. `factory-agent supervise` (replaces `worker`)

- **Keeps** enrollment (§6), the lease long-poll, heartbeats, the progress and usage relay,
  throttling, lending, drain and cancel. That's today's `internal/agent/worker` with a new runner.
- **The runner** (the `runner.Runner` seam) becomes `workspace.Runner`. Per lease it:
  1. writes the run bundle (§2) to a per-task directory;
  2. starts the task's gateway (§3) and a CNI network for the workspace whose only route out is
     the gateway, enforced with iptables in the agent host, where the workspace can't touch them;
  3. starts the workspace:
     ```
     nerdctl run --runtime io.containerd.kata.v2 --privileged \
       --cpus … --memory … --network ws-<task> \
       -v <bundle>:/run/factory:ro …
     ```
     `--privileged` is safe here: containerd's Kata config sets
     `privileged_without_host_devices = true`, so it means "privileged inside the VM", with no
     host devices;
  4. streams the workspace's stdout (JSONL events) and relays it as heartbeat progress,
     forwarding cancel as SIGTERM, then SIGKILL after the grace period;
  5. collects `out/result.json` and the transcript, uploads them, and completes the lease;
  6. destroys the VM, its network and its disk; `--keep-workspace` keeps the disk for
     debugging.
- **It drives the runtime through the `nerdctl` CLI** (exec), not a library, which fits the
  dependency allow-list as the `docker` CLI does today. It never touches the agent host's own
  Docker, if it has one.
- **Fresh VM per task.** Kata with Cloud Hypervisor boots in about 1–2 s, so a warm pool isn't
  needed at first. The slow part is what runs inside: starting dockerd and pulling local-dev's
  images. The **registry mirror** in the agent host (`registry:2` as a pull-through cache, set as
  each workspace's dockerd mirror) makes the second pull local. Phase 0 measures it. If start
  time still hurts, a pool of pre-booted VMs with dockerd up comes next.
- **Concurrency and limits:** up to `lending.maxConcurrent` workspaces at once: 1 on the Mac,
  3 on the workstation. Each is bounded by vCPUs, memory and disk, taken from the TaskSpec's
  `limits` and capped by the agent's config. local-dev with kind wants about 8 GB, so each host is
  sized for its N × that plus overhead.

### 2. The workspace (Kata microVM) and `factory-agent run`

- **Image.** `ghcr.io/compliance-framework/factory-workspace:<factory version>` is the base, an OCI
  image:
  - Debian, plus dockerd and the compose plugin, kind, kubectl, git, gh (with the pr-review and
    stack extensions), jq, python3, curl, make, node LTS with corepack (yarn and pnpm), go, and
    Playwright's dependencies;
  - the `claude` and `codex` CLIs, pinned;
  - `factory-agent` (linux) as the entrypoint.

  A TaskSpec may name another image. It must be `FROM` the base; the catalog checks this by
  label. The factory repo builds and publishes the base on each release.
- **Docker inside.** The entrypoint starts dockerd. Its storage is a scratch block device the
  supervisor attaches, because overlayfs on virtiofs is unreliable; Phase 0 settles how Kata
  exposes it. dockerd's registry mirror is the agent host's mirror, reached through the gateway's
  network. The LLM and local-dev use it like any Docker host.
- **The run bundle** (`/run/factory`, read-only). The supervisor writes it before the VM starts,
  so the workspace needs no dispatcher credential to set itself up. It holds:
  - the Task (the compiled spec);
  - the rendered prompt and context items;
  - the bundled Result schema;
  - the skills, materialised from the supervisor's cache;
  - the gateway's CA certificate;
  - a **run token**: an opaque per-run identifier, not a credential. The gateway maps it to this
    task.
- **`factory-agent run`** (in the VM) is today's pipeline from step 2 on:
  1. Clones the repos through the gateway: rw into `/work/repo`, ro into `/work/ro/<name>`.
     - The LLM is root in its VM, so "ro" inside the VM is advisory.
     - **The guarantee is upstream:** the gateway refuses pushes to every repo the task may not
       write (§3).
  2. Runs the environment's `setup` and `up` (§4), and waits for what it exposes.
  3. Writes the ticket files, the harness config and the MCP config for the Effector:
     - **claude:** `bypassPermissions`, no `sandbox` block;
     - **codex:** `--sandbox danger-full-access`.
  4. Runs the harness, then the Result validation and repair.
  5. Writes `out/result.json` and the transcript, then runs the environment's `down`.

  Its stdout is the event stream the supervisor relays. The Effector (the `factory` CLI and MCP)
  runs here too. Its git-side ops (push, stack push and rebase) are ordinary git in the VM,
  through the gateway. **No process on the agent host ever runs git in an agent-written
  checkout,** which retires #82's hardening.

### 3. The gateway: every credential, outside the VM

One per workspace, started by the supervisor in the agent host. It's a forward proxy
(`HTTPS_PROXY`/`HTTP_PROXY` in the VM), and the workspace network's only route out. It takes
credentials from the lease (§5) and keeps them in memory. Per host:

| Host | What the gateway does |
|---|---|
| `api.anthropic.com` | TLS-intercepts and sets the harness's credential: `Authorization: Bearer <OAuth token>` (subscription) or `x-api-key`. The VM carries a placeholder. |
| OpenAI / ChatGPT (codex) | **Subscription (ChatGPT) auth is a refresh-token flow** that codex runs itself. Whether the gateway can carry it, keeping the real tokens outside, is a Phase 0 question. If it can't, the PoC drops codex and runs every task on claude-code (Decision 4). |
| `github.com` (git smart HTTP) | Adds `Authorization: Basic x-access-token:<installation token>` for the repo in the URL path: the task token for its rw repo, read tokens for its read repos. **Refuses `git-receive-pack` (push) to any repo the task may not write.** Public repos it can't credential pass through anonymously. |
| `api.github.com` | Adds the task's token for **GET only.** Every write (POST, PATCH, PUT, DELETE) is refused, because GitHub writes go through the Effector to the dispatcher. That turns "writes only through the Effector" from a prompt rule into a network rule. |
| the dispatcher (`FACTORY_API`) | Adds a **run-scoped token** for the routes a run needs (§5). The agent token stays in the supervisor. |
| the registry mirror, registries (npm, yarn, PyPI, Go proxy, ghcr, Docker Hub, apt) and the rest of the internet | Plain CONNECT tunnel, no interception, allowed by default. **Denied:** link-local and metadata (169.254.0.0/16), RFC 1918 and the agent host's own addresses (except the mirror and the dispatcher), and other workspaces. The TaskSpec can tighten this (`environment.egress: deny-by-default` plus an allow list). Widening past the default needs two approvals, as `workspace.hosts` does today. |

- **Two layers.** Credential safety doesn't depend on the network rules: a process that bypasses
  the proxy simply has no credentials, so it works only as an anonymous client. The iptables
  rules in the agent host (only the gateway is reachable) add egress control and logging on top.
- **Every request is logged** (time, method, host, path, status, bytes, not bodies) into the
  run's transcript as `factory.egress` lines.
- **The CA is per task:** generated for the run, trusted only inside that VM, and destroyed with
  it. It intercepts only the credentialed hosts above.

### 4. The environment is declared in the TaskSpec

A new block, `spec.workspace.environment` (a contract change in its own PR). Because the VM has
Docker, the environment is plain commands, and local-dev runs as it is:

```yaml
workspace:
  repos:                                    # as today: rw = the task's target, ro = the rest
    - { ref: "=task.target.repo + '@main'", mode: rw }
    - { ref: compliance-framework/local-dev@main, mode: ro }
  environment:
    image: ghcr.io/compliance-framework/factory-workspace:1   # optional; default = the base
    versions: latest                         # latest | inputs | train(inputs.train), resolved at compile time
    setup: |                                 # before the environment comes up
      corepack enable
    up: |                                    # runs in the VM, with the resolved versions as CCF_*_IMAGE
      make -C /work/ro/local-dev demo-up-showcase
    exposes: { FACTORY_STACK_URL: "http://localhost:8000", FACTORY_STACK_API: "http://localhost:8080" }
    ready: { http: ["http://localhost:8080/api/health", "http://localhost:8000/"], timeout: 15m }
    down: |                                  # best effort; the VM is destroyed anyway
      make -C /work/ro/local-dev compose-down-showcase
    egress: default                          # default | deny-by-default (+ allow: [...])
```

- **The environment is the repo's own tooling, run where it works:** a privileged Docker host of
  its own, with its own `localhost:8000`/`:8080`. The block is CCF configuration in the catalog,
  not factory Go code.
- **Version sources** are resolved by the dispatcher when it compiles the task and written into
  the Task, so a run is reproducible and a Redispatch (#89) re-resolves explicitly:
  - `latest`: each component's latest final release;
  - `inputs`: a map in the task's inputs;
  - `train(…)`: CCF's train issue, through the existing extractor.
- **What it replaces:**
  - `internal/agent/stack`, and the agent's `localDev` config;
  - the `has-local-stack` and `gif-tooling` labels (every workspace has Docker, and tools ship in
    images);
  - `verifyAgainst` as a stack trigger;
  - the one-stack-per-machine lock;
  - the fixed-port clashes.
- **Skills change with it** (a ccf-skills 0.9.0):
  - HEADLESS.md drops "never start a stack" and "`factory-agent stack env|wait` only". Instead:
    the task's environment is up before you start, its URLs are in `FACTORY_STACK_*`, and you
    may use Docker.
  - docs-gif, sidebyside and docs-release read only the variables.
  - The docs repos' STYLE.md keeps "seeded showcase data", which is now true in every run.

### 5. Credentials: injected by the gateway, never in the workspace

Every credential is added by the workspace's gateway, so none enters a VM. Where each one is
*held* depends on what it is:

- **GitHub and the dispatcher: factory-managed.** The dispatcher mints them per lease, as it mints
  git tokens today. **The lease carries a credential bundle** for the gateway (a contract change):
  - **today's fields:** the git task token and the read tokens;
  - **new:** the run-scoped dispatcher token.

  Heartbeats refresh the bundle as they refresh git tokens today. It goes from the supervisor to
  the gateway in memory: never to disk, never into the VM.
- **Model subscriptions (Claude OAuth token, codex's ChatGPT login): held by the agent host, which
  is the owner's own machine. The dispatcher never stores or forwards them.**
  - **Why.** ADR-0008 records that Anthropic's terms forbid a service from collecting, storing or
    intermediating subscription credentials, and that OpenAI's forbid sharing a login. Running
    the official CLI under your own login on your own machine is fine.
  - **How.** The owner puts the token in the supervisor's state dir once, 0600: for Claude, the
    output of `claude setup-token`. The supervisor hands it only to its own local gateways, in
    memory.

  This keeps ADR-0008's decision (the factory never handles subscription credentials). It
  changes only where the agent side keeps them: outside the workspace, in the gateway, instead
  of in the harness's environment and the owner's `~/.claude*`/`~/.codex`.
- **Model API keys (Anthropic, OpenAI), if a pool ever uses them: factory-managed.** They are a
  catalog `secretRef` on the Pool and travel in the lease's credential bundle. ADR-0008 already
  allows this for ephemeral pools (`Pool.llmAuth.apiKeyRef`). Not needed for the PoC.
- Owners stop keeping `~/.claude*` and `~/.codex` logins for the factory; only the supervisor's
  token file remains.
- **The run-scoped dispatcher token:**
  - **minted per lease and stored hashed;**
  - **it authorises only:** the task's `GET` prompt, context and output schema, `POST` effects,
    and `PUT` its own transcript;
  - **it expires with the lease.**

  It replaces `FACTORY_TOKEN` = agent token (a new `runToken` security scheme in the API).
- **Accepted risk:** the agent host holds the subscription token (0600 file, and in memory while
  a lease runs). A Claude OAuth token is long-lived (1 year), so a compromised agent host leaks
  it. Rotate it with `claude setup-token` and replace the file. The gateway redacts it from egress
  logs, and it never reaches the dispatcher.

### 6. Enrollment replaces the device flow

- **An admin mints an enrollment token** on the dispatcher:
  `factory agent enroll --pool local --owner gusfcarvalho --name mac-host [--ttl 7d]`. It needs the
  CLI token or a UI admin, and prints a one-time token. The dispatcher stores it hashed, with the
  owner and pool fixed by the admin.
- **The supervisor exchanges it once** (`POST /agents` with `X-Factory-Enrollment-Token`) for a
  long-lived **agent credential**: a refresh secret kept 0600 in its state dir. With it, the
  supervisor gets today's 24 h `agt_…` tokens and re-registers after a 401 or a restart with no
  human. Revoking the agent in the UI or CLI kills the refresh secret.
- **Owner attribution** (who lends the agent, and lending rules) comes from the enrollment's
  `--owner`. The admin vouches for it; no GitHub login happens on the agent side.
- **What goes away:**
  - `internal/agent/owner` (the device flow), `X-Factory-Owner-Token` and the `OwnerVerifier`
    exchange;
  - the join-token file. `Pool.admission.tokenRef` becomes "may mint enrollments for this pool".
  - `GET /auth/device-config` stays only for the UI's sign-in.

## What gets removed

| Area | Removed |
|---|---|
| codex adapter | sandbox flags, `writable_roots`, `gitDirs` (#81), `workDirFor` (#87), MCP approval key, `CODEX_HOME` auth symlink, login probe |
| claude adapter | `sandbox` settings, `Edit`/`WebFetch` permission rules, denied reads of the owner's HOME, sandbox-support probe (`sandbox-exec`, `bwrap`/`socat`), Keychain/`USER` handling, `CLAUDE_CONFIG_DIR` as the owner's dir |
| pipeline (host) | owner `PATH`/`DOCKER_*` passthrough, the credential-helper socket (git auth moves to the gateway), the `factory` shim baking a host path, `gitsafe` (#82) |
| agent | `internal/agent/stack` (CCF local-dev), `localDev`, `has-local-stack`/`gif-tooling` probes, device flow, join-token file, `worker` (becomes `supervise`) |
| catalog / prompts | `workspace.hosts` (replaced by `environment.egress`), the "never install" rules and `not-run` stopgaps (#85), the temporary-API/UI stopgap (#88) |
| skills | HEADLESS.md's stack rules and `factory-agent stack env|wait` |

What stays as is: the Effector op table and its dispatcher-side checks, the Result schemas and
repair, transcripts and redaction, and the dispatcher's scheduling, rails, rules and work items.
#80, #83, #84, #86 and #89 stay too.

## Phases

Each phase is one or a few PRs, through lisa and reviewed with ccf-review.

0. **Spike on Gustavo's M3+ Mac (a report, no merge).** It answers:
   1. Does `tart run --nested` give a Linux VM with working `/dev/kvm`?
   2. Does Kata with Cloud Hypervisor run in it? How fast is the boot?
   3. Does dockerd run inside a Kata workspace, `privileged_without_host_devices`, with a block
      device for its storage? Do `make demo-up-showcase` (compose plus kind) and an API
      testcontainers test work inside it?
   4. Does claude-code work behind a TLS-intercepting proxy with a placeholder token?
   5. Can the gateway carry codex's subscription auth?
   6. What do the cold and warm (mirror) start times look like, for the VM, dockerd and
      local-dev up?
   7. Memory per workspace with local-dev up, which confirms the Mac's agent host VM size for
      one workspace.
   8. A Linux workstation dry run: the same provisioning on a bare host, with the supervisor
      reaching the dispatcher through the tunnel.
1. **Contracts (FND),** in this order:
   1. `TaskSpec.workspace.environment`;
   2. the lease credential bundle and the `runToken` scheme;
   3. enrollment (`POST /agents` and the enrollment token);
   4. the supervisor's local subscription-token file and the gateway's auth modes (agent
      config, not a dispatcher contract); optional API-key `secretRef`s on the Pool.

   web/ regenerates its client.
2. **Agent host provisioning (AGT):** the `factory-agent host init` script for Debian/Ubuntu
   (containerd, nerdctl, Kata+CH, CNI, iptables, the registry mirror), plus a Tart image for the
   Mac.
3. **Base workspace image (AGT)** and its CI publishing.
4. **`factory-agent run` in the VM (AGT):** the pipeline moves in, git goes through the gateway,
   the harness configs drop their sandboxes, and the environment's setup, up, ready and down run.
   Testable in a plain container with a fake gateway.
5. **The gateway (AGT):** CONNECT plus selective TLS interception, per-host policy, credential
   injection, the egress log. Table tests per host row in §3.
6. **`factory-agent supervise` (AGT):** the workspace lifecycle on nerdctl and Kata, the
   network and iptables, limits, concurrency, the event relay, teardown. E2E on a Linux CI runner
   with KVM, or on the agent host VM.
7. **Dispatcher (RT):** run tokens, the credential bundle in leases (git and dispatcher tokens), enrollment
   endpoints, `factory agent enroll`.
8. **Catalog and skills (CAT, ccf-skills 0.9.0):** release-docs, docs-review and implement
   declare CCF's environment (local-dev ro, `make demo-up-showcase`); HEADLESS.md and the docs
   skills follow.
9. **Removal (all):** everything in the table above, and the LLDs (worker → supervise,
   run-pipeline, harness-adapters, local-stack → environment), LOCAL-GUIDE.

**Ordering.** 0 → 1 → (2, 3, 5) → 4 → 6 → 8, with 7 alongside 5 and 6, and 9 last. Until 9, the
old host runtime stays selectable (`--runtime host`), so the PoC keeps running while this lands.

## Testing

- **Gateway:** table tests per §3 row:
  - credential added only for the right host, repo and method;
  - push refused for ro repos;
  - GitHub API writes refused;
  - denied ranges blocked;
  - no secret in the egress log.
- **No credential in the VM:** a fake harness that dumps its environment and greps the
  filesystem for token-shaped strings; the test fails on any hit.
- **Isolation (E2E on Linux with KVM),** checked on the running workspace:
  - its only route out is the gateway (iptables);
  - no host devices;
  - the agent host's filesystem isn't reachable;
  - the bundle is read-only.
- **E2E without real LLMs:**
  - harnesstest's git server and fake dispatcher;
  - a fake harness image that brings up a trivial `environment.up` (a compose file with one
    nginx), clones, edits, pushes through the Effector and writes a Result.
- **Smoke tests on the agent host:** `make smoke-claude` and `make smoke-codex` in a real
  workspace, with a real TaskSpec schema (IMPROVEMENTS D), and once a local-dev `up`.

## Decisions (Gustavo, 2026-10-21)

1. **microVMs only:** Kata + Cloud Hypervisor, no container-or-VM hybrid, so that every task
   CCF needs (local-dev, kind, testcontainers) can run.
2. **Two agent hosts, four workspaces in all:**
   - **Gustavo's M3+ Mac: 1 workspace.** The agent host VM is sized for one local-dev-with-kind
     workspace plus overhead: about 12 GB of RAM and 6 vCPUs (Phase 0 confirms).
     `lending.maxConcurrent: 1`.
   - **Gustavo's Linux workstation (64 GiB): 3 workspaces**, running directly on the host.
     `maxConcurrent: 3`.
   - Each host is its own agent, with its own enrollment (§6) and its own gateways. The scheduler
     already spreads leases across agents.
3. **The dispatcher stays on the Mac.** The workstation's supervisor reaches it through the
   ngrok tunnel (`dispatcher: https://<tunnel>`); the Mac's agent host VM can use either. Run
   tokens and the credential bundle travel over that TLS connection, as git tokens do today.
4. **Codex subscription** (held on the agent host, per §5): if Phase 0 shows the gateway can't carry codex's ChatGPT
   subscription auth, the PoC **uses claude-code for every task** (the codex-default TaskSpecs'
   `harness.default` switches to claude-code in phase 8) until the PoC is done. No OpenAI API key.
5. **System packages:** the LLM may run `apt`/`apk` as root in its microVM. The VM is the
   boundary, and nothing is gained by restricting root inside it.
6. **Egress default: open,** except the denied ranges in §3 (link-local and metadata, RFC 1918
   and the agent host's addresses, other workspaces). Every request is logged.
7. **One gateway per workspace,** started and stopped with it.
