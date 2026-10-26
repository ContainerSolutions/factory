# LLD — The kata driver: Linux agent hosts (AGT)

> Workstream: [AGT](../../definitions.md#agt-agent) · Tasks: P1 phase 9 · Packages: `internal/agent/host` (the kata steps), `internal/agent/katahelper` (the root helper), `internal/agent/workspace/kata`, `internal/agent/workspace/vmexec` (shared with tart), `internal/agent/host/provision` (the image), `cmd/factory-agent`
> Design: [P1 §1](../../../proposals/P1-agent-workspaces.md#1-factory-agent-supervise-replaces-worker), [§2](../../../proposals/P1-agent-workspaces.md#2-the-workspace-and-factory-agent-run), [§7](../../../proposals/P1-agent-workspaces.md#7-setting-up-the-mac-the-tart-agent-host), [Q8](../../../proposals/P1-phase0-report.md#q8-linux-workstation-dry-run) · Builds on: [workspace-host.md](workspace-host.md) (steps, records, the sudo batch), [supervise.md](supervise.md) (the `Driver` interface, the tart driver, the runner), [gateway.md](gateway.md) (`Clients`, egress)

Phase 9 makes an Ubuntu x86-64 machine an agent host: `factory-agent host init --driver kata` sets it up, and `factory-agent supervise --driver kata` runs each leased task in a Kata microVM (Cloud Hypervisor) on it, behind the same gateway, bundle and `factory-agent run` as the tart driver. The first such host is Gustavo's workstation (Ubuntu 24.04, 20 cores, 62 GiB, kernel 7.0), which joins the pool with `maxConcurrent: 3`. Everything here was tried by hand first in the Q8 dry run (2026-10-26): what it found is in the [phase 0 report](../../../proposals/P1-phase0-report.md#q8-linux-workstation-dry-run), and every decision below that cites a Q8 check comes from it.

## Refinements of P1

Decided with Gustavo on 2026-10-26, after Q8:

1. **A containerd of the factory's own.** The workstation already runs Docker and its containerd (2.4.2). The factory installs the pinned nerdctl-full release and runs its own containerd and buildkitd, with their own units, sockets, state and namespace, so Docker and its upgrades never touch a workspace and the factory never touches Docker (Q8 D01: Docker's daemons, containers, config and networks unchanged across the run).
2. **Root through one narrow helper.** Rootful nerdctl sets up CNI and network namespaces itself, so it must run as root. The supervisor stays the owner's process (the gateway and the logins never run as root); every root action goes through `sudo -n /usr/local/libexec/factory-agent/kata-helper <verb> …`, a root-owned copy of `factory-agent` whose verbs take only workspace names, sizes and slot numbers and build every nerdctl argument themselves ([the helper](#the-helper-internalagentkatahelper)). A sudoers drop-in allows exactly that path.
3. **The image is built, not booted.** The base is an OCI image built by BuildKit from `provision/kata/Dockerfile` and the same `base.sh` the tart base runs, in its image mode ([the image](#the-image-provisionkata)). CI can build it.
4. **A warm Docker disk replaces P1's registry mirror.** Each workspace's dockerd and containerd store live on a loop-mounted ext4 file (the rootfs is virtio-fs, where overlayfs doesn't work). The warm base's counterpart is a pre-filled copy of that file: Q8 copied 3.7 GB of images in 0.6 s (`cp --sparse=always`), and a warm `kind create cluster` took 27 s against 49 s cold. A registry mirror would have covered Docker Hub only (dockerd mirrors no other registry) and not kind's node images.
5. **The agent binary isn't in the image.** The host and the guest are both linux/amd64, so the driver copies the supervisor's own executable into each workspace at `Start`. A new `factory-agent` never rebuilds the base (the tart driver's agent-only update, #162, isn't needed here).
6. **DNS is refused, not forwarded.** Each workspace's resolver is its gateway's address, where nft rejects port 53, so a lookup fails at once (Q8 T01, I16: under 10 ms) and everything resolves through the proxy. Q8 ran curl, git, apt, npm, pip, `go get`, docker pulls from Docker Hub and ghcr.io, compose and kind (with a pod pulling through the nodes' proxy) this way. Compose services still find each other through Docker's embedded DNS (T14), which relays outside names to the gateway and waits about 5 s before failing (T14b): accepted, since those containers have no route out anyway.
7. **amd64 only.** Kata's arm64 static release ships no runtime-rs Cloud Hypervisor config (phase 0 rendered it by hand); the amd64 one does (Q8 K01). An arm64 Linux host is refused by the platform step.

## Host layout

| What | Where | Owner |
|---|---|---|
| nerdctl-full 2.4.1 (containerd, nerdctl, buildkitd, runc, CNI plugins) | `/usr/local/libexec/factory-agent/nerdctl-full/` | root |
| Kata 4.2.0 static | `/opt/kata/` (its configs name that path), with `/opt/kata/.factory-agent` holding the version | root |
| The Kata config | `/etc/kata-containers/runtime-rs/configuration.toml` (runtime-rs reads only its three built-in paths; `KATA_CONF_FILE` must name one of them) | root |
| containerd's and buildkitd's config | `/etc/factory-agent/containerd.toml`, `/etc/factory-agent/buildkitd.toml` | root |
| Their units | `factory-agent-containerd.service`, `factory-agent-buildkit.service` in `/etc/systemd/system/` | root |
| The workspace firewall | `/etc/factory-agent/workspaces.nft`, loaded by `factory-agent-nft.service` (oneshot, `Before=factory-agent-containerd.service`) | root |
| The helper and its sudoers rule | `/usr/local/libexec/factory-agent/kata-helper`; `/etc/sudoers.d/factory-agent` | root |
| containerd's state | `/var/lib/factory-agent/containerd`, `/run/factory-agent/containerd/containerd.sock`; namespace `factory` | root |
| CNI | conflists in `/etc/factory-agent/cni/net.d/`, IPAM state in `/var/lib/factory-agent/cni/` | root |
| Workspaces' disks and slots | `/var/lib/factory-agent/kata/disks/<name>.img`, `/var/lib/factory-agent/kata/slots/<n>`, the warm disk `/var/lib/factory-agent/kata/warm-docker.img` | root |
| The agent's state dir | `$XDG_STATE_HOME/factory-agent`, default `~/.local/state/factory-agent` (0700): logins, runs, gateway CA, `host.lock`, `supervise.lock`, the records `katabase.json`, `katawarm.json` | the owner |

`host.Env.StateDir()` becomes per OS: `~/Library/Application Support/factory-agent` on darwin, the XDG path above on linux; `login` and `supervise`'s `--state-dir` default follows it.

## CLI

```text
factory-agent host check [--driver kata] [--json] [--config path]
factory-agent host init  [--driver kata] [--dry-run] [--yes] [--only step,...] [--keep-failed] [--config path]
make kata-up [HOST_INIT_FLAGS="--dry-run"]
```

- The flags, the output, the exit codes, `--only`, `--keep-failed` and the host lock are [workspace-host.md](workspace-host.md#cli)'s. `--no-pf`, `--remove-pf` and `--agent-linux` are tart's and refused with `--driver kata` (the kata image carries no agent binary). `--yes` skips the [privileges prompt](#the-privileges-step).
- `--driver` defaults to the config's `workspace.driver`, which defaults to kata on linux.
- **`make kata-up`** builds `bin/factory-agent` for linux/amd64 and runs `bin/factory-agent host init --driver kata $(HOST_INIT_FLAGS)`.

## Steps (kata driver)

`host.Steps` returns these for `Driver == kata`. Root lines go into the one sudo script, as for tart; no root line takes a file from the user-writable stage dir without checking it first (each archive's sha256 is checked again by root after root copies it, as Softnet's binary is), and every file root writes from text comes as a quoted heredoc in the script, as pf's do, so `--dry-run` shows the exact bytes.

| # | Step | Wanted state | Apply |
|---|---|---|---|
| 1 | `platform` | linux/amd64; `/dev/kvm` is a character device; `vmx` or `svm` in `/proc/cpuinfo`; cgroup v2 (`/sys/fs/cgroup/cgroup.controllers`); systemd is PID 1 (`/run/systemd/system`); `nft`, `systemctl`, `visudo` on `PATH`; ≥ 120 GiB free on `/var/lib`'s volume; `workspace.size.memory` + 4 GiB physical memory; no route already in the slot pool `10.213.0.0/16` | none: a failed check stops `init` |
| 2 | `statedir` | as tart's, at the XDG path | as tart's |
| 3 | `nerdctl` | `nerdctl-full/bin/nerdctl --version` and `containerd --version` report the pin | download into the stage, check the sha256; the root lines copy it into a root-owned temp dir, check the sha256 again, extract and move it into place |
| 4 | `kata` | `/opt/kata/.factory-agent` holds the pinned version and `runtime-rs/bin/containerd-shim-kata-v2` is executable. An `/opt/kata` without the marker is `wrong` ("another Kata install: move it away"): `init` never replaces it | as `nerdctl`: the 1 GB `kata-static-<v>-amd64.tar.zst`, extracted by root after its sha256 is checked again, and refused unless every entry is under `opt/kata` |
| 5 | `kataconfig` | the Kata config is exactly the rendering below | root renders it from the release's `share/defaults/kata-containers/runtime-rs/configuration-clh-runtime-rs.toml` with one change, `reconnect_timeout_ms = 60000` (phase 0: an 8 GiB guest outlived the default 3 s), and a first comment line naming the source. Check renders the same from the installed release and compares |
| 6 | `containerd` | the two configs and two units hold exactly the rendered text; both units enabled and active | heredocs; `systemctl daemon-reload`, `enable --now` both, wait for both sockets (30 s) |
| 7 | `network` | `workspaces.nft` and `factory-agent-nft.service` hold the rendered text; the unit is enabled and active | heredocs; `systemctl enable --now factory-agent-nft.service` |
| 8 | `privileges` | the helper is a regular file, root:root 0755, and `kata-helper protocol` prints this binary's `katahelper.Protocol`; `/etc/sudoers.d/factory-agent` is the rendered rule, root:root 0440 | see [the privileges step](#the-privileges-step) |
| 9 | `kataimage` | `<statedir>/katabase.json` names `factory-agent-base:<version>`, its `contextSHA256` is this binary's build context's, and `kata-helper image-exists` finds that tag | build it ([the image](#the-image-provisionkata)) |
| 10 | `katawarm` | `<statedir>/katawarm.json`'s `image`, `imageBuiltAt` and `imagesSHA256` match the image record and `workspace.warm` (the same hash as tart's `warmbase.json`), and `kata-helper warm-exists` finds the warm disk | build it ([the warm disk](#the-warm-disk)) |

**Pins** go in `pins.go` with their source: nerdctl-full 2.4.1 (`nerdctl-full-2.4.1-linux-amd64.tar.gz`, sha256 `ff018f11ac2f1354bf7b3aaaca6e0b52d81139c746a89134744d0b7be3a0e62f`, the asset's digest in GitHub's releases API and its `SHA256SUMS`) and Kata 4.2.0 (`kata-static-4.2.0-amd64.tar.zst`, sha256 `b828904fa3f1e49ddd7dc799c72cb1503cd1e772d354c3987c8d4189b2a623a8`, the asset's digest). 4.3.x was under two weeks old when pinned.

### containerd and buildkitd

```toml
# /etc/factory-agent/containerd.toml (factory-agent host init)
version = 3
root = "/var/lib/factory-agent/containerd"
state = "/run/factory-agent/containerd"
disabled_plugins = ["io.containerd.grpc.v1.cri", "io.containerd.cri.v1.images", "io.containerd.cri.v1.runtime", "io.containerd.nri.v1.nri"]
[grpc]
  address = "/run/factory-agent/containerd/containerd.sock"
[ttrpc]
  address = "/run/factory-agent/containerd/containerd.sock.ttrpc"
[debug]
  address = "/run/factory-agent/containerd/debug.sock"
[plugins."io.containerd.internal.v1.opt"]
  path = "/var/lib/factory-agent/containerd-opt"
```

`factory-agent-containerd.service` runs that containerd with `Delegate=yes`, `KillMode=process` (the shims and VMs outlive a restart, as with containerd's own unit), `LimitNOFILE=infinity` and `Environment=PATH=/opt/kata/runtime-rs/bin:/usr/local/libexec/factory-agent/nerdctl-full/bin:/usr/sbin:/usr/bin:/sbin:/bin`, so containerd finds the runtime-rs shim for `io.containerd.kata.v2`. `factory-agent-buildkit.service` (`Requires=` and `After=` the containerd unit) runs `buildkitd --addr unix:///run/factory-agent/buildkit/buildkitd.sock --root /var/lib/factory-agent/buildkit --oci-worker=false --containerd-worker=true --containerd-worker-addr=<the socket> --containerd-worker-namespace=factory --containerd-worker-net=host`.

### The workspace network

Each workspace gets a **slot** `n` from 1 to 63: the bridge `fws<n>` with `10.213.<n>.1/29` (the gateway's address) and the VM at `10.213.<n>.2`, its only address in the range. One bridge per workspace keeps workspaces apart at layer 2, and the nft rules bind each VM to its address, which is the precondition [gateway.md](gateway.md#refinements-of-p1) sets on the kata driver for its client pin.

```nft
# /etc/factory-agent/workspaces.nft (factory-agent host init)
table inet factory_workspaces {
	set ws_allow {
		type ifname . ipv4_addr . ipv4_addr
	}
	chain input {
		type filter hook input priority filter - 10; policy accept;
		iifname != "fws*" accept
		tcp dport <workspace.gatewayPort> iifname . ip saddr . ip daddr @ws_allow accept
		udp dport 53 reject
		tcp dport 53 reject with tcp reset
		meta l4proto tcp reject with tcp reset
		reject
	}
	chain forward {
		type filter hook forward priority filter - 10; policy accept;
		iifname "fws*" drop
		oifname "fws*" drop
	}
}
```

- **What it allows:** from a workspace bridge, only TCP to the gateway port, only from that bridge's VM address to that bridge's gateway address (the set element `"fws<n>" . 10.213.<n>.2 . 10.213.<n>.1`, which the helper adds when it creates the workspace and removes when it destroys it). DNS is rejected so lookups fail at once; any other TCP gets a reset and anything else an ICMP reject. Nothing is forwarded to or from a workspace: no NAT, no route out.
- **What Q8 checked against it** (I01–I18, N01): the gateway's other ports (a canary on every host address), the host's LAN and docker0 addresses, the other workspace and its gateway, a spoofed source in the own subnet and another workspace's address (to either gateway), the internet directly, IPv6 and metadata are all refused, and no spoofed source reached a gateway's own pin.
- **The bridge is made before anything attaches** (`ip link add fws<n> type bridge`, `net.ipv6.conf.fws<n>.disable_ipv6=1`, the address, up), then the CNI conflist names it:

```json
{ "cniVersion": "1.0.0", "name": "ws-<n>",
  "plugins": [{ "type": "bridge", "bridge": "fws<n>", "isGateway": true, "ipMasq": false, "hairpinMode": false,
    "ipam": { "type": "host-local", "dataDir": "/var/lib/factory-agent/cni",
      "ranges": [[{ "subnet": "10.213.<n>.0/29", "gateway": "10.213.<n>.1", "rangeStart": "10.213.<n>.2", "rangeEnd": "10.213.<n>.2" }]],
      "routes": [{ "dst": "0.0.0.0/0" }] } }] }
```

### The privileges step

The helper is a root-owned copy of the running `factory-agent`; `main` runs `katahelper.Main` when its `argv[0]`'s base name is `kata-helper`. Apply copies `os.Executable()` into the stage and records its sha256; the root lines copy it into a root-owned temp file, check the sha256 again, then `install -o root -g root -m 0755` it into place. The sudoers rule is a heredoc written to a temp file, checked with `visudo -cf`, then installed 0440:

```text
# /etc/sudoers.d/factory-agent (factory-agent host init): the kata driver's root helper.
<owner> ALL=(root) NOPASSWD: /usr/local/libexec/factory-agent/kata-helper
```

`<owner>` is the user running `init` (`SUDO_USER` is never trusted: `init` runs unprivileged and renders its own user name, which must match `^[a-z_][a-z0-9_-]{0,31}$`). The helper is root-equivalent for the owner only in what its verbs do; the owner already holds the subscriptions and, on the workstation, the docker group. `init` explains this and asks `y/N` before the step, unless `--yes`.

`katahelper.Protocol` is a small integer bumped whenever a verb's arguments or behaviour change; `check` compares it, not the binary's sha256, so a new `factory-agent` build doesn't need sudo unless the helper changed.

## The helper: `internal/agent/katahelper`

`kata-helper <verb> [args]` runs as root under sudo, so it trusts nothing from its environment: fixed paths only, `PATH` set to its own, every argument validated before anything runs, and an exclusive flock on `/run/factory-agent/kata.lock` around anything that allocates or frees a slot. Its output to stdout is either the verb's JSON or, for `exec`, the remote command's stdout. Exit 0 on success, 2 for usage, 1 for any other failure with one line on stderr.

| Verb | Arguments | What it does |
|---|---|---|
| `protocol` | | prints `katahelper.Protocol` |
| `list` | | the workspaces (`ws-tsk_…` names) in containerd's `factory` namespace, one per line |
| `create` | `<name> <cpus> <memMiB> warm\|empty` | allocates the lowest free slot, writes `slots/<n>` (the name), makes the bridge, the conflist and the nft element, makes the disk (`cp --sparse=always` of the warm disk, or a sparse `truncate -s <workspace.size.disk>` file), then `nerdctl create` with the [fixed flags](#nerdctl-create); prints `{"slot": n, "vm": "10.213.n.2", "gateway": "10.213.n.1"}`. A failure undoes what it made |
| `start` | `<name>` | `nerdctl start` |
| `exec` | `[-i] <name> <argv…>` | `nerdctl exec [-i] <name> <argv…>`; stdout and stderr pass through. nerdctl exits 1 for any remote failure and prints the status only in its last stderr line (Q8 H01: `exec failed with exit code 7`), so the helper reads that line (anchored regex `exec failed with exit code ([0-9]+)"?$`) and exits with the remote status; any other nerdctl failure exits 125 |
| `stop` | `<name>` | `nerdctl stop -t 30` (systemd's halt, the image's stop signal) |
| `rm` | `<name>` | `stop` bounded to 60 s, then `nerdctl rm -f` bounded to 60 s; when either hangs, kills every process whose command line holds the container's id (its shim, cloud-hypervisor and virtiofsd: Q8's first run saw `rm -f` wait forever on a systemd PID 1) and retries `rm -f`; then frees the slot (nft element, conflist, bridge, IPAM state, disk). A missing workspace is no error |
| `inspect` | `<name>` | `{"slot", "vm", "gateway", "running"}` from the slot file and nerdctl |
| `image-build` | `<context dir> <tag>` | `nerdctl build --buildkit-host unix:///run/factory-agent/buildkit/buildkitd.sock -t <tag> <dir>`; `<tag>` must be `factory-agent-base:<version>` |
| `image-exists`, `image-rm` | `<tag>` | |
| `warm-load` | `<name> <ref>` | `nerdctl pull <ref>` on the host, then `nerdctl save <ref>` streamed into `exec -i <name> docker load` ([the warm disk](#the-warm-disk)) |
| `warm-promote` | `<name>` | the stopped workspace's disk becomes `warm-docker.img` (rename, after the old one is moved aside and before it's deleted) |
| `warm-exists` | | |

Names are `workspace.ValidName`'s (`ws-tsk_…`); the warm step's build workspace is `ws-tsk_factory-warm-build`, which supervise's orphan cleanup leaves alone (`host init` holds `host.lock` while it exists, and deletes it on any exit path unless `--keep-failed`). Sizes are integers within the config's bounds. No verb takes a path but `image-build`'s context dir, which must be an absolute directory; a context the owner prepares runs only in BuildKit's build containers (runc, no host mounts).

### `nerdctl create`

```text
nerdctl --address /run/factory-agent/containerd/containerd.sock --namespace factory \
  --data-root /var/lib/factory-agent/nerdctl \
  --cni-path /usr/local/libexec/factory-agent/nerdctl-full/libexec/cni --cni-netconfpath /etc/factory-agent/cni/net.d \
  create --name <name> --hostname <name> --runtime io.containerd.kata.v2 \
  --privileged --security-opt privileged-without-host-devices \
  --cpus <cpus> --memory <memMiB>m \
  --network ws-<n> --dns 10.213.<n>.1 \
  -e container=docker --tmpfs /run --tmpfs /run/lock \
  -v /var/lib/factory-agent/kata/disks/<name>.img:/var/lib/factory/docker.img \
  factory-agent-base:<version>
```

- **No host directory is ever mounted.** Q8 N02: a `:ro` bind mount is read-only only inside the guest. Root in the VM remounted it read-write and its write reached the host directory. The only mount is the workspace's own disk file, which the VM writes anyway and which is deleted with it. The bundle goes in by `Put`, as on tart.
- **Sizing:** Kata adds its `default_vcpus` (1) to the container's CPUs (Q8 H06: 6 asked, 7 in the guest; memory as asked), so the driver passes `spec.CPU - 1` (at least 1).

## The image: `provision/kata/`

Embedded in `factory-agent` with the script (`//go:embed`); the build context is written to the stage dir and built with `kata-helper image-build`.

- **`Dockerfile`**: `FROM docker.io/library/ubuntu:24.04@sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac`; a prelude installs `systemd systemd-sysv dbus sudo iproute2 kmod e2fsprogs util-linux`, removes the image's `ubuntu` user and creates `admin` (uid 1000, passwordless sudo, as the cirruslabs images have); `RUN FACTORY_BASE_MODE=image bash /tmp/base.sh`; then the two files below, `systemctl enable factory-docker-storage.service docker.service containerd.service`, the masks (`getty.target`, `console-getty.service`, `systemd-udevd.service`, `systemd-udev-trigger.service`), an empty `/etc/machine-id`, `STOPSIGNAL SIGRTMIN+3`, `CMD ["/usr/local/sbin/factory-init"]`.
- **`factory-init`**, PID 1 before systemd, two lines Q8 found necessary:
  - `mkdir -p /sys/fs/cgroup/init`. On cgroup v2 Kata's agent puts every exec'd process into the container cgroup's `init` child when it exists (rustjail's `cgroup_has_init_subcgroup`, Docker-in-Docker's convention), else into the container's cgroup itself; systemd turns on controllers in that cgroup, after which the kernel refuses to add a process to it, and every exec after boot failed with EBUSY.
  - `mknod -m 0600 /dev/kmsg c 1 11` when it's missing: kubelet reads it, `privileged-without-host-devices` leaves it out, and without it kind's control plane never started (Q8 T15, W02).

  Then `exec /sbin/init`.
- **`factory-docker-storage`** and its unit (`Before=containerd.service docker.service`, `RequiredBy=` both): makes `/dev/loop-control` and `/dev/loop0`–`7` when missing, `mkfs.ext4 -E lazy_itable_init=1,lazy_journal_init=1` on `/var/lib/factory/docker.img` unless it's already ext4 (Q8: 0.1 s), loop-mounts it at `/var/lib/factory/disk` and bind-mounts its `docker` and `containerd` directories on `/var/lib/docker` and `/var/lib/containerd`: Docker 29 keeps images in containerd's store (Q8 T11b), so both must be on the disk.
- **`base.sh`** gains an architecture and a mode (PR 2):
  - `ARCH="$(dpkg --print-architecture)"`, amd64 or arm64, and every pin a pair (`KIND_SHA256_amd64`, `KIND_SHA256_arm64`, …) from the same sources the arm64 ones cite; the release asset names per architecture (`kind-linux-amd64`, `node-…-linux-x64`, `x86_64-unknown-linux-gnu` Rust components, `codex-x86_64-unknown-linux-musl`, `@anthropic-ai/claude-code-linux-x64`, gh extensions' `linux-amd64`, vhs `Linux_x86_64`). gifski is still built from its crate on both, so there's one path.
  - `FACTORY_BASE_MODE=vm` (the default: the tart base, unchanged behaviour) or `image`: no `systemctl --now` or `start` (`enable` only, which works in a build), no `sysctl --system` (the file is written; the guest applies it at boot), no cloud-init, and the apt timers masked with `systemctl mask` only.
  - Its sha256 changes, so every Mac rebuilds its tart base once.
- **The record** `katabase.json`: `{image, contextSHA256, builtAt}`. `contextSHA256` is the sha256 of the build context's files (name and content, sorted), so any change to the Dockerfile, `base.sh` or the two scripts rebuilds the image under the same tag; a new version's tag deletes the previous version's image (as tart deletes the previous base).
- **CI** builds the image on every change under `internal/agent/host/provision/` (`docker build`, amd64 runner, a job of its own in `ci.yml` with a paths filter), so the script's amd64 form is tested for real; the arm64 form is still only `bash -n`'d.

## The warm disk

The step (10) builds a workspace from the image with an **empty** disk (`create ws-tsk_factory-warm-build <cpus> <mem> empty`, `start`), waits for dockerd, and for each of `workspace.warm.images` runs `kata-helper warm-load <name> <ref>`: the host pulls the image through its own network and streams `nerdctl save` into the workspace's `docker load`, so the build workspace needs no route out and no gateway. Each of `warm.kindImages` is loaded the same way and saved with `docker save -o /var/lib/factory/kind-images/<tag with [^A-Za-z0-9._-] as _>.tar` inside the workspace, as tart's warm base does (digest-pinned refs tagged first). Then `stop`, `warm-promote`, `rm`, and the record `katawarm.json`: `{image, imageBuiltAt, imagesSHA256, builtAt}`. A failure removes the build workspace unless `--keep-failed`. With no warm images the warm disk is an empty, freshly formatted one.

## The driver: `internal/agent/workspace/kata`

`kata.New(Config{Helper, Sudo, Runner, Image, AgentBinary, Log, Boot, Poll, Grace, MaxGet})` implements `workspace.Driver`. `Helper` defaults to the installed path, `Sudo` to `sudo -n`, `AgentBinary` to `os.Executable()`. Every call is a helper verb.

| Call | What it does |
|---|---|
| `Create` | `kata-helper create <name> <cpu-1> <MiB> warm` (warm when the warm disk exists, else `empty`) |
| `Start` | `kata-helper start`; poll `exec <name> test -S /run/systemd/private` every `Poll` until `Boot` (3 min): this also proves exec works after systemd's boot. Then the network read, as tart's: `ip -4 route show default`'s `via` and `ip -4 route get <via>`'s `src`, which must be the slot's addresses from `inspect`; then `Put` the agent binary's directory (only that file) at `/var/lib/factory/bin` and `ln -sfn /var/lib/factory/bin/factory-agent /usr/local/bin/factory-agent`. Any failure destroys nothing (the runner does) but returns the error |
| `Put`, `Get`, `Exec` | tart's, over `kata-helper exec [-i] <name> …` instead of `tart exec [-i] <name> …`: the tar streams, the pid-file cancel (SIGTERM, then SIGKILL after `Grace`), the caps and `host.Extract` |
| `Stop` | `kata-helper stop` (`--keep-workspace`) |
| `Destroy` | `kata-helper rm` |
| `List` | `kata-helper list` |

**`internal/agent/workspace/vmexec`** is what the two drivers share, moved out of `workspace/tart` unchanged: the `Runner`/`Cmd`/`ExecRunner` seam, `writeTar`, the put and get scripts, the pid-file exec and signal scripts, the network read, `capWriter`, `limitedBuffer`, `lockedWriter` (also duplicated in `host`: one copy now). Each driver supplies only its command prefix (`tart exec [-i] <name>` or `sudo -n kata-helper exec [-i] <name>`) and its lifecycle verbs. The move is its own commit in PR 4, with tart's goldens unchanged.

## Supervise (PR 6)

- `--driver kata` (the default on linux), `host check` with the kata steps, the supervise lock and the orphan cleanup (`List`, `Destroy`) as for tart; `--warm-base` is tart's and refused with kata.
- **Concurrency:** kata honours `lending.maxConcurrent`, capped by the slots (63) and by memory (`(physical − 4 GiB) / workspace.size.memory`), with a warning when the cap bites, at start and on SIGHUP; tart stays at 1. It relies on #165's single ChatGPT refresher per supervisor, which every workspace's gateway shares: gateways refreshing the codex login on their own would spend each other's refresh tokens.
- **What the runner must hold with several workspaces at once:** per-task run directories and egress logs (it has them), the gateway CA loaded once under its lock (it is), and each gateway on its own slot's address (the driver gives that). PR 6's test runs two leases at once against a fake driver.

## PRs

Stacked, each on the one before; each passes `make reviewable` on its own.

| # | Branch | What |
|---|---|---|
| 0 | `fx/p9-0-kata-lld` | this LLD, the Q8 section of the phase 0 report, P1's kata details brought in line |
| 1 | `fx/p9-1-host-linux` | the per-OS state dir; `host.Steps` for kata with the `platform` and `statedir` steps; `host check --driver kata`; `init --driver kata` refused past them ("comes in the next PRs") |
| 2 | `fx/p9-2-base-amd64` | `base.sh`'s architecture and mode, the amd64 pins; `provision/kata/` (the Dockerfile, `factory-init`, `factory-docker-storage` and its unit); the CI image build |
| 3 | `fx/p9-3-host-kata` | steps 3 to 8, the pins, the renderings and their goldens; the helper's `protocol` and `list`; `main`'s `kata-helper` dispatch |
| 4 | `fx/p9-4-kata-driver` | `vmexec` (moved from tart), the helper's workspace verbs, `workspace/kata`; the `kataintegration` tests |
| 5 | `fx/p9-5-kata-image` | steps 9 and 10: the image build, the warm disk, the helper's image and warm verbs |
| 6 | `fx/p9-6-supervise-kata` | `supervise --driver kata` with concurrency above 1, `make kata-up`, the owner's run below |

## Tests

- **Steps:** table tests with the fakes `host` already has (the `Runner` answering `systemctl`, `nft`, `visudo`, `nerdctl --version`; releases from `httptest`; system paths under `Env.Root`): every state of every step, sha256 mismatches, a foreign `/opt/kata`, a drifted config or unit, the protocol mismatch. Goldens for the dry run's plan and sudo script (`testdata/kata-init-dry-run.golden`); the script passes `sh -n`, the nft text `nft -c -f` where `nft` is present (in a user and network namespace: `unshare -rn`, no root), the sudoers text `visudo -cf` where present.
- **The helper:** argument validation for every verb (names, sizes, slots, the context dir); the exact nerdctl argv per verb (goldens) against a fake runner; slot allocation and release under concurrent `create`s; `exec`'s status recovery from nerdctl's message; `rm`'s timeout path (a fake nerdctl that hangs) killing by id and retrying; undo on a failed `create`.
- **The driver:** the full lifecycle against a fake helper (golden `testdata/lifecycle.golden`), tart's `Put`/`Get`/`Exec`/cancel cases reused through `vmexec`, the network read's refusals, the agent binary's `Put`.
- **`go test -tags kataintegration ./internal/agent/workspace/kata/`** on a host where `host check --driver kata` passes (the workstation; CI doesn't run it): one workspace's whole lifecycle from the real image, two at once, the isolation checks of Q8 (I01–I18 and N01) run from inside, `Destroy` of a workspace whose systemd ignores the stop.
- **The owner's run (PR 6):** `make kata-up`, `factory-agent host check`, the logins, `supervise`, then a real task from the dispatcher on the Mac through the tunnel.

## Risks and open questions

1. **The helper's verbs are a root API.** They take only names, sizes and the build context; the review of PR 3 and PR 4 should treat each verb as an attack surface from the owner's account.
2. **Kata 4.2.0 on kernel 7.0** ran Q8 cleanly; a host kernel or Kata bump reruns the dry run (`spike/q8/q8.sh`, kept out of the repo) before the pin moves.
3. **Docker's embedded DNS waits about 5 s** before failing an outside name in compose services (decision 6). If a task's tooling retries lookups, `daemon.json`'s `dns` can point at a resolver that answers at once.
4. **The image is 1.7 GB** before the full `base.sh` (Playwright, the GIF tools, the harnesses); the build and its pulls take minutes on the workstation's network, and every `base.sh` change rebuilds it.
