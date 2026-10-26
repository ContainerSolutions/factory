# P1 Phase 0: spike report (agent workspaces on the Mac)

> Status: **report** (2026-10-22), no code merged. Answers the Phase 0 questions of
> [P1](P1-agent-workspaces.md) §Rollout on Gustavo's Mac: M4 Pro, 24 GiB, macOS 26.6.2.
> Tart 2.41.1, Softnet 0.24.1, Kata Containers 4.2.0, nerdctl-full 2.4.1, mitmproxy 12.2.3.
> The spike's scripts aren't committed; their findings are below. Q8 was answered on
> 2026-10-26 on the Linux workstation ([Q8](#q8-linux-workstation-dry-run)).

## Outcome

1. **Nested Kata on the Mac boots, but isn't usable.** It needs Ubuntu's 6.8 kernel in the agent
   host VM. Even then, a CPU loop in the workspace runs about 140× slower than in the agent host
   VM. Virtualization.framework then stopped the agent host VM with *"Internal Virtualization
   error"*.
2. **One Tart VM per workspace works.** There's no nesting: dockerd, compose, kind,
   testcontainers and local-dev's showcase all run unchanged inside a plain Tart Linux VM.
   Softnet gives each VM a default-deny network whose only reachable address is the Mac, where
   the gateway listens.
3. **So P1 gets two workspace drivers behind one supervisor interface:**
   - **tart** on macOS: one Tart VM per task, cloned from a base image;
   - **kata** on the Linux workstation, as designed, running on the host without nesting.

   The TaskSpec environment, the gateway rules, the run bundle and the in-workspace
   `factory-agent run` are the same for both drivers.
4. **Both subscription CLIs work behind the gateway with placeholder credentials:** claude-code
   with the OAuth token, and codex with ChatGPT auth. No real token enters the workspace. Codex
   needs its account identifiers in the VM, and the gateway must own token refresh.
5. **The Linux workstation works too** (Q8, 2026-10-26): Kata 4.2.0 with Cloud Hypervisor on
   the bare host boots a workspace in under a second, at the host's CPU speed, isolated as P1
   requires. The design is [workspace-kata.md](../workstreams/agent/LLD/workspace-kata.md).

## Answers

### Q1. Does `tart run --nested` give a Linux VM with a working `/dev/kvm`?

Yes:
- all CPUs come up at EL2 with "Hyp mode initialized successfully";
- `/dev/kvm` reports API 12, and `KVM_CREATE_VM` succeeds;
- the cirruslabs Ubuntu 24.04 image ships the 7.0 HWE kernel; the kvm group must be added for
  `admin`.

### Q2. Does Kata with Cloud Hypervisor run in it? How fast is the boot?

Only on the 6.8 kernel, and too slowly to use. What it took to get there:

- **The 4.x packaging changed.**
  - Kata 4.x ships only the Rust runtime (runtime-rs). The shim is at
    `/opt/kata/runtime-rs/bin/containerd-shim-kata-v2`.
  - The arm64 static tarball ships `cloud-hypervisor`, but no runtime-rs Cloud Hypervisor
    config. The spike rendered upstream's `configuration-clh-runtime-rs.toml.in` (tag 4.2.0).
- **On 7.0 every VMM's guest stalls on its first virtio interrupt.**
  - This happens with Cloud Hypervisor, Dragonball and Kata's QEMU alike.
  - The cause: an L2 guest whose memory is shared or file-backed (`memfd`, `/dev/shm`) never
    completes virtio I/O. Anonymous memory boots fine.
  - Kata needs shared memory for virtio-fs, and runtime-rs hard-codes `shared: true` for
    Cloud Hypervisor.
  - **Ubuntu's 6.8 GA kernel in the agent host VM fixes it.** Booting it is a GRUB default.
- **On 6.8:**
  - Kata with Cloud Hypervisor and virtio-fs works, for the rootfs and for rw and ro bind
    mounts;
  - a busybox workspace boots in about 7 s; idle, it uses about 186 MiB on the agent host;
  - a 4 vCPU / 8 GiB workspace takes 23–34 s to boot. That's longer than Kata's default
    agent-connect budget, so `reconnect_timeout_ms` had to be raised;
  - inside a 4 vCPU workspace, the guest ran at load 7 with `cloud-hypervisor` at 480% CPU, and
    a 20M-iteration Python loop took **25.2 s** (0.18 s in the agent host VM);
  - `mkfs` plus starting dockerd took about 5 minutes, and dockerd's containerd hit its own
    start timeout;
  - minutes later, Virtualization.framework stopped the agent host VM.
- **Other Kata limits on this platform:**
  - **Block hotplug:** Cloud Hypervisor on arm64 boots the guest with a device tree, so a block
    device hot-plugged with `nerdctl --device` never appears in the guest (no PCI hotplug).
  - **NIC hotplug:** Kata's QEMU path always hot-plugs the NIC onto a `pci-bridge` with
    `shpc=off`, which arm64 `virt` can't hotplug.
  - **QEMU won't start as configured:** the nested CPU exposes no PMU, so QEMU rejects `-cpu
    host,pmu=off`.

**Verdict:** Kata is out for the Mac. Bare-metal Linux is unaffected: no nesting, and x86 PCI
hotplug works. Kata stays the driver there; the Linux dry run (Q8) confirms it.

### Q3. Do Docker, local-dev and testcontainers work inside a workspace?

These results are from the Tart driver. The workspace VM is 6 vCPU / 12 GiB / 60 GB, cloned
from a base with docker-ce 29.9, compose 5.6, kind 0.33 and kubectl 1.37.

| Check | Result |
|---|---|
| `docker info` | overlayfs, cgroup v2, native: no special storage needed |
| compose (nginx, port mapped, curl) | pass, 6.8 s |
| `kind create cluster` | pass, 48.8 s including the node image pull; Ready after 16 s |
| Python testcontainers (Postgres 17.5) | pass, 21.6 s |
| **The API's own testcontainers test** (`internal/service/relational/tests`, `TestCatalog`, cross-compiled offline) | **pass**, 6.5 s |
| local-dev showcase: compose stack | UI `:8000` → 200, API `:8080` → 200 |
| local-dev showcase: seeding, workflows, dashboards, 4-node kind, in-cluster agent | pass (on the second run, see below) |

local-dev findings. They're local-dev's own issues, not the factory's:

- `compose-up-showcase` depends on `make build`, which builds **every** demo image, including
  `ccf-worker-github` from the sibling `../agent` checkout. The showcase stack itself needs no
  builds. Until local-dev drops that dependency, a TaskSpec environment has to call
  `scripts/do start_showcase` directly, or declare the `agent` repo.
- **`create-user` races the API's migrations on a fresh stack.** It runs as soon as Postgres
  accepts connections, so `ccf_users` doesn't exist yet. `wait-for-api` then can't log in and
  fails after 120 s. A second run passes. This happens on every fresh stack, cold or warm.
- `demo-go-check-github` needs real GitHub plugin tokens and the `aws` and `az` CLIs, even for
  the showcase. The spike used placeholder tokens: the showcase runs, and its GitHub plugins
  can't collect.
- `.env.example` line 31 has an unquoted value with a space, so sourcing it prints an error. It's
  harmless.

### Q4. Does claude-code work behind a TLS-intercepting gateway with a placeholder token?

**Yes** (Gustavo's run, 2026-10-22).

- **The setup:** claude-code ran in a default-deny workspace VM with
  `CLAUDE_CODE_OAUTH_TOKEN=placeholder-not-a-token`, trusting the gateway's CA through
  `NODE_EXTRA_CA_CERTS`.
- **The result:** `claude -p` answered. `POST api.anthropic.com/v1/messages` returned 200, with
  the gateway swapping in the owner's OAuth token (`"injected": "anthropic-oauth"`). The real
  token never entered the VM.
- **What else claude-code calls:**
  - with the bearer swapped in: `/api/claude_code/settings` (404),
    `/api/claude_code/policy_limits`, `/api/eval/sdk-*`, `/api/claude_cli/bootstrap` (403);
  - with no auth header: `/api/event_logging/v2/batch` and `/mcp-registry/v0/servers`;
  - tunnelled, not intercepted: `downloads.claude.ai` (the installer) and Datadog log intake.

  The gateway's Anthropic host list covered everything claude-code needed.

### Q5. Can the gateway carry codex's subscription auth?

**Yes, with two conditions** (Gustavo's run, 2026-10-22; Claude never runs the codex CLI).

- **The setup:** codex rust-v0.163.0 ran in the same default-deny VM with a fake `auth.json`:
  - `access_token` and `refresh_token` are placeholders;
  - `id_token` is an unsigned JWT;
  - `last_refresh` is set to now.

  The gateway swaps in the owner's real ChatGPT access token and `chatgpt-account-id`.
- **The result:** `codex exec` answered. `GET chatgpt.com/backend-api/codex/responses` upgraded
  to a WebSocket (101) with the token swapped in, and every other `backend-api` call returned
  200 the same way: `codex/models`, `ps/plugins/list`, `ps/mcp`, `wham/accounts/check`,
  `wham/settings/user` and analytics. There were no calls to `auth.openai.com`.
- **Condition 1: the VM needs the real account identifiers.** Codex picks its workspace locally
  from the id_token's `chatgpt_account_id`, `chatgpt_user_id` and `chatgpt_plan_type`. With
  placeholders it fails with *"selected workspace missing from routing discovery"* before
  sending the prompt. These are identifiers, not credentials, and codex doesn't verify the
  fake JWT's signature.
- **Condition 2: refresh belongs to the gateway.** The VM's `refresh_token` is a placeholder and
  `last_refresh` is fresh, so codex never refreshes. The real access token expires, so the
  gateway (the supervisor, on the owner's machine) has to refresh it from the real
  `refresh_token` and keep the rotated tokens in the supervisor's copy of the login. P1's
  gateway phase builds that.
- **Two harmless warnings:**
  - bubblewrap is missing: that's codex's own sandbox, which is off in the workspace;
  - `codex-code-mode-host` isn't installed.

  The base image can add `bubblewrap`.

So Decision 4's fallback (claude-code for every task) isn't needed: codex stays.

The shared setup for Q4 and Q5:
- the check VM is default-deny and trusts only the gateway's CA;
- the gateway swaps credentials in from 0600 files on the Mac, and logs requests without
  their values.

### Q6. What are the cold and warm start times?

| Phase | Cold (base without images) | Warm (base with images pulled) |
|---|---|---|
| `tart clone` | 0.3 s | 0.3 s |
| boot to `tart exec` | 10.1 s | 6.0 s |
| dockerd ready | 0.4 s | 0.4 s |
| showcase compose stack up | ~14 min (image pulls) | **3 s** |
| seeding + kind + in-cluster agent | 404 s | **failed** after 628 s: kind pulled `agent:0.7.1` again and passed the rollout's 10 min deadline |

- **The network dominated every cold number.** It ran at 80–300 KB/s for most of the day, with
  short fast periods: the 6.8 kernel installed in 16 s.
- **A warm base is free with Tart:** an APFS clone of a VM whose Docker cache is full. Images
  inside **kind's** nodes aren't in that cache, though. The in-cluster agent's image is pulled
  again on every fresh cluster: `ghcr.io/compliance-framework/agent:0.7.1`, which kind's
  manifests pin apart from compose's 0.9.0. On the warm run that pull alone passed the rollout's
  10-minute deadline. Warm bases need `kind load docker-image` from the VM's cache, or a
  registry mirror that kind's containerd uses.

### Q7. How much memory does a workspace with local-dev up use?

- **In the guest:** 1.9 GiB used plus 7.6 GiB page cache, out of 12 GiB, with the full showcase
  and a 4-node kind cluster running. kind's control plane uses 583 MiB, and each worker about
  170 MiB.
- **On the Mac:** both running VMs together, the workspace VM and the 4 GiB check VM, used
  4.9 GiB RSS.
- **Sizing:** 8 GiB per workspace leaves headroom. The Mac can run one workspace comfortably,
  and two at a push.

### Q8. Linux workstation dry run

**Yes: Kata on the bare workstation is fast, isolated, and runs everything a task needs**
(2026-10-26: Ubuntu 24.04.5, kernel 7.0.0-38, x86-64, 20 cores, 62 GiB; Kata 4.2.0 static,
nerdctl-full 2.4.1; Docker 29.9 already running on the host). The dry run's script set up
what `host init --driver kata` will, ran three workspaces behind stand-in gateways (a forward
proxy pinned to its VM) and ran 76 pass/fail checks and 47 measurements; the final run passed every check but the read-only
mount (below). The script isn't committed; the design it led to is
[workspace-kata.md](../workstreams/agent/LLD/workspace-kata.md).

| Check | Result |
|---|---|
| Kata's runtime-rs Cloud Hypervisor config | shipped in the amd64 static release (`configuration-clh-runtime-rs.toml`); arm64's has none |
| A workspace (6 vCPU, 8 GiB, systemd as PID 1, privileged without host devices) | commands run 0.6 s after `nerdctl run`, dockerd 2.2 s; 7 CPUs in the guest (Kata adds its default one), 8 GiB |
| CPU | a 20M-iteration Python loop: 0.3 s, as on the host (phase 0's nested Kata: 25.2 s) |
| Disk | 1.3 GB/s on the loop-mounted ext4, 927 MB/s on the virtio-fs rootfs; `mkfs` 0.1 s |
| Three workspaces at once | about 4 GiB more host memory in use; 53 GiB still available |
| Isolation (from two workspaces) | only the own gateway's port answers; refused: the gateway's other ports, the host's LAN and docker0 addresses, the other workspace and its gateway, spoofed sources (none reached a gateway), the internet directly, IPv6, metadata; no `/dev/kvm` or host disks |
| No DNS, everything through the proxy | lookups refused at once; curl, git, apt, npm, pip, `go get`, docker pulls (Docker Hub, ghcr.io), compose services by name, kind (49 s cold) with a pod pulling through the nodes' proxy |
| A warm disk | 3.7 GB of images copied in 0.6 s; kind 27 s warm |
| Docker on the host | untouched: its daemons, containers, config and networks |

What it found that the design had to change:

1. **A `:ro` bind mount is read-only only inside the guest.** Root in the VM remounted the
   bundle read-write and its write reached the host directory. No host directory is mounted
   into a workspace; the bundle goes in by tar stream, as on tart.
2. **Every exec after systemd's boot failed with EBUSY.** Kata's agent puts exec'd processes
   in the container's cgroup, where systemd had turned on controllers. The agent uses the
   cgroup's `init` child instead when it exists, so the image's entrypoint makes it before it
   starts systemd.
3. **kind's control plane never started** without `/dev/kmsg`, which privileged without host
   devices leaves out. The entrypoint makes it.
4. **Docker 29 keeps its images in containerd's store**, so the loop-mounted disk holds both
   `/var/lib/docker` and `/var/lib/containerd`.
5. **`nerdctl exec` exits 1 for any remote failure**; the status is only in its last stderr
   line (`exec failed with exit code 7`).
6. **`nerdctl rm -f` can wait forever** on a systemd PID 1 that doesn't exit. Removal is a
   bounded `stop`, a bounded `rm -f`, and a kill of the workspace's shim, hypervisor and
   virtiofsd when either hangs.
7. **Runtime-rs reads its config only from its built-in paths**; `KATA_CONF_FILE` must name one
   of them.

## The gateway and egress on the Mac

| Check, from a default-deny workspace VM | Result |
|---|---|
| Direct HTTPS to github.com | blocked |
| The LAN (192.168.1.1), another workspace VM, metadata (169.254.169.254) | blocked |
| The gateway on the VM's Softnet bridge address | reachable |
| `https://example.com` via the gateway (not a credentialed host) | 200, tunnelled, not intercepted |
| `GET api.github.com/repos/...` via the gateway | 200, intercepted, logged |
| `POST api.github.com/...` via the gateway | 403, refused by the gateway, logged as `api-write` |
| `git clone` of a public repo via the gateway | works: `info/refs` and `git-upload-pack` are logged |
| `git push` to a repo that isn't rw for the task | 403 at `info/refs?service=git-receive-pack`, before GitHub is reached |
| Host ports on the bridge address | **open** (AirPlay Receiver :5000 answered; ssh :22 was closed) |

- **Softnet:**
  - `--net-softnet-block=0.0.0.0/0` alone also blocks the gateway, so it needs
    `--net-softnet-allow=@host`;
  - each VM gets its own random /29, and the host address on it is the VM's gateway. So it's
    **one gateway per workspace, bound to that address**, which is what P1 already says;
  - Softnet has a JSON-RPC control channel (`--net-softnet-control-fd`) for changing a running
    VM's policy.
- **`@host` opens every port on the Mac's bridge address.** The tart driver's one-time
  provisioning needs a pf anchor on `bridge1xx` that allows only the gateway port. That
  provisioning is a privileged LaunchDaemon, like Softnet's own SUID setup.
- **Explicit proxy, not transparent:** the spike's gateway runs as an explicit proxy
  (`HTTPS_PROXY`). Under default-deny, a tool that ignores the proxy simply can't connect, so the
  audit stays complete. Transparent interception would need pf `rdr` on the bridges, which isn't
  needed for the PoC.

## Changes to P1

[P1](P1-agent-workspaces.md) is revised with these changes in the same PR.

1. **Where it runs** becomes two drivers:
   - **macOS: the `tart` driver.** The supervisor runs on the Mac itself; there is no always-on
     agent host VM.
     - Per task: `tart clone <base> ws-<run>`, then `tart run --no-graphics
       --net-softnet-block=0.0.0.0/0 --net-softnet-allow=@host`, then `tart exec` to start
       `factory-agent run`. Teardown is `tart stop` and `tart delete`.
     - The gateway runs on the Mac, bound to the workspace's bridge address. Subscription logins
       stay in the supervisor's 0600 state dir on the Mac.
   - **Linux: the `kata` driver**, as designed (nerdctl, Kata with Cloud Hypervisor, a CNI
     network whose only route out is the gateway).
2. **The workspace image:** one provisioning script, embedded in `factory-agent`. Each agent host
   builds its own base: a Tart VM on the Mac, an OCI image for Kata. Nothing is published, and
   TaskSpecs don't name images (P1 Decision 8).
3. **Warm starts:**
   - **tart:** warm bases are APFS clones of a base whose Docker cache holds the TaskSpec's
     images, refreshed when the image list changes. Images inside kind are preloaded with `kind
     load`.
   - **kata:** keeps the registry mirror.
4. **Network:** P1 §Gateway is unchanged. Softnet's default-deny plus `@host` does the job of
   the kata driver's iptables. Add the pf port restriction for the tart driver.
5. **Phase 2 (agent host provisioning):**
   - **Mac:** `factory-agent host init` installs Tart and Softnet (SUID), adds the pf anchor and
     builds the base VM and the warm base. One `sudo` prompt; the logins and enrollment are the
     only other manual steps.
   - **Linux:** containerd, Kata and the rendered config.
   - The always-on Linux agent host VM on the Mac is dropped.
6. **Decision 2** (Mac = 1 workspace, in an agent host VM) becomes: Mac = 1 workspace as a Tart
   VM, 8 GiB / 6 vCPU, 2 at most.
7. **Decision 4** (claude-code everywhere if codex can't be carried) isn't needed: the gateway
   carries codex. The gateway phase adds:
   - the codex account claims to the run bundle;
   - ChatGPT token refresh in the gateway, which writes the rotated tokens back to the
     supervisor's copy of the login on the agent host.
8. **The workspace base image** adds `bubblewrap`, to silence codex's sandbox warning; the
   harness sandboxes stay off inside the VM.
