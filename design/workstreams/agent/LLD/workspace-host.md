# LLD — Workspace host setup: `factory-agent host` (AGT)

> Workstream: [AGT](../../definitions.md#agt-agent) · Task: P1 phase 2a · Packages: `internal/agent/host`, `internal/agent/config` (`workspace`), `cmd/factory-agent`
> Design: [P1 §7](../../../proposals/P1-agent-workspaces.md#7-setting-up-the-mac-the-tart-agent-host), [§3](../../../proposals/P1-agent-workspaces.md#3-the-gateway-every-credential-outside-the-vm), [Phase 0's gateway and egress](../../../proposals/P1-phase0-report.md#the-gateway-and-egress-on-the-mac)

## CLI

```text
factory-agent host check [--driver tart] [--json] [--config path]
factory-agent host init  [--driver tart] [--dry-run] [--yes] [--no-pf] [--remove-pf] [--config path]
```

- `host` reads only the config's `workspace` block (`config.LoadWorkspace`): no dispatcher, pool or join token is needed, and without the default config file the defaults apply. An explicit `--config` must exist. `worker` validates the block too when it's present.
- **`check`** prints one line per step, `<step> <state> <detail>`, with the states `ok`, `missing`, `outdated`, `wrong` (a step whose check fails is `wrong`, with the error) and `skipped` (the pf step, when the owner opted out: see [the opt-out](#opting-out-of-pf)). `--json` prints `[{step, state, detail}]`. Exit 0 when every step is ok or skipped, else 1; 2 for usage. A skipped step also prints `warning: <step> skipped: <detail>` on stderr.
- **`init`** checks every step, prints the plan, applies the steps that aren't ok in order, runs the root lines in one sudo script, then checks again: the exit code is that check's. When an `Apply` or the sudo script fails, `init` stops, prints a fresh check and exits 1. `--dry-run` prints the plan, the sudo script and the statuses, changes nothing, and exits 0 (1 when the platform fails).
- `--driver kata` fails with "the kata driver's host init comes in P1 phase 9".

## Steps (tart driver)

Each step is a `host.Step`: `Check`, `Plan` (the unprivileged actions, and the root shell lines), and `Apply` (the unprivileged part: downloads, verification, files staged for the root lines).

| # | Step | Wanted state | Apply |
|---|---|---|---|
| 1 | `platform` | darwin/arm64; macOS 26 or later (`sw_vers -productVersion`); ≥ 80 GiB free on the state dir's volume (`df -Pk`); `workspace.size.memory` + 4 GiB physical memory (`sysctl -n hw.memsize`) | none: a failed check stops `init` before anything changes |
| 2 | `statedir` | `~/Library/Application Support/factory-agent/`, a directory owned by the user, mode 0700 | create, chmod; one owned by someone else is left for a person |
| 3 | `tart` | `<statedir>/tart/tart.app/Contents/MacOS/tart --version` is the pin, **or** a `tart` on `PATH` is the pin or newer | download into a new temp dir under the state dir, check the sha256, extract, check `codesign --verify --deep --strict -R '=anchor apple generic and certificate leaf[subject.OU] = "9M2P8L4D89"'` (the chain is anchored at Apple, so a self-made certificate with that OU fails; `-dv`'s `TeamIdentifier` alone comes from whatever certificate the bundle carries) and `TeamIdentifier=9M2P8L4D89` in `codesign -dv` (for the error message), then move `tart.app` into `<statedir>/tart/`. Any failure removes the temp dir |
| 4 | `softnet` | `/usr/local/libexec/factory-agent/softnet`: a regular file, root:wheel, mode 4755, with the pinned binary's sha256 | download, check the tarball's sha256, extract, check the binary's sha256, stage it; the root lines install it |
| 5 | `pf` | the anchor `/etc/pf.anchors/com.compliance-framework.factory` and the LaunchDaemon `/Library/LaunchDaemons/com.compliance-framework.factory.pf.plist` hold exactly the rendered files, `/etc/pf.conf` has the line `anchor "com.compliance-framework.factory"`, and `launchctl print system/com.compliance-framework.factory.pf` succeeds without a non-zero `last exit code` (the daemon's pfctl failed). `skipped` when it isn't installed and `<statedir>/pf.optout` exists | nothing to stage: the sudo script carries both files as heredocs |

The tart driver (P1 phase 5) runs Tart with `/usr/local/libexec/factory-agent` first on its `PATH`, so Tart finds this Softnet and not another one.

**Pins** are in `internal/agent/host/pins.go`, each with where its checksum comes from: Tart 2.41.1 (tarball sha256, Cirrus Labs' team ID) and Softnet 0.24.1 (tarball and binary sha256; Softnet isn't code-signed). A bump records the sha256 of a download made on a trusted machine.

**Downloads:** each goes into a new empty temp dir, at most 512 MiB, and its sha256 is checked before extraction. `host.Extract` (`archive/tar`, `compress/gzip`) refuses absolute paths, `..` entries, symlinks pointing outside the dir, entries under a symlink the archive made, hard links and other special entries, and more than 512 MiB in all; at the end it resolves every symlink through the `os.Root`, so one whose target goes through another symlink and out of the dir is refused too. It writes through that `os.Root`, and keeps only the 0755 mode bits.

## The sudo batch

Steps 4 and 5's root lines run in **one** `sudo /bin/sh <stage>/root.sh`, so the owner types the password once. The script starts with `set -eu`, is written 0600 into the stage dir (a temp dir, 0700) after every step's `Apply`, and is removed after. `--dry-run` prints it. Its exact lines are in `internal/agent/host/testdata/init-dry-run.golden`.

- **Softnet:** root copies the staged binary to `softnet.new` (root:wheel, 0700), checks the copy's sha256 again with `shasum`, and only then sets mode 4755 and renames it into place. A file swapped in the stage dir after `Apply` is never made SUID.
- **pf:** the anchor and the plist are **in the script**, as quoted heredocs (`<<'FACTORY_AGENT_EOF'`, so sh expands nothing) that root writes into a temp dir it makes (`mktemp -d`, removed on exit). Nothing root installs for pf comes from the user-writable stage dir, so a file swapped there can't become a root LaunchDaemon, and `--dry-run` shows the exact bytes root installs; a sha256 check of staged copies, as for Softnet's binary, would show only a hash. `pfctl -n` parses root's copy of the anchor; `install` puts the anchor and the plist in place (root:wheel, 0644); then, if `/etc/pf.conf` lacks the anchor line, it is backed up once to `/etc/pf.conf.factory-backup` and the line is **appended** (after a newline, if the file doesn't end with one); nothing else in pf.conf changes. Then the script runs the daemon's two `pfctl` commands itself, so a failure stops it (launchd runs the daemon asynchronously, so a failure there wouldn't), and finally `launchctl bootout` (ignored if not loaded) and `launchctl bootstrap system <plist>`.

## The pf rules

```
# com.compliance-framework.factory: factory workspaces reach only their gateway on the Mac.
# macOS pf has no interface groups, so the rules name bridge100 to bridge163: Softnet and vmnet
# number their bridges from bridge100 up, one per running VM.
bridges = "{ bridge100 bridge101 … bridge163 }"
pass in quick on $bridges proto tcp from any to (self) port <workspace.gatewayPort, default 3128>
pass in quick on $bridges proto udp from any to (self) port { 53, 67 }
block drop in quick on $bridges proto { tcp, udp } from any to (self)
```

Softnet's `--net-softnet-allow=@host` lets a VM reach every port on the Mac's bridge address (Phase 0: AirPlay Receiver's :5000 answered). The anchor lets the bridges reach only the gateway port, DNS and DHCP on the Mac. The LaunchDaemon (`RunAtLoad`) runs `/sbin/pfctl -E -f /etc/pf.conf && /sbin/pfctl -a com.compliance-framework.factory -f <anchor>` at boot and when bootstrapped, so it also **enables pf**, which macOS leaves off by default.

**Why the bridges are named.** macOS's pf has no interface groups: XNU's `pfi_kif_match` matches one interface, by name, so `on bridge` would match an interface literally called `bridge` and filter nothing. The macro names `bridge100` to `bridge163` (`pfFirstBridge` and `pfBridges` in `pf.go`): Softnet and vmnet number their bridges from `bridge100` up, one per running VM, and pf attaches a rule to a named interface when it appears, so a bridge made after the anchor loaded is covered. The rules don't match a subnet, because Softnet's vary (Phase 0 saw 192.168.96.x, 192.168.17.x, 192.168.83.x and 172.23.33.x). pfctl expands the macro into 320 rules (64 bridges × 5); `TestPFRules` checks that `pfctl -n` parses it on a Mac (no root needed).

**The effect on other VMs.** The rules apply to every VM on `bridge100` to `bridge163`, not only factory workspaces: other VMs' shared networking (plain Tart NAT, UTM, Docker Desktop on vmnet) gets one of those bridges too, and those VMs lose access to the Mac's other listening services (DNS, DHCP and the gateway port stay open). So `init` explains it and asks `y/N` before the pf step (`--yes` skips the question), and `--no-pf` skips the step with a warning that workspaces can then reach the Mac's listening services. A declined prompt does the same.

### Opting out of pf

`--no-pf`, or a declined prompt, records an explicit opt-out in `<statedir>/pf.optout` (0600: a comment, `date=<RFC 3339, UTC>` and `reason=--no-pf` or `reason=declined at host init's prompt`); `--dry-run` only prints that it would. While pf isn't installed and that file exists, the pf step's check is **`skipped`**, with the date and the reason. `AllOK` counts `skipped` as ok, so `host check` and `init` exit 0, and `host check` prints the warning. `host init` without `--no-pf` asks again (or installs with `--yes`); once the sudo script has installed pf, it removes the opt-out file. An installed pf is `ok` whatever the file says.

**`--remove-pf`** undoes it in one sudo script (`testdata/remove-pf.golden`): bootout and remove the LaunchDaemon, remove the anchor file, flush the anchor's rules, delete the anchor line from `/etc/pf.conf` (any other edit made since stays), and reload pf.conf (a failed reload fails the script). When pf.conf had no final newline, `init` added one before the anchor line; the delete leaves it, so when pf.conf is then exactly the backup plus a newline (and the backup doesn't end with one), the backup is copied back. The backup is removed once pf.conf is back to it. `TestPFConfRoundTrip` runs the append and the delete with `sh` on pf.confs with and without a final newline, an empty one, and one edited after `init`. pf stays enabled until the next boot.

### Verifying the rules on a Mac (manual, for the owner)

`pfctl -n` only parses the rules; it can't show that they match. After a real `host init` (pf installed):

1. `sudo pfctl -a com.compliance-framework.factory -sr | head -3` lists `pass in quick on bridge100 …` rules, and `sudo pfctl -s info` says `Status: Enabled`.
2. Make sure a non-gateway service listens on the Mac: AirPlay Receiver on :5000 (System Settings → General → AirDrop & Handoff), as in Phase 0. Start a gateway, or any listener, on the gateway port (3128 by default), e.g. `nc -lk 3128`.
3. Start a Softnet VM as the tart driver will (Phase 0's flags): `tart run --no-graphics --net-softnet-block=0.0.0.0/0 --net-softnet-allow=@host <vm>`. On the Mac, `ifconfig | grep -A4 '^bridge1'` shows its bridge (`bridge1NN`) and the Mac's address on it (`<host>`), which is also the VM's default gateway.
4. From the VM (`tart exec <vm> …` or ssh): `nc -vz -w 3 <host> 5000` **must fail** (time out: the rule drops it), and `nc -vz -w 3 <host> 3128` **must connect**.
5. `sudo pfctl -a com.compliance-framework.factory -vsr` shows non-zero `Packets` on the `block drop in quick on bridge1NN` rule for that bridge.
6. `factory-agent host init --remove-pf`, then step 4's :5000 probe connects again.

## Tests

- Table tests for every step's `Check` and `Apply` with fakes: a `Runner` that answers `sw_vers`, `sysctl`, `df`, `tart --version`, `codesign` and `launchctl` and records `sudo`; releases served by `httptest`; system paths under a temp `Env.Root`; the owner, UID, GOOS/GOARCH, `LookPath`, the prompt and the pins injected. They cover the ok, missing and outdated states, sha256 mismatches, a wrong team ID, a failed `codesign --verify` (with the `-R` requirement asserted), Softnet not SUID or not root's, the anchor drifting, `--no-pf` and a declined prompt (the opt-out, `skipped`, and its removal by a later init).
- Goldens (`-update`) for the dry run's plan and sudo script, and for `--remove-pf`'s script; `init` runs exactly one sudo and removes the script.
- The extractor: traversal, absolute paths, symlink escapes, hard links, oversize, and modes (setuid, setgid and group/other-write bits are dropped).
- The pf rules: `pfctl -n` parses them where `/sbin/pfctl` exists; the dry run's script passes `sh -n`; the pf.conf append and delete run under `sh` (see `--remove-pf`).
- `cmd/factory-agent`: usage and exit codes, `--dry-run` (no sudo, no download, no state dir), and a host where every step is ok.
- `go test -tags tartinteg ./internal/agent/host/` on a Mac: every step's `Check` on the real machine returns a status without error. It never runs `init` or sudo, and CI doesn't run it.

## Later phases

- **Phase 2b** adds two steps after `pf`: `basevm` (builds `factory-ws-base` from the provisioning script embedded in `factory-agent`) and `warmbase` (clones it into `factory-ws-warm` and pulls `workspace.warm.images`, saving `warm.kindImages` for `kind load`; rebuilt when the list or the base changes). They plug into the same `Step` list, check and plan.
- **Phase 5:** `factory-agent supervise` runs `host check` at start. For a step that isn't ok it runs `init`'s steps itself, and stops only for one that needs a person (the sudo prompt, a platform failure), saying which.
