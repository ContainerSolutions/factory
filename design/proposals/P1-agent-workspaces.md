# P1: Agents run each task in a VM workspace (Tart on macOS, Kata on Linux)

> Status: **proposal, revised after Phase 0** (2026-10-22; first version 2026-10-21). For
> Gustavo's review. Workstreams: AGT (agent), RT (dispatcher), FND (contracts), CAT (catalog),
> with a ccf-skills release. Supersedes the host-side harness sandboxes and the CCF-specific local
> stack (IMPROVEMENTS rows 1–11).
>
> - **What Phase 0 changed** ([report](P1-phase0-report.md)):
>   - Nested Kata on the Mac boots, but runs about 140× slower and crashed
>     Virtualization.framework. So the Mac runs **one Tart VM per workspace** (the `tart`
>     driver), and Kata stays the driver for the Linux workstation (the `kata` driver).
>   - Both subscription CLIs work behind the gateway with placeholder credentials.
> - **This revision puts the Mac first:** the [Phases](#phases) build the `tart` path end to
>   end, then add `kata`.
> - The decisions are at the end.

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
`--privileged`, which undoes the hardening. A **VM** can: it has its own kernel, and a privileged
process inside it is privileged only inside that VM.

## Goal

- **`factory-agent` is a supervisor.** It keeps the connection to the dispatcher and creates,
  runs and destroys one **VM workspace** per task, through a **workspace driver**:
  - **`tart` on macOS:** one Tart Linux VM per task, cloned from a base VM image;
  - **`kata` on Linux:** one Kata microVM (Cloud Hypervisor) per task.
- **Each workspace** has:
  - its own kernel and its own **dockerd**, so docker, compose, kind, testcontainers and local-dev
    run unchanged;
  - the repos the TaskSpec asks for;
  - the tools of the base image; the LLM may install more (yarn, an apt package, a Playwright
    browser), since it's root in its own VM;
  - the network only through a **gateway** that holds every credential.
- **No credential enters a workspace.** The gateway adds Anthropic, OpenAI, GitHub and the
  dispatcher's tokens to the requests that need them, much as the Effector's MCP server speaks to
  the dispatcher on the agent's behalf today. Subscription logins stay on the owner's agent host
  and never reach the dispatcher (§5, ADR-0008).
- **Four workspaces at once across two agent hosts** (1 on the Mac, 3 on the Linux workstation),
  sharing Gustavo's subscriptions through their gateways.
- **The TaskSpec declares the environment as commands:** repos, setup, up, ready, what it
  exposes, and egress. The factory core knows nothing about CCF, trains or local-dev, and the
  catalog never names a VM image (Decision 8).
- **No device flow.** An agent enrolls with a token an admin issues.

## Where it runs

```
 Gustavo's Mac (M4 Pro, macOS 26)                       Linux workstation (64 GiB)
 ┌───────────────────────────────────────────────┐      ┌──────────────────────────────┐
 │ factory serve (dispatcher), ngrok             │      │ factory-agent supervise      │
 │ factory-agent supervise --driver tart         │◄─────┤   --driver kata (via ngrok)  │
 │   gateways (one per workspace, on its bridge) │      │ containerd + nerdctl + Kata  │
 │   Tart + Softnet; base VM image; warm base    │      │ gateways, warm Docker disk   │
 │                                               │      │  ┌ Kata ws ┐ ┌ ws ┐ ┌ ws ┐   │
 │  ┌ Tart VM ws-<run> (Softnet, default-deny) ┐ │      │  └─────────┘ └────┘ └────┘   │
 │  │ factory-agent run, dockerd, the repos    │ │      └──────────────────────────────┘
 │  └──────────────────────────────────────────┘ │
 └───────────────────────────────────────────────┘
```

- **On the Mac the supervisor runs on macOS itself** (a darwin build of `factory-agent`), and
  `factory-agent host init` sets the Mac up: it installs Tart and Softnet, then builds the base
  VM locally (§7). There's
  no always-on Linux VM and no nesting: each workspace is a plain Tart Linux VM, which
  Virtualization.framework runs at full speed. Tart is a small, maintained CLI that stores VMs as
  OCI images and clones them with APFS copy-on-write (0.3 s per clone in Phase 0).
- **Softnet** (Tart's userspace packet filter, SUID) gives each VM its own /29 and a
  default-deny policy: `--net-softnet-block=0.0.0.0/0 --net-softnet-allow=@host`. The VM reaches
  only the Mac's address on its own bridge, where its gateway listens.
- **On the Linux workstation** the supervisor runs on the host with the `kata` driver: nerdctl and
  Kata with Cloud Hypervisor, a CNI network whose only route out is the gateway, and a warm
  Docker disk. It reaches the dispatcher on the Mac through the ngrok tunnel.
- **What's the same on both:** the run bundle, the gateway rules, `factory-agent run` inside the
  workspace, the TaskSpec environment, and the dispatcher contracts. A driver only creates,
  starts, streams from and destroys a VM.

## Architecture

```
  dispatcher ◄── leases, heartbeats, effects, transcripts ── supervisor (agent host)
      │                                                          │ workspace driver: tart | kata
      └─ credentials per lease (short-lived) ──────────────► gateway (one per workspace)
                                                                 ▲ only route out
  ┌────────────────────── VM workspace (Tart VM or Kata microVM) ┴───────────────┐
  │ the base image: factory-workspace (agent-host config, never the catalog)     │
  │ factory-agent run   (the in-VM runtime: pipeline, harness, Result)           │
  │ dockerd (root in the VM) → local-dev, kind, testcontainers                   │
  │ /work/repo (rw clone)   /work/ro/<name> (clones of the read repos)           │
  │ claude / codex with their own sandboxes off: the VM is the boundary          │
  │ HTTPS_PROXY = gateway, its CA trusted; no credential on disk or in env       │
  └──────────────────────────────────────────────────────────────────────────────┘
```

### 1. `factory-agent supervise` (replaces `worker`)

- **Keeps** enrollment (§6), the lease long-poll, heartbeats, the progress and usage relay,
  throttling, lending, drain and cancel. That's today's `internal/agent/worker` with a new runner.
- **The runner** (the `runner.Runner` seam) becomes `workspace.Runner`. Per lease it:
  1. writes the run bundle (§2) to a per-task directory in its state dir;
  2. asks the driver to **create** the workspace (sized from the TaskSpec's `limits`, capped by
     the agent's config), then **start** it with its network;
  3. starts the task's gateway (§3) on the workspace's gateway address;
  4. delivers the bundle, then **execs** `factory-agent run` in the workspace and streams its
     stdout (JSONL events), relaying it as heartbeat progress. Cancel is forwarded as SIGTERM,
     then SIGKILL after the grace period;
  5. collects `out/result.json` and the transcript, uploads them, and completes the lease;
  6. **destroys** the workspace and its gateway. `--keep-workspace` keeps a stopped VM for
     debugging.
- **The driver interface** is small: `Create`, `Start` (returns the gateway address), `Put`
  (copy a directory in), `Exec` (streamed, cancellable), `Get` (copy a file out), `Destroy`.
- **The `tart` driver** drives the `tart` CLI (exec, not a library, as `docker` is driven today):
  - `Create`: `tart clone <warm base> ws-<run>`, then `tart set --cpu … --memory …`;
  - `Start`: `tart run ws-<run> --no-graphics --net-softnet-block=0.0.0.0/0
    --net-softnet-allow=@host` in the background. It waits for `tart exec` to answer (about
    6–10 s), then reads the VM's default route: that's the gateway address (Softnet's host IP
    on the VM's /29);
  - `Put`/`Get`: a tar stream over `tart exec -i`. The bundle lands read-only at
    `/var/lib/factory/run` (the root disk: `/run` is a `noexec` tmpfs, and skills execute from
    the bundle);
  - `Exec`: `tart exec ws-<run> factory-agent run`;
  - `Destroy`: `tart stop`, then `tart delete`.
- **The `kata` driver** drives `nerdctl`:
  - `Create` + `Start`: `nerdctl run -d --runtime io.containerd.kata.v2 --privileged
    --security-opt privileged-without-host-devices --cpus … --memory … --network ws-<run>
    -v <bundle>:/var/lib/factory/run:ro`, with the agent host's iptables making the gateway the only
    route out;
  - `Exec`: `nerdctl exec`;
  - `Destroy`: `nerdctl rm -f`, plus the network.

  The Kata settings Phase 0 found (the rendered runtime-rs Cloud Hypervisor config, the agent
  connect timeout, and a loop device for dockerd's storage) are in the [report](P1-phase0-report.md#q8-linux-workstation-dry-run).
- **Fresh VM per task, warm by default:**
  - **tart:** the supervisor keeps a **warm base**: an APFS clone of the base VM whose Docker
    cache already holds the images in the agent config's `warm.images` list, with the
    `warm.kindImages` subset preloaded for kind. It rebuilds the warm base when the list or the
    base version changes. In Phase 0 a warm clone brought local-dev's compose stack up in 3 s;
    cold, the image pulls took about 14 minutes.
  - **kata:** each workspace's Docker disk is a copy of a pre-filled warm disk
    ([workspace-kata.md](../workstreams/agent/LLD/workspace-kata.md#the-warm-disk)). It replaces
    the registry mirror planned here first, which would have covered Docker Hub only.
- **Concurrency and limits:** up to `lending.maxConcurrent` workspaces at once: 1 on the Mac, 3
  on the workstation. local-dev with kind used 1.9 GiB in the guest plus page cache, so a
  workspace is 8 GiB / 6 vCPUs by default.

### 2. The workspace and `factory-agent run`

- **The base image is built on the agent host, not published.** The factory repo holds one
  provisioning script, embedded in `factory-agent`, and `host init` builds the base with it:
  - **tart:** clones `ghcr.io/cirruslabs/ubuntu` (pinned by digest) into
    `factory-agent-base-<version>`, boots it, runs the script, and shuts it down;
  - **kata:** builds an OCI image (Ubuntu 24.04, as the tart base) from the same script with
    BuildKit ([workspace-kata.md](../workstreams/agent/LLD/workspace-kata.md#the-image-provisionkata)).

  Both carry:
  - dockerd with the compose and buildx plugins, kind, kubectl, git, gh (with the pr-review and
    stack extensions), jq, python3, curl, make, node LTS with corepack (yarn and pnpm), go,
    Playwright's dependencies and bubblewrap;
  - the `claude` and `codex` CLIs, pinned;
  - `factory-agent` (linux/arm64 on the Mac). `make tart-up` cross-builds it alongside the
    darwin binary, and `host init` copies it in.

  The base is versioned with `factory-agent`: a new release rebuilds it. Nothing is pushed to a
  registry, and the macOS form never needs CI: GitHub's hosted macOS runners are themselves VMs,
  and Virtualization.framework can't start a VM inside one. CI builds the kata form on Linux to
  test the script.

  The base belongs to the agent host, not to the catalog. A TaskSpec can't name another image;
  tools a task needs beyond the base come from its environment's `setup` (Decision 8).
- **Docker inside:**
  - **tart:** dockerd runs on the VM's own disk (overlayfs, native);
  - **kata:** the entrypoint loop-mounts an ext4 file for `/var/lib/docker`, after creating
    `/dev/loop*` nodes, which `privileged-without-host-devices` leaves out.

  The LLM and local-dev use it like any Docker host.
- **The run bundle** (`/var/lib/factory/run`, read-only). The supervisor writes it before the run starts,
  so the workspace needs no dispatcher credential to set itself up. It holds:
  - the Task (the compiled spec);
  - the rendered prompt and context items;
  - the bundled Result schema;
  - the skills, materialised from the supervisor's cache;
  - the gateway's CA certificate and the gateway's URL;
  - for codex, the account identifiers it reads locally (account id, user id, plan type: not
    credentials, §5);
  - the run's id (`run_…`), for logs and the transcript. It's not a credential, and the
    gateway needs no identifier from the workspace: it serves exactly one. The lease's run
    token (§5) stays in the gateway and never enters the bundle.
- **`factory-agent run`** (in the VM) is today's pipeline from step 2 on:
  1. Clones the repos through the gateway: rw into `/work/repo`, ro into `/work/ro/<name>`.
     - The LLM is root in its VM, so "ro" inside the VM is advisory.
     - **The guarantee is upstream:** the gateway refuses pushes to every repo the task may not
       write (§3).
  2. Runs the environment's `setup` and `up` (§4), and waits for what it exposes.
  3. Writes the ticket files, the harness config and the MCP config for the Effector:
     - **claude:** `bypassPermissions`, no `sandbox` block, `CLAUDE_CODE_OAUTH_TOKEN` a
       placeholder;
     - **codex:** `--sandbox danger-full-access`, and a placeholder `auth.json` built from the
       bundle's account identifiers.
  4. Runs the harness, then the Result validation and repair.
  5. Writes `out/result.json` and the transcript, then runs the environment's `down`.

  Its stdout is the event stream the supervisor relays. The Effector (the `factory` CLI and MCP)
  runs here too. Its git-side ops (push, stack push and rebase) are ordinary git in the VM,
  through the gateway. **No process on the agent host ever runs git in an agent-written
  checkout,** which retires #82's hardening.

### 3. The gateway: every credential, outside the VM

One per workspace, started by the supervisor on the agent host and bound to that workspace's
gateway address (on the Mac, the host's IP on the VM's Softnet bridge). It's an explicit forward
proxy (`HTTPS_PROXY`/`HTTP_PROXY` in the VM) and the workspace's only route out. It takes
credentials from the lease and from the agent host's subscription store (§5), and keeps them in
memory. Per host:

| Host | What the gateway does |
|---|---|
| `api.anthropic.com` (also `claude.ai`, `console.anthropic.com`, `platform.claude.com`) | TLS-intercepts and replaces the bearer token with the owner's Claude OAuth token, or sets `x-api-key`. The VM carries a placeholder. Phase 0: `/v1/messages` and claude-code's settings, policy and bootstrap calls all worked this way. |
| `chatgpt.com` (codex) | TLS-intercepts and replaces the bearer token with the owner's ChatGPT access token, plus `chatgpt-account-id`. Phase 0: codex's `/backend-api/codex/responses` WebSocket and its other `backend-api` calls worked. **The gateway refreshes the access token itself** from the refresh token on the agent host; codex in the VM never refreshes, because its `last_refresh` is always fresh. |
| `github.com` (git smart HTTP) | Adds `Authorization: Basic x-access-token:<installation token>` for the repo in the URL path: the task token for its rw repo, read tokens for its read repos. **Refuses `git-receive-pack` (push) to any repo the task may not write**, at `info/refs?service=git-receive-pack`, before GitHub sees it. Public repos it can't credential pass through anonymously. |
| `api.github.com` | Adds the task's token for **GET only.** Every write (POST, PATCH, PUT, DELETE) is refused, because GitHub writes go through the Effector to the dispatcher. That turns "writes only through the Effector" from a prompt rule into a network rule. |
| the dispatcher (`FACTORY_API`) | Adds a **run-scoped token** for the routes a run needs (§5). The agent token stays in the supervisor. |
| registries (npm, yarn, PyPI, Go proxy, ghcr, Docker Hub, apt) and the rest of the internet | Plain CONNECT tunnel, no interception, allowed by default. **Denied:** link-local and metadata (169.254.0.0/16), RFC 1918 and the agent host's own addresses (except the dispatcher), and other workspaces. The TaskSpec can tighten this (`environment.egress: deny-by-default` plus an allow list). Widening past the default needs two approvals (FACTORY-CAT-002). |

- **Two layers.** Credential safety doesn't depend on the network rules: a process that bypasses
  the proxy has no credentials, so it works only as an anonymous client. The network layer
  (Softnet default-deny on the Mac, iptables on Linux) makes the gateway the only reachable
  address, so the egress log is complete.
- **The Mac's bridge ports:** `@host` lets the VM reach every port on the Mac's bridge address,
  not only the gateway's (AirPlay Receiver's :5000 answered in Phase 0). Mac provisioning
  installs a pf anchor that allows only the gateway port from Softnet bridges (§7).
- **Every request is logged** (time, method, host, path, status, which credential was injected;
  no bodies or values) into the run's transcript as `factory.egress` lines.
- **One CA per agent host** (Decision 9): it intercepts only the credentialed hosts above. Its
  public half goes in every bundle, and its key stays 0600 in the supervisor's state dir.
  Rotating it means `host init --rotate-ca`, which the next workspaces pick up.

### 4. The environment is declared in the TaskSpec

A new block, `spec.workspace.environment` (a contract change in its own PR). Because the VM has
Docker, the environment is plain commands, and local-dev runs as it is:

```yaml
workspace:
  repos:                                    # as today: rw = the task's target, ro = the rest
    - { ref: "=task.target.repo + '@main'", mode: rw }
    - { ref: compliance-framework/local-dev@main, mode: ro }
  environment:
    versions: { from: latest }               # from: latest | inputs | train (+ input: <input name>); resolved at compile time
    setup: |                                 # before the environment comes up; root in the VM
      corepack enable
    up: |                                    # FACTORY_VERSION_* are set; map them to local-dev's CCF_*_IMAGE here
      make -C /work/ro/local-dev todo-agent-env
      cd /work/ro/local-dev && ./scripts/do start_showcase
      make -C /work/ro/local-dev demo-up-showcase-load showcase-kind register-todo-agent
    exposes: { FACTORY_STACK_URL: "http://localhost:8000", FACTORY_STACK_API: "http://localhost:8080" }
    ready: { http: ["http://localhost:8080/api/health", "http://localhost:8000/"], timeout: 20m }
    down: |                                  # best effort; the VM is destroyed anyway
      make -C /work/ro/local-dev compose-down-showcase
    egress: { mode: default }                # mode: default | deny-by-default (+ allow: [hosts])
```

- **The environment is the repo's own tooling, run where it works:** a Docker host of its own,
  with its own `localhost:8000`/`:8080`. The block is CCF configuration in the catalog, not
  factory Go code. (The `up` above sidesteps two local-dev issues Phase 0 found; it becomes `make
  demo-up-showcase` once local-dev fixes them.)
- **No image field.** The environment runs in the agent host's base image (§2). A tool the base
  lacks is installed in `setup`.
- **Version sources** are resolved by the dispatcher when it compiles the task and written into
  the Task, so a run is reproducible and a Redispatch (#89) re-resolves explicitly:
  - `latest`: each component's latest final release;
  - `inputs`: a map in the task's inputs;
  - `train`: CCF's train issue named by the input, through the existing extractor.

  The resolved versions are the Task's `spec.versions`, exported in the workspace as
  `FACTORY_VERSION_<COMPONENT>`; the environment's `up` maps them to the repo's own variables
  (local-dev's `CCF_*_IMAGE`).
- **What it replaces:**
  - `internal/agent/stack`, and the agent's `localDev` config;
  - the `has-local-stack` and `gif-tooling` labels (every workspace has Docker, and tools ship in
    the base image);
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
  - **How.** The owner loads them into the supervisor's state dir once, 0600, with
    `factory-agent login`:
    - **claude:** the output of `claude setup-token`, read from stdin;
    - **codex:** imported from the owner's `~/.codex/auth.json`: tokens and account claims.

    The supervisor hands them only to its own local gateways, in memory.
  - **Codex refresh:** the gateway refreshes the ChatGPT access token and writes the rotated
    tokens back to the supervisor's copy. After the import, the supervisor's copy is the
    authoritative login. If the owner keeps using the same login in their own codex, one of the
    two will eventually hold a spent refresh token. The fix is to log in again and re-import.
  - **What enters the VM:** placeholders, plus codex's account identifiers in the bundle.
    Codex picks its workspace from them locally, and they authenticate nothing.

  This keeps ADR-0008's decision (the factory never handles subscription credentials). It
  changes only where the agent side keeps them: outside the workspace, in the gateway, instead
  of in the harness's environment and the owner's `~/.claude*`/`~/.codex`.
- **Model API keys (Anthropic, OpenAI), if a pool ever uses them: factory-managed.** They are a
  catalog `secretRef` on the Pool and travel in the lease's credential bundle. ADR-0008 already
  allows this for ephemeral pools (`Pool.llmAuth.apiKeyRef`). Not needed for the PoC.
- **The run-scoped dispatcher token:**
  - **minted per lease and stored hashed;**
  - **it authorises only:** the task's `GET` prompt, context and output schema, `POST` effects,
    and `PUT` its own transcript;
  - **it expires with the lease.**

  - **its format:** `rtk_` followed by 43 base64url characters (32 random bytes), a prefix no
    identifier uses (run IDs start with `run_`).

  It replaces `FACTORY_TOKEN` = agent token: the lease's `runToken`, for the API's
  [`runToken` security scheme](../../api/openapi.yaml) (`components.securitySchemes.runToken`).
- **Accepted risk:** the agent host holds the subscription tokens (0600 files, and in memory
  while a lease runs). A Claude OAuth token is long-lived (1 year), so a compromised agent host
  leaks it. Rotate it with `claude setup-token` and `factory-agent login claude`. The gateway
  never logs credential values, and they never reach the dispatcher.

### 6. Enrollment replaces the device flow

- **An admin mints an enrollment token** on the dispatcher:
  `factory agent enroll --pool local --owner gusfcarvalho --name mac [--ttl 7d]`. It needs the
  CLI token or a UI admin, and prints a one-time token. The dispatcher stores it hashed, with the
  owner and pool fixed by the admin.
- **The supervisor exchanges it once** (`POST /agents` with `X-Factory-Enrollment-Token`) for a
  long-lived **agent credential**: a refresh secret kept 0600 in its state dir. With it, the
  supervisor gets today's 24 h `agt_…` tokens and re-registers after a 401 or a restart with no
  human. Revoking the agent in the UI or CLI kills the refresh secret.
- **The contract** ([`api/openapi.yaml`](../../api/openapi.yaml), P1 phase 1.3): the
  `enrollmentToken` and `agentRefreshToken` security schemes on `registerAgent`, next to the join
  and owner pair until the removal phase; `AgentRegistration.refreshToken`; the admin routes
  `createEnrollment`, `listEnrollments` and `deleteEnrollment` (`/enrollments`, ids `enl_…`); and
  admin access on `deregisterAgent`. Both tokens are 32 random bytes, stored hashed:
  - the **enrollment token** is `enr_` followed by 43 base64url characters. It registers one
    agent, then it's spent, and it expires with the enrollment (`ttl`, default 7 d, at most 30 d);
  - the **refresh token** is `arf_` followed by 43 base64url characters, one per agent. Each use
    rotates it (the response carries the next one) and an admin's `deregisterAgent` revokes it.

  The dispatcher issues both from phase 6.
- **Owner attribution** (who lends the agent, and lending rules) comes from the enrollment's
  `--owner`. The admin vouches for it; no GitHub login happens on the agent side.
- **What goes away:**
  - `internal/agent/owner` (the device flow), `X-Factory-Owner-Token` and the `OwnerVerifier`
    exchange;
  - the join-token file. `Pool.admission.tokenRef` becomes "may mint enrollments for this pool".
  - `GET /auth/device-config` stays only for the UI's sign-in.

### 7. Setting up the Mac (the `tart` agent host)

**`factory-agent host init --driver tart` does the setup, and anyone with an Apple Silicon Mac
on macOS 26 can run it.** `make tart-up` in the factory repo builds `factory-agent` and runs it.
`factory-agent supervise` runs the same checks (`host check`) at start: if something is missing
or out of date, it runs the missing `init` steps itself. It stops only for a step that needs a
person, and says which.
The steps, pins, sudo batch and pf rules are in the
[workspace-host LLD](../workstreams/agent/LLD/workspace-host.md).

| Step | Phase 0 (by hand) | `host init` |
|---|---|---|
| Checks | — | Apple Silicon, macOS 26 or later (Softnet needs it), free disk and memory for `workspace.size` |
| Tart | notarized release, sha256-checked (the Homebrew tap is broken) | installs the pinned release into the state dir if it's missing or older: sha256 from `factory-agent`'s own pin list, and the Developer ID checked with `codesign` |
| Softnet | release 0.24.1, installed SUID root by hand | installs the pinned release to `/usr/local/libexec/factory-agent/softnet`, root-owned and SUID. Tart is run with that directory on its `PATH`. **Needs `sudo` once:** it prompts in the terminal |
| pf anchor | — | from Softnet bridges, only the gateway port reaches the Mac (`com.compliance-framework.factory`, loaded by a LaunchDaemon). **The same `sudo` prompt** |
| Base VM | cloned and provisioned by hand | builds `factory-agent-base-<version>` locally (§2): about 6 minutes on a good connection. See the [LLD's VM steps](../workstreams/agent/LLD/workspace-host.md#the-vm-steps-p1-2) |
| Warm base | cloned by hand | clones the base into `factory-agent-warm` and pulls `warm.images` into it, with `warm.kindImages` saved for `kind load`. Rebuilt when the list or the base changes. See the [LLD's VM steps](../workstreams/agent/LLD/workspace-host.md#the-vm-steps-p1-2) |
| Subscriptions | the spike's 0600 files | **a person, once:** `factory-agent login claude` reads `claude setup-token`'s output from stdin; `factory-agent login codex` imports `~/.codex/auth.json` |
| Enrollment | — | **a person, once:** `factory-agent supervise --enroll <token>` with a token an admin minted (§6) |
| State dir | — | `~/Library/Application Support/factory-agent/` (0700): agent credential, logins, gateway CA, Tart, run bundles |

The only steps that need a person:
- one `sudo` prompt, for Softnet's SUID bit and the pf anchor;
- the two logins;
- the enrollment token.

`host init` is idempotent, and `host init --dry-run` lists what it would change.

The agent config gains:

```yaml
workspace:
  driver: tart                      # tart | kata
  size: { cpus: 6, memory: 8Gi, disk: 60Gi }
  warm:
    images: [postgres:17.5, ghcr.io/compliance-framework/api:…, …]
    kindImages: [ghcr.io/compliance-framework/agent:0.7.1]
lending: { maxConcurrent: 1 }
```

## What gets removed

| Area | Removed |
|---|---|
| codex adapter | sandbox flags, `writable_roots`, `gitDirs` (#81), `workDirFor` (#87), MCP approval key, `CODEX_HOME` auth symlink, login probe |
| claude adapter | `sandbox` settings, `Edit`/`WebFetch` permission rules, denied reads of the owner's HOME, sandbox-support probe (`sandbox-exec`, `bwrap`/`socat`), Keychain/`USER` handling, `CLAUDE_CONFIG_DIR` as the owner's dir |
| pipeline (host) | owner `PATH`/`DOCKER_*` passthrough, the credential-helper socket (git auth moves to the gateway), the `factory` shim baking a host path, `gitsafe` (#82) |
| agent | `internal/agent/stack` (CCF local-dev), `localDev`, `has-local-stack`/`gif-tooling` probes, device flow, join-token file, `worker` (becomes `supervise`) |
| catalog / prompts | `workspace.hosts` (replaced by `environment.egress`), the "never install" rules and `not-run` stopgaps (#85), the temporary-API/UI stopgap (#88) |
| skills | HEADLESS.md's stack rules and `factory-agent stack env\|wait` |

What stays as is: the Effector op table and its dispatcher-side checks, the Result schemas and
repair, transcripts and redaction, and the dispatcher's scheduling, rails, rules and work items.
#80, #83, #84, #86 and #89 stay too.

## Phases

Each phase is one or a few PRs, through lisa and reviewed with ccf-review. **The Mac track comes
first and ends with a real task run in a Tart workspace;** the Linux track reuses everything but
the driver.

**0. Spike: done** ([report](P1-phase0-report.md)). One question is left for the Linux track:
the workstation dry run (Q8).

### Mac track (`tart`)

1. **Contracts (FND),** in this order:
   1. `TaskSpec.workspace.environment` (§4, no image field);
   2. the lease credential bundle and the `runToken` scheme (§5);
   3. enrollment: `POST /agents` with the enrollment token (§6).

   web/ regenerates its client.
2. **`host init` and the base VM (AGT):**
   - the provisioning script, embedded in `factory-agent`;
   - `host check` and `host init --driver tart` (§7): install Tart and Softnet, the pf anchor,
     the base VM and the warm base;
   - `make tart-up`.

   Tested on the Mac: Tart can't run in CI. CI runs the script's kata form on Linux.
3. **`factory-agent run` in the VM (AGT):**
   - the pipeline moves in, and git goes through the gateway;
   - the harness configs drop their sandboxes and use placeholders;
   - the environment's setup, up, ready and down run.

   Testable in a plain container with a fake gateway.
4. **The gateway (AGT, darwin and linux):**
   - CONNECT plus selective TLS interception, and the per-host policy of §3;
   - Claude and ChatGPT injection, with ChatGPT refresh;
   - the egress log.

   Table tests per row of §3. It replaces the spike's mitmproxy addon.
5. **`factory-agent supervise` with the `tart` driver (AGT):**
   - the driver interface (§1) and the tart driver;
   - limits, the event relay, teardown;
   - `factory-agent login`, and `supervise` running `host check` at start.

   E2E on the Mac, since Tart can't run in CI.
6. **Dispatcher (RT):** run tokens, the credential bundle in leases, the enrollment endpoints,
   `factory agent enroll`. Alongside 4 and 5.
7. **Catalog and skills (CAT, ccf-skills 0.9.0):**
   - release-docs, docs-review and implement declare CCF's environment (§4);
   - HEADLESS.md and the docs skills follow;
   - local-dev fixes its two showcase issues, `build` and the `create-user` race, in its own
     repo.
8. **First real run:** a mock-docs issue end to end in a Tart workspace on the Mac, with claude
   and codex. `--runtime host` is still selectable until 10.

### Linux track (`kata`)

9. **The workstation:**
   1. the Q8 dry run: the spike's Kata provisioning on the bare host (done 2026-10-26; the rest
      of this phase is [workspace-kata.md](../workstreams/agent/LLD/workspace-kata.md));
   2. `host init --driver kata` (containerd, nerdctl, Kata with the rendered Cloud Hypervisor
      config, CNI, nftables, the warm Docker disk);
   3. the kata driver;
   4. the base image's kata form, built by `host init` (CI only tests the script).

   Then the workstation joins with `maxConcurrent: 3`.

### Cleanup

10. **Removal (all):** everything in the table above, and the LLDs (worker → supervise,
    run-pipeline, harness-adapters, local-stack → environment), LOCAL-GUIDE.

**Ordering:** 1 → (2, 3, 4) → 5 → 7 → 8, with 6 alongside 4 and 5. Then 9, then 10. Until 10,
the old host runtime stays selectable (`--runtime host`), so the PoC keeps running while this
lands.

## Testing

- **Gateway:** table tests per §3 row:
  - credential added only for the right host, repo and method;
  - push refused for ro repos;
  - GitHub API writes refused;
  - denied ranges blocked;
  - no secret in the egress log;
  - ChatGPT refresh rotates the stored tokens.
- **No credential in the VM:** a fake harness that dumps its environment and greps the
  filesystem for token-shaped strings; the test fails on any hit.
- **Isolation, per driver,** checked on a running workspace:
  - **tart (on the Mac):**
    - direct egress to the internet, the LAN, metadata and other VMs is blocked;
    - only the gateway port answers on the bridge address;
    - the bundle is read-only;
  - **kata (on Linux with KVM):**
    - the only route out is the gateway (iptables);
    - no host devices;
    - the agent host's filesystem isn't reachable;
    - the bundle is read-only.
- **E2E without real LLMs:**
  - harnesstest's git server and fake dispatcher;
  - a fake harness that brings up a trivial `environment.up` (a compose file with one nginx),
    clones, edits, pushes through the Effector and writes a Result.
- **Smoke tests on the agent host:** `make smoke-claude` and `make smoke-codex` in a real
  workspace, with a real TaskSpec schema (IMPROVEMENTS D), and once a local-dev `up`.

## Decisions

From 2026-10-21, revised 2026-10-22 after Phase 0.

1. **A VM per task, through a workspace driver:**
   - **`tart` on macOS:** a Tart VM per task, no nesting;
   - **`kata` on Linux:** Kata with Cloud Hypervisor.

   Still no container-or-VM hybrid, so every task CCF needs (local-dev, kind, testcontainers)
   can run. Revised: nested Kata on the Mac was too slow and unstable.
2. **Two agent hosts, four workspaces in all:**
   - **Gustavo's Mac: 1 workspace,** a Tart VM of 8 GiB / 6 vCPUs, 2 at most. Any Mac sets
     itself up with `host init` (§7).
     `lending.maxConcurrent: 1`.
   - **Gustavo's Linux workstation (64 GiB): 3 workspaces**, Kata on the host.
     `maxConcurrent: 3`.
   - Each host is its own agent, with its own enrollment (§6) and its own gateways. The scheduler
     already spreads leases across agents.
3. **The dispatcher stays on the Mac.** The workstation's supervisor reaches it through the
   ngrok tunnel. Run tokens and the credential bundle travel over that TLS connection, as git
   tokens do today.
4. **Codex stays** (revised): Phase 0 showed the gateway carries codex's ChatGPT subscription
   auth. The gateway owns refresh (§5). The claude-code-only fallback isn't needed.
5. **System packages:** the LLM may run `apt` as root in its VM. The VM is the boundary, and
   nothing is gained by restricting root inside it.
6. **Egress default: open, through the gateway,** except the denied ranges in §3. At the network
   layer the VM reaches only its gateway. Every request is logged.
7. **One gateway per workspace,** started and stopped with it, bound to that workspace's gateway
   address.
8. **The workspace image is the agent host's, not the catalog's** (new). The factory ships the
   provisioning script in `factory-agent`, and each agent host builds its base locally; nothing
   is published. TaskSpecs declare only commands
   (`workspace.environment`), which always run inside a workspace on a registered agent; the
   dispatcher never runs them.
9. **One gateway CA per agent host** (new). A per-task CA buys little, since each VM trusts only
   its own host's CA, and the key never leaves the supervisor's state dir.
