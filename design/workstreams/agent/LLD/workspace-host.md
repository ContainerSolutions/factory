# LLD — Workspace host setup: `factory-agent host` (AGT)

> Workstream: [AGT](../../definitions.md#agt-agent) · Task: P1 phase 2a · Packages: `internal/agent/host`, `internal/agent/config` (`workspace`), `cmd/factory-agent`
> Design: [P1 §7](../../../proposals/P1-agent-workspaces.md#7-setting-up-the-mac-the-tart-agent-host), [§3](../../../proposals/P1-agent-workspaces.md#3-the-gateway-every-credential-outside-the-vm), [Phase 0's gateway and egress](../../../proposals/P1-phase0-report.md#the-gateway-and-egress-on-the-mac)

## CLI

```text
factory-agent host check [--driver tart] [--json] [--config path]
factory-agent host init  [--driver tart] [--dry-run] [--yes] [--no-pf] [--remove-pf] [--config path]
```

- `host` reads only the config's `workspace` block (`config.LoadWorkspace`): no dispatcher, pool or join token is needed, and without the default config file the defaults apply. An explicit `--config` must exist. `worker` validates the block too when it's present.
- **`check`** prints one line per step, `<step> <state> <detail>`, with the states `ok`, `missing`, `outdated` and `wrong` (a step whose check fails is `wrong`, with the error). `--json` prints `[{step, state, detail}]`. Exit 0 when every step is ok, else 1; 2 for usage.
- **`init`** checks every step, prints the plan, applies the steps that aren't ok in order, runs the root lines in one sudo script, then checks again: the exit code is that check's. When an `Apply` or the sudo script fails, `init` stops, prints a fresh check and exits 1. `--dry-run` prints the plan, the sudo script and the statuses, changes nothing, and exits 0 (1 when the platform fails).
- `--driver kata` fails with "the kata driver's host init comes in P1 phase 9".

## Steps (tart driver)

Each step is a `host.Step`: `Check`, `Plan` (the unprivileged actions, and the root shell lines), and `Apply` (the unprivileged part: downloads, verification, files staged for the root lines).

| # | Step | Wanted state | Apply |
|---|---|---|---|
| 1 | `platform` | darwin/arm64; macOS 26 or later (`sw_vers -productVersion`); ≥ 80 GiB free on the state dir's volume (`df -Pk`); `workspace.size.memory` + 4 GiB physical memory (`sysctl -n hw.memsize`) | none: a failed check stops `init` before anything changes |
| 2 | `statedir` | `~/Library/Application Support/factory-agent/`, a directory owned by the user, mode 0700 | create, chmod; one owned by someone else is left for a person |
| 3 | `tart` | `<statedir>/tart/tart.app/Contents/MacOS/tart --version` is the pin, **or** a `tart` on `PATH` is the pin or newer | download into a new temp dir under the state dir, check the sha256, extract, check `codesign --verify --deep --strict` and `TeamIdentifier=9M2P8L4D89` in `codesign -dv`, then move `tart.app` into `<statedir>/tart/`. Any failure removes the temp dir |
| 4 | `softnet` | `/usr/local/libexec/factory-agent/softnet`: a regular file, root:wheel, mode 4755, with the pinned binary's sha256 | download, check the tarball's sha256, extract, check the binary's sha256, stage it; the root lines install it |
| 5 | `pf` | the anchor `/etc/pf.anchors/com.compliance-framework.factory` and the LaunchDaemon `/Library/LaunchDaemons/com.compliance-framework.factory.pf.plist` hold exactly the rendered files, `/etc/pf.conf` has the line `anchor "com.compliance-framework.factory"`, and `launchctl print system/com.compliance-framework.factory.pf` succeeds | render both into the stage dir; the root lines install them |

The tart driver (P1 phase 5) runs Tart with `/usr/local/libexec/factory-agent` first on its `PATH`, so Tart finds this Softnet and not another one.

**Pins** are in `internal/agent/host/pins.go`, each with where its checksum comes from: Tart 2.41.1 (tarball sha256, Cirrus Labs' team ID) and Softnet 0.24.1 (tarball and binary sha256; Softnet isn't code-signed). A bump records the sha256 of a download made on a trusted machine.

**Downloads:** each goes into a new empty temp dir, at most 512 MiB, and its sha256 is checked before extraction. `host.Extract` (`archive/tar`, `compress/gzip`) refuses absolute paths, `..` entries, symlinks pointing outside the dir, entries under a symlink the archive made, hard links and other special entries, and more than 512 MiB in all; at the end it resolves every symlink through the `os.Root`, so one whose target goes through another symlink and out of the dir is refused too. It writes through that `os.Root`, and keeps only the 0755 mode bits.

## The sudo batch

Steps 4 and 5's root lines run in **one** `sudo /bin/sh <stage>/root.sh`, so the owner types the password once. The script starts with `set -eu`, is written 0600 into the stage dir (a temp dir, 0700) after every step's `Apply`, and is removed after. `--dry-run` prints it. Its exact lines are in `internal/agent/host/testdata/init-dry-run.golden`.

- **Softnet:** root copies the staged binary to `softnet.new` (root:wheel, 0700), checks the copy's sha256 again with `shasum`, and only then sets mode 4755 and renames it into place. A file swapped in the stage dir after `Apply` is never made SUID.
- **pf:** `pfctl -n` parses the staged anchor; `install` puts the anchor and the plist in place (root:wheel, 0644); then, if `/etc/pf.conf` lacks the anchor line, it is backed up once to `/etc/pf.conf.factory-backup` and the line is **appended** (after a newline, if the file doesn't end with one); nothing else in pf.conf changes. Finally `launchctl bootout` (ignored if not loaded) and `launchctl bootstrap system <plist>`.

## The pf rules

```
# com.compliance-framework.factory: factory workspaces reach only their gateway on the Mac.
pass in quick on bridge proto tcp from any to (self) port <workspace.gatewayPort, default 3128>
pass in quick on bridge proto udp from any to (self) port { 53, 67 }
block drop in quick on bridge proto { tcp, udp } from any to (self)
```

Softnet's `--net-softnet-allow=@host` lets a VM reach every port on the Mac's bridge address (Phase 0: AirPlay Receiver's :5000 answered). The anchor lets bridges reach only the gateway port, DNS and DHCP on the Mac. The LaunchDaemon (`RunAtLoad`) runs `/sbin/pfctl -E -f /etc/pf.conf && /sbin/pfctl -a com.compliance-framework.factory -f <anchor>` at boot and when bootstrapped, so it also **enables pf**, which macOS leaves off by default.

**The effect on other VMs.** `bridge` is pf's interface group for every `bridgeN`, so the rules also apply to other VMs' shared networking: plain Tart NAT, UTM, Docker Desktop on vmnet. Those VMs lose access to the Mac's other listening services. So `init` explains it and asks `y/N` before the pf step (`--yes` skips the question), and `--no-pf` skips the step with a warning that workspaces can then reach the Mac's listening services. A declined prompt does the same.

**`--remove-pf`** undoes it in one sudo script (`testdata/remove-pf.golden`): bootout and remove the LaunchDaemon, remove the anchor file, flush the anchor's rules, delete the anchor line from `/etc/pf.conf` (any other edit made since stays), remove the backup when pf.conf is back to it, and reload pf.conf (a failed reload fails the script). pf stays enabled until the next boot.

## Tests

- Table tests for every step's `Check` and `Apply` with fakes: a `Runner` that answers `sw_vers`, `sysctl`, `df`, `tart --version`, `codesign` and `launchctl` and records `sudo`; releases served by `httptest`; system paths under a temp `Env.Root`; the owner, UID, GOOS/GOARCH, `LookPath`, the prompt and the pins injected. They cover the ok, missing and outdated states, sha256 mismatches, a wrong team ID, Softnet not SUID or not root's, the anchor drifting, `--no-pf` and a declined prompt.
- Goldens (`-update`) for the dry run's plan and sudo script, and for `--remove-pf`'s script; `init` runs exactly one sudo and removes the script.
- The extractor: traversal, absolute paths, symlink escapes, hard links, oversize.
- `cmd/factory-agent`: usage and exit codes, `--dry-run` (no sudo, no download, no state dir), and a host where every step is ok.
- `go test -tags tartinteg ./internal/agent/host/` on a Mac: every step's `Check` on the real machine returns a status without error. It never runs `init` or sudo, and CI doesn't run it.

## Later phases

- **Phase 2b** adds two steps after `pf`: `basevm` (builds `factory-ws-base` from the provisioning script embedded in `factory-agent`) and `warmbase` (clones it into `factory-ws-warm` and pulls `workspace.warm.images`, saving `warm.kindImages` for `kind load`; rebuilt when the list or the base changes). They plug into the same `Step` list, check and plan.
- **Phase 5:** `factory-agent supervise` runs `host check` at start. For a step that isn't ok it runs `init`'s steps itself, and stops only for one that needs a person (the sudo prompt, a platform failure), saying which.
