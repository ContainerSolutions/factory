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
| The workspace firewall | `/etc/factory-agent/workspaces.nft`, loaded by `factory-agent-nft.service` (oneshot, `Before=` and `RequiredBy=factory-agent-containerd.service`, so no workspace starts without it) | root |
| The helper and its sudoers rule | `/usr/local/libexec/factory-agent/kata-helper`; `/etc/sudoers.d/factory-agent` | root |
| containerd's state | `/var/lib/factory-agent/containerd`, `/run/factory-agent/containerd/containerd.sock`; namespace `factory` | root |
| CNI | conflists in `/etc/factory-agent/cni/net.d/`, IPAM state in `/var/lib/factory-agent/cni/` | root |
| Workspaces' disks and slots | `/var/lib/factory-agent/kata/disks/<name>.img`, `/var/lib/factory-agent/kata/slots/<n>`, the warm disk `/var/lib/factory-agent/kata/warm-docker.img` | root |
| The agent's state dir | `$XDG_STATE_HOME/factory-agent`, default `~/.local/state/factory-agent` (0700): logins, runs, gateway CA, `host.lock`, `supervise.lock`, the records `katabase.json`, `katawarm.json`; during `host init`, `stage/` (0700: the downloads, about 1.3 GB, and the helper, for the root lines; not `$TMPDIR`, which may be a tmpfs), made under the host lock and removed before it's released | the owner |

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
| 1 | `platform` | linux/amd64; `/dev/kvm` is a character device; `vmx` or `svm` in `/proc/cpuinfo`; cgroup v2 (`/sys/fs/cgroup/cgroup.controllers`); systemd is PID 1 (`/run/systemd/system`); `nft`, `systemctl`, `visudo`, `zstd` (for `tar --zstd`) on `PATH`; ≥ 120 GiB free on `/var/lib`'s volume; `workspace.size.memory` + 4 GiB physical memory; no route already in the slot pool `10.213.0.0/16` | none: a failed check stops `init` |
| 2 | `statedir` | as tart's, at the XDG path | as tart's |
| 3 | `nerdctl` | `nerdctl-full/bin/nerdctl --version` and `containerd --version` report the pin | download into the stage, check the sha256; the root lines copy it into a root-owned temp dir, check the sha256 again, extract and move it into place |
| 4 | `kata` | `/opt/kata/.factory-agent` holds the pinned version and `runtime-rs/bin/containerd-shim-kata-v2` is executable. An `/opt/kata` without the marker (or a symlink) is `wrong` ("another Kata install: move it away"): `init` never replaces it, and the root lines check it again | as `nerdctl`: the 1 GB `kata-static-<v>-amd64.tar.zst`, extracted by root after its sha256 is checked again, and refused unless every entry `tar -t` lists is under `opt/kata` (and again after extraction) |
| 5 | `kataconfig` | the Kata config is exactly the rendering below. One without the rendering's first comment line is another install's: `wrong`, never replaced | root renders it from the release's `share/defaults/kata-containers/runtime-rs/configuration-clh-runtime-rs.toml` with one change, `reconnect_timeout_ms = 60000` (phase 0: an 8 GiB guest outlived the default 3 s), and a first comment line naming the source. Check renders the same from the installed release and compares (`host.RenderKataConfig`; a test runs the root lines' `sed` against it) |
| 6 | `containerd` | the two configs and two units hold exactly the rendered text; both units enabled and active | heredocs; `systemctl daemon-reload`, `enable` both, `restart` both (so a changed config takes effect; the shims and VMs outlive it), wait for both sockets (30 s) |
| 7 | `network` | `workspaces.nft` and `factory-agent-nft.service` hold the rendered text; the unit is enabled and active (no `nft list`, which needs root) | heredocs; `nft -c` of root's copy; `nft -f` the installed file (an active oneshot unit doesn't run again), `systemctl enable --now factory-agent-nft.service` |
| 8 | `privileges` | the helper is a regular file, root:root 0755, and `kata-helper protocol` prints this binary's `katahelper.Protocol`; `/etc/sudoers.d/factory-agent` is root:root 0440 and the rendered rule (its text when readable; as installed the owner can't read it, so its size), and `sudo -n -k -l kata-helper` succeeds (else `wrong`: "the sudoers rule doesn't let you run the helper"). The check runs nothing as root: `-l` only lists, sudo's default `listpw=any` lets a user with a `NOPASSWD` rule list without a password, and `-k` ignores a cached sudo login (host init's own) | see [the privileges step](#the-privileges-step) |
| 9 | `kataimage` | `<statedir>/katabase.json` names `factory-agent-base:<version>`, its `contextSHA256` is this binary's build context's, and `kata-helper image-exists` finds that tag | build it ([the image](#the-image-provisionkata)); the helper's `image-build` is bounded on this side too, at 2 hours (its own `nerdctl build` bound is 90 minutes) |
| 10 | `katawarm` | `<statedir>/katawarm.json`'s `image`, `imageBuiltAt`, `imagesSHA256` and `diskGiB` match the image record, `workspace.warm` (the same hash as tart's `warmbase.json`) and `workspace.size.disk` (every warm workspace's disk is a copy of the warm disk, so a new size rebuilds it), and `kata-helper warm-exists` finds the warm disk | build it ([the warm disk](#the-warm-disk)) |

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

`factory-agent-containerd.service` runs that containerd with `Delegate=yes`, `KillMode=process` (the shims and VMs outlive a restart, as with containerd's own unit), `LimitNOFILE=infinity` and `Environment=PATH=/opt/kata/runtime-rs/bin:/usr/local/libexec/factory-agent/nerdctl-full/bin:/usr/sbin:/usr/bin:/sbin:/bin`, so containerd finds the runtime-rs shim for `io.containerd.kata.v2`. Both units make their socket's directory first (`ExecStartPre=install -d -m 0711`), since `/run` is emptied at boot. `factory-agent-buildkit.service` (`Requires=` and `After=` the containerd unit) runs `buildkitd --config /etc/factory-agent/buildkitd.toml --addr unix:///run/factory-agent/buildkit/buildkitd.sock --root /var/lib/factory-agent/buildkit --oci-worker=false --containerd-worker=true --containerd-worker-addr=<the socket> --containerd-worker-namespace=factory --containerd-worker-net=host`. `buildkitd.toml` sets one thing the flags can't, the build steps' resolver option `[dns] options = ["no-aaaa"]`, and `--config` keeps buildkitd from reading `/etc/buildkit/buildkitd.toml`, another BuildKit's. Build steps run on the host network, and on the workstation IPv6 is advertised (a default route on `wlo1`) but blackholed to hosts such as `storage.googleapis.com`: Playwright gives each address at least 5 s, so `base.sh`'s `playwright install chromium` timed out on every attempt, where Docker's builds (an IPv4-only bridge) pass. With `no-aaaa`, glibc lookups in a build step return IPv4 addresses only; BuildKit (v0.33.1) overrides only the options in the `resolv.conf` it writes for host-network steps, so the nameservers and search domains stay the host's.

### The workspace network

Each workspace gets a **slot** `n` from 1 to 63: the bridge `fws<n>` with `10.213.<n>.1/29` (the gateway's address) and the VM at `10.213.<n>.2`, its only address in the range. One bridge per workspace keeps workspaces apart at layer 2, and the nft rules bind each VM to its address, which is the precondition [gateway.md](gateway.md#refinements-of-p1) sets on the kata driver for its client pin.

```nft
# /etc/factory-agent/workspaces.nft (factory-agent host init)
table inet factory_workspaces
delete table inet factory_workspaces
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

- **Loading it again** (`nft -f`, atomic) replaces the table: its first line creates the table when it's missing, so the second always has one to delete. That also empties `ws_allow`, which is accepted: it fails closed (a running workspace loses its gateway until its element is added again), and host init reloads only when the file or the unit isn't ok.
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

`<owner>` is the user running `init` (`SUDO_USER` is never trusted: `init` runs unprivileged and renders its own user name, which must match `^[a-z_][a-z0-9_-]{0,31}$` and isn't `root`). The helper is root-equivalent for the owner only in what its verbs do; the owner already holds the subscriptions and, on the workstation, the docker group. `init` explains this and asks `y/N` before the step, unless `--yes`. Declined, the step is left out with a warning, and so are the steps that run the helper (`kataimage`, `katawarm`), each with a warning naming why ("kataimage skipped: privileges were declined, and the image is built through the kata helper"): their `sudo -n` calls could only fail. `init` doesn't fail on them; its final check reports them as they are, so `init` and `host check` exit non-zero until the helper is installed. Unlike tart's `pf.optout`, nothing records the answer: the next `init` asks again.

`--only` never reaches that prompt when it leaves `privileges` out, so right after a passing platform check (a failed one still stops `init` first), before any prompt, plan or apply, `init` checks the other way round: when the list names `kataimage` or `katawarm` and that step isn't ok while `privileges` isn't ok either (the helper missing, out of date, or its rule not working), it fails at once and changes nothing, naming the steps, the privileges status's detail and the fix ("kataimage needs the kata helper, which isn't installed (privileges missing: …): run host init without --only (or add privileges to --only)"). `--dry-run` reports the same error (that plan can't run), and `--yes` doesn't bypass it. A full `init`, or `--only privileges,kataimage`, applies `privileges` first as usual.

`katahelper.Protocol` is a small integer bumped whenever a verb's arguments or behaviour change; `check` compares it, not the binary's sha256, so a new `factory-agent` build doesn't need sudo unless the helper changed.

## The helper: `internal/agent/katahelper`

`kata-helper <verb> [args]` runs as root under sudo, so it trusts nothing from its environment: fixed paths only, `PATH` set to its own, every argument validated before anything runs, and an exclusive flock on `/run/factory-agent/kata.lock` around anything that allocates or frees a slot. Its output to stdout is either the verb's JSON or, for `exec`, the remote command's stdout. Exit 0 on success, 2 for usage, 1 for any other failure with one line on stderr.

**Signals.** Every verb runs under `signal.NotifyContext(SIGTERM, SIGINT, SIGHUP)`: a signal ends the verb's context, which kills its command's whole process group (each command the helper runs leads its own, so `nerdctl build`'s `buildctl` dies with it; one given the helper's own stdin, `exec -i`'s, stays in the helper's group, since a background group can't read a terminal) and lets the verb remove what it made, an `image-build`'s context copy included. On the caller's side, every runner that runs `sudo` (`host`'s `ExecRunner`, host init's; `vmexec`'s, the driver's during supervise; `vmrun`'s, in the VM) cancels it the same way, through `internal/agent/sudocancel`: a cancelled call whose program is `sudo` (with or without `-n`) gets SIGTERM, which sudo relays to its command, and SIGKILL only after a grace; anything else gets SIGKILL at once. sudo can't relay a SIGKILL, and the helper, root, is out of the owner's reach, so it would run on to its own bounds. The grace is 30 s (`sudocancel.HelperGrace`) for the helper's verbs, in `host` and `vmexec`, and for the root script (`sudo /bin/sh`, whose lines are local and whose sudo, interrupted at its password prompt, restores the terminal's echo on SIGTERM); 10 s in `vmrun` ([workspace-run.md](workspace-run.md#system-setup-vmrunsystemgo)). sudo stays in `factory-agent`'s process group, so a terminal's Ctrl-C or hangup reaches it too. sudo ignores a signal sent from a process group that sudo or its command leads (sudo 1.9.15's `exec_nopty.c` and `exec_pty.c`), which `factory-agent`'s never is.

| Verb | Arguments | What it does |
|---|---|---|
| `protocol` | | prints `katahelper.Protocol` |
| `list` | | the workspaces (`ws-tsk_…` names) in containerd's `factory` namespace, one per line |
| `create` | `<name> <cpus> <memMiB> <diskGiB> warm\|empty <image>` | allocates the lowest free slot, writes `slots/<n>` (the name), makes the bridge, the conflist and the nft element, makes the disk (`cp --sparse=always` of the warm disk, or a sparse `truncate -s <diskGiB>G` file: the driver passes `workspace.size.disk`; a warm disk keeps the warm disk's size), then `nerdctl create` with the [fixed flags](#nerdctl-create); prints `{"slot": n, "vm": "10.213.n.2", "gateway": "10.213.n.1"}`. A failure undoes what it made. `<image>` is `factory-agent-base:<version>`, the driver's `Image` (the installed helper may be older than the running `factory-agent`, so it can't name the image itself); a name that already has a slot or a disk is refused |
| `start` | `<name>` | `nerdctl start` |
| `exec` | `[-i] <name> <argv…>` | `nerdctl exec [-i] --user admin <name> <argv…>`: the image's `admin`, as `tart exec`'s user, never the caller's choice (Claude Code refuses to bypass permissions as root; anything that needs root goes through `sudo -n`); stdout and stderr pass through. nerdctl exits 1 for any remote failure and prints the status only in its last stderr line (Q8 H01: `exec failed with exit code 7`), so the helper reads that line (anchored regex `exec failed with exit code ([0-9]+)"?$`) and exits with the remote status. Any other failure (nerdctl's, or the helper's own, usage included) exits 125 with `kata-helper: <error>` as its last stderr line; the driver reports exec failed only for 125 with that line, so a remote command's own 125 stays its status. The VM can forge the line, which only turns its own failure into an exec failure: the run fails either way |
| `stop` | `<name>` | `nerdctl stop -t 30` (systemd's halt, the image's stop signal) |
| `rm` | `<name>` | `stop` bounded to 60 s, then `nerdctl rm -f` bounded to 60 s; when either hangs, kills every process whose command line holds the container's id (its shim, cloud-hypervisor and virtiofsd: Q8's first run saw `rm -f` wait forever on a systemd PID 1) and retries `rm -f`; then frees the slot (nft element, conflist, bridge, IPAM state, disk). A missing workspace is no error |
| `inspect` | `<name>` | `{"slot", "vm", "gateway", "running"}` from the slot file and nerdctl |
| `image-build` | `<tag>`, the build context as a tar on stdin | copies the context into a root-only directory under `/var/lib/factory-agent/build` (regular files at its top level only, plain names, each once, a `Dockerfile` among them, at most 32 files and 16 MiB; modes 0755 or 0644), then `nerdctl build --buildkit-host unix:///run/factory-agent/buildkit/buildkitd.sock --progress plain --platform linux/amd64 -t <tag> <copy>` (90 minutes at most; its output on stderr) and removes the copy. It holds an exclusive flock on the build directory throughout (a second `image-build` fails at once) and first removes the `context-*` copies a killed one left. The context never comes from a path: a path would let the owner have root read any directory into an image |
| `image-exists`, `image-rm` | `<tag>` | `nerdctl image inspect <tag>`; `nerdctl rmi <tag>`, never forced, so nerdctl refuses while a workspace uses it |
| `warm-load` | `<name> <ref>` | `nerdctl pull --platform linux/amd64 --unpack=false <ref>` on the host, then `nerdctl save --platform linux/amd64 <ref>` streamed into `nerdctl exec -i <name> docker load` (the helper's own command, run as root in the workspace), 30 minutes at most ([the warm disk](#the-warm-disk)). `<ref>` is checked as the config checks a warm image (`config.ValidWarmImage`: at most 200 bytes of `[A-Za-z0-9._/:@-]`, starting with a letter or digit) |
| `warm-promote` | `<name>` | the workspace must be stopped (`nerdctl inspect`) and its disk a regular file; one `rename` makes it `warm-docker.img`, replacing the older one atomically, so there's never a moment without a warm disk (a `create` copying the older one keeps its open file). Its container and slot stay, for `rm` |
| `warm-exists` | | succeeds when `warm-docker.img` is a regular file |

Names are `workspace.ValidName`'s (`ws-tsk_…`), CPUs 1 to 16, memory 2048 to 65536 MiB and disk 20 to 500 GiB (the config's bounds); the warm step's build workspace is `ws-tsk_factory-warm-build`, which supervise's orphan cleanup leaves alone (`host init` holds `host.lock` while it exists, and deletes it on any exit path unless `--keep-failed`). Sizes are integers within the config's bounds. No verb takes a path: `image-build` reads its context from stdin, and a context the owner prepares runs only in BuildKit's build containers (runc, no host mounts).

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
- **The hostname** is `--hostname <name>`, which nerdctl writes to the guest's `/etc/hostname` and maps to the VM's address in its `/etc/hosts`. systemd, PID 1 in the guest, then sets the hostname from `/etc/hostname` with the characters a hostname can't have dropped (`hostname_cleanup`): `ws-tsk_<id>` becomes `ws-tsk<id>`, which nothing maps, so every `sudo` printed `unable to resolve host ws-tsk<id>` after a DNS lookup through the gateway (test T4). The image's `nss-myhostname`, before `dns`, resolves whatever the hostname is (to the guest's own addresses) with no DNS, so the helper and its protocol stay as they are.
- **Sizing:** Kata adds its `default_vcpus` (1) to the container's CPUs (Q8 H06: 6 asked, 7 in the guest; memory as asked), so the driver passes `spec.CPU - 1` (at least 1).

## The image: `provision/kata/`

Embedded in `factory-agent` with the script (`//go:embed`); the build context is written to the stage dir and streamed as a tar into `kata-helper image-build`.

- **`Dockerfile`**: `FROM docker.io/library/ubuntu:24.04@sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac`; a prelude installs `systemd systemd-sysv dbus sudo iproute2 kmod e2fsprogs util-linux`, removes the image's `ubuntu` user and creates `admin` (uid 1000, passwordless sudo, as the cirruslabs images have); `RUN FACTORY_BASE_MODE=image bash /tmp/base.sh`; `libnss-myhostname`, with `/etc/nsswitch.conf`'s line `hosts: files myhostname dns` (the package appends it after `dns`; [the hostname](#nerdctl-create) says why); then the two files below, `systemctl enable factory-docker-storage.service docker.service containerd.service`, the masks (`getty.target`, `console-getty.service`, `systemd-udevd.service`, `systemd-udev-trigger.service`), an empty `/etc/machine-id`, `STOPSIGNAL SIGRTMIN+3`, `CMD ["/usr/local/sbin/factory-init"]`.
- **`factory-init`**, PID 1 before systemd, two lines Q8 found necessary:
  - `mkdir -p /sys/fs/cgroup/init`. On cgroup v2 Kata's agent puts every exec'd process into the container cgroup's `init` child when it exists (rustjail's `cgroup_has_init_subcgroup`, Docker-in-Docker's convention), else into the container's cgroup itself; systemd turns on controllers in that cgroup, after which the kernel refuses to add a process to it, and every exec after boot failed with EBUSY.
  - `mknod -m 0600 /dev/kmsg c 1 11` when it's missing: kubelet reads it, `privileged-without-host-devices` leaves it out, and without it kind's control plane never started (Q8 T15, W02).

  Then `exec /sbin/init`.
- **`factory-docker-storage`** and its unit (`Before=containerd.service docker.service`, `RequiredBy=` both): makes `/dev/loop-control` and `/dev/loop0`–`7` when missing, `mkfs.ext4 -E lazy_itable_init=1,lazy_journal_init=1` on `/var/lib/factory/docker.img` unless it's already ext4 (Q8: 0.1 s), loop-mounts it at `/var/lib/factory/disk` and bind-mounts its `docker` and `containerd` directories on `/var/lib/docker` and `/var/lib/containerd`: Docker 29 keeps images in containerd's store (Q8 T11b), so both must be on the disk. Its `kind-images` directory goes on `/var/lib/factory/kind-images` too, so the archives the warm disk's build saves there come with the disk (the rootfs is the image's, and goes with the build workspace).
- **`base.sh`** gains an architecture and a mode (PR 2):
  - `ARCH="$(dpkg --print-architecture)"`, amd64 or arm64, and every pin a pair (`KIND_SHA256_amd64`, `KIND_SHA256_arm64`, …) from the same sources the arm64 ones cite; the release asset names per architecture (`kind-linux-amd64`, `node-…-linux-x64`, `x86_64-unknown-linux-gnu` Rust components, `codex-x86_64-unknown-linux-musl`, `@anthropic-ai/claude-code-linux-x64`, gh extensions' `linux-amd64`, vhs `Linux_x86_64`). gifski is still built from its crate on both, so there's one path.
  - `FACTORY_BASE_MODE=vm` (the default: the tart base, unchanged behaviour) or `image`: no `systemctl --now` or `start` (`enable` only, which works in a build), no `sysctl --system` (the file is written; the guest applies it at boot), no cloud-init, and the apt timers masked with `systemctl mask` only.
  - Its sha256 changes, so every Mac rebuilds its tart base once.
- **The record** `katabase.json`: `{image, contextSHA256, builtAt}`. `contextSHA256` is the sha256 of the build context's files (name and content, sorted), so any change to the Dockerfile, `base.sh` or the two scripts rebuilds the image under the same tag; a new version's tag deletes the previous version's image (as tart deletes the previous base) only when no workspace uses it: one that does is left, with a warning that names the `kata-helper image-rm` to run once none does. `image-rm` never forces.
- **CI** builds the image on every change under `internal/agent/host/provision/` (`docker build`, amd64 runner, a job of its own in `ci.yml` with a paths filter), so the script's amd64 form is tested for real; the arm64 form is still only `bash -n`'d. The job then checks the tools are on `PATH`, and that a hostname no `/etc/hosts` line maps resolves with no network and `sudo` prints nothing (`docker run --network none`).

## The warm disk

The step (10) removes a leftover build workspace (an earlier `--keep-failed`, or a killed `init`), builds a workspace from the image with an **empty** disk (`create ws-tsk_factory-warm-build <cpus> <mem> <disk> empty <image>`, `start`), waits for dockerd (`exec … sudo -n docker info`, for 3 minutes), and for each of `workspace.warm.images` runs `kata-helper warm-load <name> <ref>`: the host pulls the image through its own network and streams `nerdctl save` into the workspace's `docker load`, so the build workspace needs no route out and no gateway. Each of `warm.kindImages` is loaded the same way and saved with `docker save --platform linux/amd64 -o /var/lib/factory/kind-images/<tag with [^A-Za-z0-9._-] as _>.tar` inside the workspace (`exec … sudo -n docker save`, which writes onto the disk), then made `0644` (`exec … sudo -n chmod 0644 <archive>`), as tart's warm base does (digest-pinned refs tagged first, `sudo -n docker tag`). The save names one platform, the one `warm-load` pulled (`katahelper.WarmPlatform`): without it Docker 29 writes the image's multi-platform index, whose other platforms' manifests were never pulled, and kind's import fails on the first missing one (`ctr: content digest … not found`, test T4). The chmod is because `docker save` as root writes the archive `0600`, and the environment's `up` runs `kind load image-archive` as `admin`. Then `stop`, `warm-promote`, `rm`, and the record `katawarm.json`: `{image, imageBuiltAt, imagesSHA256, diskGiB, builtAt}`. A failure removes the build workspace unless `--keep-failed`. With no warm images the warm disk is an empty, freshly formatted one.

## The driver: `internal/agent/workspace/kata`

`kata.New(Config{Helper, Sudo, Runner, Image, DiskGiB, Warm, AgentBinary, Boot, Poll, Grace, MaxGet})` implements `workspace.Driver`. `Helper` defaults to the installed path, `Sudo` to `sudo -n`, `AgentBinary` to `os.Executable()`; `Image` (`katabase.json`'s), `DiskGiB` (`workspace.size.disk`) and `Warm` (whether `katawarm.json`'s disk exists) come from supervise. Every call is a helper verb.

| Call | What it does |
|---|---|
| `Create` | `kata-helper create <name> <cpu-1> <MiB> <diskGiB> warm <image>` (`empty` unless `Warm`) |
| `Start` | `kata-helper start`; poll `exec <name> test -S /run/systemd/private` every `Poll` until `Boot` (3 min): this also proves exec works after systemd's boot. Then the network read, as tart's: `ip -4 route show default`'s `via` and `ip -4 route get <via>`'s `src`, which must be the slot's addresses from `inspect`; then `Put` the agent binary's directory (only that file) at `/var/lib/factory/bin` and `sudo -n ln -sfn /var/lib/factory/bin/factory-agent /usr/local/bin/factory-agent` (exec runs as `admin`). Any failure destroys nothing (the runner does) but returns the error |
| `Put`, `Get`, `Exec` | tart's, over `kata-helper exec [-i] <name> …` instead of `tart exec [-i] <name> …`: the tar streams, the pid-file cancel (SIGTERM, then SIGKILL after `Grace`), the caps and `host.Extract` |
| `Stop` | `kata-helper stop` (`--keep-workspace`) |
| `Destroy` | `kata-helper rm` |
| `List` | `kata-helper list` |

**`internal/agent/workspace/vmexec`** is what the two drivers share, moved out of `workspace/tart` unchanged: the `Runner`/`Cmd`/`ExecRunner` seam, `writeTar`, the put and get scripts, the pid-file exec and signal scripts, the network read, `capWriter`, `limitedBuffer`, `lockedWriter` (also duplicated in `host`: one copy now). Each driver supplies only its command prefix (`tart exec [-i] <name>` or `sudo -n kata-helper exec [-i] <name>`) and its lifecycle verbs. The move is its own commit in PR 4, with tart's goldens unchanged.

## Supervise (PR 6)

- `--driver kata` (the default on linux), `host check` with the kata steps, the supervise lock and the orphan cleanup (`List`, `Destroy`) as for tart, leaving `ws-tsk_factory-warm-build` alone; `--warm-base` is tart's and refused with kata (a usage error with `--driver kata`, a start error when the config chose kata). A failed check exits 1 with "run `factory-agent host init --driver kata`".
- **The driver:** `kata.New` with `Image` from `katabase.json`, `DiskGiB` `workspace.size.disk`, and `Warm` when host check's `katawarm` step is ok and `katawarm.json` exists. Each workspace is at most `workspace.size` (as tart's are: a task's `limits.cpus` and `limits.memory` can make it smaller, never larger), so the memory the cap divides by is the most each workspace gets.
- **Concurrency:** kata honours `lending.maxConcurrent`, capped by the slots (63) and by memory (`(physical − 4 GiB) / workspace.size.memory`, at least 1), with a warning when the cap bites, at start and on SIGHUP; tart stays at 1. SIGHUP re-reads the physical memory (`/proc/meminfo`'s `MemTotal`; a failed read keeps the last good value, with a warning) and caps the reloaded `maxConcurrent` with it, logging the new memory and cap when the memory changed; `workspace.size` stays the start's (a change needs a restart, as the runner's does). It relies on #165's single ChatGPT refresher per supervisor, which every workspace's gateway shares: gateways refreshing the codex login on their own would spend each other's refresh tokens.
- **What the runner must hold with several workspaces at once:** per-task run directories and egress logs (it has them), the gateway CA loaded once under its lock (it is), and each gateway on its own slot's address (the driver gives that). PR 6's test runs two leases at once against a fake driver.

## PRs

Stacked, each on the one before; each passes `make reviewable` on its own.

| # | Branch | What |
|---|---|---|
| 0 | `fx/p9-0-kata-lld` | this LLD, the Q8 section of the phase 0 report, P1's kata details brought in line |
| 1 | `fx/p9-1-host-linux` | the per-OS state dir; `host.Steps` for kata with the `platform` and `statedir` steps; `host check` and `init --driver kata` run those two (the usage says the rest comes in the next PRs; `supervise --driver kata` stays refused until PR 6) |
| 2 | `fx/p9-2-base-amd64` | `base.sh`'s architecture and mode, the amd64 pins; `provision/kata/` (the Dockerfile, `factory-init`, `factory-docker-storage` and its unit); the CI image build |
| 3a | `fx/p9-3a-kata-install` | the pins, steps 3 to 5 (`nerdctl`, `kata`, `kataconfig`), the stage under the state dir, `katahelper`'s host layout |
| 3b | `fx/p9-3b-kata-services` | steps 6 and 7 (`containerd`, `network`) and their renderings |
| 3c | `fx/p9-3c-kata-helper` | step 8 (`privileges`) and its prompt; the helper's `protocol` and `list`; `main`'s `kata-helper` dispatch; the dry run's golden |
| 4a | `fx/p9-4a-vmexec` | `vmexec` (moved from tart, tart's tests and goldens unchanged); `host.LockedWriter`, the one copy |
| 4b | `fx/p9-4b-helper-verbs` | the helper's `create` and `rm`: slots, the network, the disk, undo, the hung-`rm` kill |
| 4b2 | `fx/p9-4b2-helper-exec` | the helper's `start`, `stop`, `inspect` and `exec` |
| 4c | `fx/p9-4c-kata-driver` | `workspace/kata` and its tests against a fake helper |
| 4d | `fx/p9-4d-kata-integration` | the `kataintegration` tests (run by hand once PR 5's image exists) |
| 5a | `fx/p9-5a-kata-image` | step 9 (`kataimage`): the image build; the helper's `image-build`, `image-exists` and `image-rm` |
| 5b | `fx/p9-5b-kata-warm` | step 10 (`katawarm`): the warm disk; the helper's `warm-load`, `warm-promote` and `warm-exists`; the kind archives on the disk |
| 6 | `fx/p9-6-supervise-kata` | `supervise --driver kata` with concurrency above 1, `make kata-up`, the owner's run below |
| 7 | `fx/p9-7-build-ipv4` | buildkitd's `[dns] options = ["no-aaaa"]`: the image build's steps look up IPv4 addresses only ([containerd and buildkitd](#containerd-and-buildkitd)) |

## Tests

- **Steps:** table tests with the fakes `host` already has (the `Runner` answering `systemctl`, `nft`, `visudo`, `nerdctl --version`; releases from `httptest`; system paths under `Env.Root`): every state of every step, sha256 mismatches, a foreign `/opt/kata`, a drifted config or unit, the protocol mismatch, `--only kataimage`/`katawarm` without a usable helper (fails before any prompt, plan or helper call, `--dry-run` and `--yes` too; runs with the helper ok or `privileges` in the list). Goldens for the dry run's plan and sudo script (`testdata/kata-init-dry-run.golden`); the script passes `sh -n`, the nft text `nft -c -f` where `nft` is present (in a user and network namespace: `unshare -rn`, no root), the sudoers text `visudo -cf` where present.
- **The helper:** argument validation for every verb (names, sizes, slots, the context dir); the exact nerdctl argv per verb (goldens) against a fake runner; slot allocation and release under concurrent `create`s; `exec`'s status recovery from nerdctl's message; `rm`'s timeout path (a fake nerdctl that hangs) killing by id and retrying; undo on a failed `create`.
- **The driver:** the full lifecycle against a fake helper (golden `testdata/lifecycle.golden`), tart's `Put`/`Get`/`Exec`/cancel cases reused through `vmexec`, the network read's refusals, the agent binary's `Put`.
- **`go test -tags kataintegration ./internal/agent/workspace/kata/`** on a host where `host check --driver kata` passes (the workstation; CI doesn't run it): one workspace's whole lifecycle from the real image, two at once, the isolation checks of Q8 (I01–I18 and N01) run from inside, `Destroy` of a workspace whose systemd ignores the stop.
- **The owner's run (PR 6):** [below](#the-owners-run).

## The owner's run

On the workstation, from a checkout of this branch, as the user that will own the workspaces (not root; the dispatcher up on the Mac with its tunnel, and the Pool's join token in `~/.config/factory-agent/join-token`, 0600):

1. `make kata-up HOST_INIT_FLAGS=--dry-run` to read the plan and the sudo script, then `make kata-up`: it builds `bin/factory-agent` (linux/amd64, static), asks `y/N` before installing the helper and its sudoers rule, then one sudo prompt for every root line; the image build and the warm disk take a while (the first build pulls Ubuntu and runs `base.sh`).
2. `bin/factory-agent host check`: all ten steps ok (`--driver kata` is the default on Linux). `sudo -n -k -l /usr/local/libexec/factory-agent/kata-helper` lists the rule without a password.
3. The model logins, into `~/.local/state/factory-agent/logins/`: run `claude setup-token` on its own and copy its output, then `bin/factory-agent login claude`, which asks for it: paste, then Enter and Ctrl-D (the token stays out of argv and the shell history; `xclip -o -selection clipboard |` or `wl-paste |` in front does the same where installed, and the workstation has neither). For codex, install the codex CLI on the workstation ([github.com/openai/codex](https://github.com/openai/codex)), `codex login` (with ChatGPT), then `bin/factory-agent login codex` (reads `~/.codex/auth.json`; `--auth-json <path>` for another). Not a copy of the Mac's `auth.json`: both gateways would refresh the same login, each spending the other's refresh token. Without a codex login supervise runs without codex, dropping it with a warning (claude's login is then the one it needs).
4. `~/.config/factory-agent/agent.yaml` (`chmod 600`), from `deploy/poc/agent.yaml.example`: `dispatcher` the tunnel's URL, the harnesses whose logins step 3 stored, `lending: {unlimited: true, maxConcurrent: 3}` and `workspace: {driver: kata}` (`size` and `warm` as you like; a changed `warm` makes `host check` fail `katawarm`: `make kata-up HOST_INIT_FLAGS="--only katawarm"`).
5. `go test -tags kataintegration -count=1 -v -timeout 30m ./internal/agent/workspace/kata/`: one workspace's lifecycle, two at once, the Q8 isolation checks from inside, and the hung-stop `Destroy`. Afterwards `sudo -n /usr/local/libexec/factory-agent/kata-helper list` prints nothing.
6. `bin/factory-agent supervise --log-level debug` (add `--keep-workspace` to keep a stopped workspace and `runs/<task>/` for a look), and approve the device-flow prompt. The start logs no cap warning (62 GiB less 4 fits 7 workspaces of 8 GiB) and registers `maxConcurrent: 3`.
7. From the Mac, dispatch a task (`bin/factory dispatch --event event.json --server …`, [deploy/poc/README.md](../../../../deploy/poc/README.md) §9); while it runs, `sudo -n /usr/local/libexec/factory-agent/kata-helper list` shows `ws-tsk_<id>` and `ip -br link show type bridge` its `fws<n>`; after it, neither. The run's transcript shows its `factory.egress` lines.
8. Dispatch two or three tasks at once: each gets its own slot (`fws1`, `fws2`, …) and gateway, and all complete. Then set `maxConcurrent: 10`, `pkill -HUP -f 'factory-agent supervise'`, and see the warning `lending.maxConcurrent capped … max=7`.
9. Stop with Ctrl-C (drain; a second aborts). A supervise killed with `kill -9` leaves its workspaces: the next start destroys them (`destroying a leftover workspace`).

## Risks and open questions

1. **The helper's verbs are a root API.** They take only names, sizes and the build context; the review of PR 3 and PR 4 should treat each verb as an attack surface from the owner's account.
2. **Kata 4.2.0 on kernel 7.0** ran Q8 cleanly; a host kernel or Kata bump reruns the dry run (`spike/q8/q8.sh`, kept out of the repo) before the pin moves.
3. **Docker's embedded DNS waits about 5 s** before failing an outside name in compose services (decision 6). If a task's tooling retries lookups, `daemon.json`'s `dns` can point at a resolver that answers at once.
4. **The image is 1.7 GB** before the full `base.sh` (Playwright, the GIF tools, the harnesses); the build and its pulls take minutes on the workstation's network, and every `base.sh` change rebuilds it.
5. **The build's `no-aaaa` covers glibc lookups only.** A Go program in a build step that uses Go's pure resolver ignores the option and still gets AAAA records, as do static binaries with their own resolver; Go's dialer falls back to IPv4 within 300 ms (Happy Eyeballs), so only tools that wait seconds per address, as Playwright does, stall on a host with broken IPv6. glibc before 2.36 ignores the option too (the image's Ubuntu has 2.39).
