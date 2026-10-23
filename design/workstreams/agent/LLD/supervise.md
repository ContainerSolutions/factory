# LLD — The supervisor: `factory-agent supervise` and the tart driver (AGT)

> Workstream: [AGT](../../definitions.md#agt-agent) · Tasks: P1 phase 5 · Packages: `internal/agent/workspace`, `internal/agent/workspace/tart`, `cmd/factory-agent` (`supervise`, `login`)
> Design: [P1 §1](../../../proposals/P1-agent-workspaces.md#1-factory-agent-supervise-replaces-worker), [§2](../../../proposals/P1-agent-workspaces.md#2-the-workspace-and-factory-agent-run), [§3](../../../proposals/P1-agent-workspaces.md#3-the-gateway-every-credential-outside-the-vm), [§5](../../../proposals/P1-agent-workspaces.md#5-credentials-injected-by-the-gateway-never-in-the-workspace) · Builds on: [workspace-host.md](workspace-host.md) (the warm base, Softnet, pf), [workspace-run.md](workspace-run.md#phase-5s-supervisor) (the bundle, `factory-agent run`, the trust model), [gateway.md](gateway.md) (`Config`, `Serve`, `Close`, `Sink`)

Phase 5 is the PoC's end-to-end run on the owner's Mac: a lease becomes a fresh VM cloned from the warm base, `factory-agent run` in it behind a gateway on the Mac, and the outcome back to the dispatcher. Deferred: sizing from `limits`, rebuilding the warm base, concurrency above 1, the kata driver, `host init` from `supervise`, and removing `worker`.

## Refinements of P1

- **Names are `ws-` and the task id** (`ws-tsk_…`, at most 64 of `[A-Za-z0-9_-]` after `tsk_`; `workspace.Name`). Every driver call refuses any other name and `List` returns only these, so Phase 0's VMs (`ws-1`, `ws-2`, `ws-gw`), the base and the warm base are never cloned into, stopped or deleted, the orphan cleanup included.
- **Cancel signals the remote process by pid.** `tart exec` forwards no signal (Tart 2.41.1's `Exec.swift` handles only SIGWINCH), so `Exec` runs `sh -c 'echo $$ > "$0" && exec "$@"' /tmp/factory-exec-<n>.pid argv…`, and a cancel runs another `tart exec` that sends `kill -s TERM` to the pid in that file (`sudo -n`), then `KILL` after the grace, and then stops the local `tart exec`. The grace defaults to 45 s: `factory-agent run`'s `down` gets at most 30 s after the first SIGTERM.
- **The gateway address comes from inside the VM:** `ip -4 route show default`'s `via` (Phase 0's `owner-checks.sh`), and the VM's own address is the `src` of `ip -4 route get <gateway>`. Both run at `Start`, before the bundle or any task code is in the VM. Each must be a unicast IPv4 address and they must differ; exactly one default route.

## `internal/agent/workspace` (PR 1)

`Driver`: `Create(ctx, name, Spec{CPU, MemoryMiB})` (zero fields default to 6 vCPUs and 8 GiB), `Start(ctx, name) (Network{VM, Gateway netip.Addr}, error)`, `Put(ctx, name, srcDir, dstDir)`, `Exec(ctx, name, argv, stdin, stdout, stderr) error` (a non-zero exit is `*ExitError{Code}`; a cancel returns ctx's error once the process has exited), `Get(ctx, name, srcDir, dstDir)`, `Stop(ctx, name)` (`--keep-workspace`), `Destroy(ctx, name)` (a missing VM is no error), `List(ctx) ([]string, error)`.

## The tart driver: `internal/agent/workspace/tart` (PR 1)

`tart.New(Config{Bin, WarmBase, Path, Runner, Log, Boot, Poll, Grace, MaxGet})`. `Bin` is `host.Env.TartBin` (the tart `host check` accepts: the state dir's pin, else `PATH`'s); `WarmBase` defaults to `factory-agent-warm`. Every call goes through the injected `Runner` (`Run(ctx, Cmd)`, `Start(Cmd) (host.Process, error)`, with `Cmd{Name, Args, Env, Stdin, Stdout, Stderr}`); `ExecRunner` runs real programs, its `Start` through `host.StartCmd` (own process group; `Stop` is SIGTERM, then SIGKILL after 30 s). Tests use a fake that records argv; nothing runs the real tart.

| Call | tart |
|---|---|
| `Create` | `tart clone <warm base> <name>`; `tart set <name> --cpu <n> --memory <MiB>` (a failed set deletes the clone) |
| `Start` | `PATH=/usr/local/libexec/factory-agent:$PATH tart run <name> --no-graphics --net-softnet-block=0.0.0.0/0 --net-softnet-allow=@host` in the background, so Tart runs host init's Softnet; poll `tart exec <name> true` every `Poll` until `Boot` (3 min; `tart run` exiting fails it); then the two `ip` reads. Any failure stops `tart run` |
| `Put` | `srcDir` as a tar stream (directories and regular files only, owned by root) into `tart exec -i <name> sudo -n sh -c '<script>' <dst>`: `rm -rf`, `mkdir -p`, `tar -xf - --no-same-owner`, `chown -R root:root`, `chmod -R a+rX,a-w`, so the bundle at `/run/factory` is files 0444 and directories and executables 0555. `dst` must be a clean absolute path other than `/` |
| `Exec` | `tart exec [-i] <name> sh -c '<pid script>' <pid file> argv…`, stdout and stderr kept apart (stdout is `run`'s event stream) |
| `Get` | `tart exec <name> sudo -n tar -czf - -C <src> .` into a temp file next to `dstDir`, capped at 64 MiB, then `host.Extract` into `dstDir` (made 0700; it must not exist): no absolute or `..` entries, no symlink out, no special files, 512 MiB in all. A failure removes `dstDir` |
| `Stop` | `tart stop <name>` ("is not running" is fine), then its `tart run` is reaped |
| `Destroy` | `tart list --format json`; `tart stop` if it isn't stopped; reap `tart run`; `tart delete` |
| `List` | `tart list --format json`: the local VMs with a workspace name, sorted |

Output a VM produces for the driver itself (the `ip` reads, stderr in errors) is capped (4 MiB, 8 KiB). The driver is safe for concurrent use on different workspaces.

## The workspace runner (PR 2)

- **`factory-agent login claude`** reads `claude setup-token`'s output on stdin, as pasted, and stores the one `sk-ant-oat…` token in it (none, or two different ones, is refused, naming neither). `claude setup-token` is interactive and prints prose around the token, so it runs on its own: copy its output, then `pbpaste | factory-agent login claude` (the token stays out of argv and the shell history); **`factory-agent login codex [--auth-json <path>]`** (default `~/.codex/auth.json`) imports its tokens and account claims. Each goes to `<statedir>/logins/{claude,codex}.json`, 0600, in a 0700 directory.
- **The gateway CA** is `ca.LoadOrGenerate(<statedir>/gateway-ca)` at first use.
- **`workspace.Runner`** implements `runner.Runner`. Per lease: `PrepareBundle` into `<statedir>/runs/<task>/bundle` (the codex account from the login store) → `Create` + `Start` → `gateway.New` + `Serve` on `<Gateway>:3128` with `Clients: [VM]`, `Git` the job's credentials (heartbeat refreshes apply), `Dispatcher` and `DispatcherToken` the agent token, the logins from the store, and a `Sink` writing `factory.egress` lines into the run's transcript → `Put` the bundle at `/run/factory` → `Exec factory-agent run --bundle /run/factory --system-setup on --proxy http://<gateway>:3128 --remaining <left>`, decoding its events into the `runner.Sink` (progress rebuilt on the host: a known kind else `message`, the message redacted and cut to `MaxSummary`, no `Data`; usage; `step` to the log) → `Get /work/out` → `runbundle.ReadOutcome` (throttle clamp), the transcript re-redacted with the real tokens and capped, uploaded as today → the outcome → `Destroy` (or `Stop` with `--keep-workspace`). A cancel is ctx → `Exec`'s SIGTERM, then `Destroy`. Exit 3 or 4 without an outcome is `failed{harness-error}`. Everything from the VM is untrusted ([trust model](workspace-run.md#trust-model)).
- **What PR 2's code adds** (`internal/agent/workspace/runner.go`, `internal/agent/login`):
  - A missing login for the task's harness fails the run `harness-error` before a VM exists; the login's tokens are redacted from everything the VM reports, as the job's are (`pipeline.Redact`, `Pipeline.StoreTranscript`, the two pipeline additions).
  - The Result is re-checked on the host: a throttled or cancelled outcome carries none; any other needs one for this task, with the outcome's status, valid against the bundle's schema and the task's outcomes (`pipeline.ValidateResult`), else `invalid-result`.
  - The VM's transcript is read from inside `out/` as a regular file, gunzipped up to 256 MiB, without lines over 1 MiB and without host-only lines (`factory.egress`, `factory.egress-dropped`, `factory.workspace-transcript`, under any case of the `type` key: only the gateway and the runner write those); each kept object line is stored re-marshalled from its decoded keys, so what was checked is what is stored; what was dropped, or why there is none, is a `factory.workspace-transcript` line. The gateway's records follow it (at most 50,000; then a `factory.egress-dropped` count).
  - The supervisor's own deadline is the task's wall clock plus 2 minutes (`failed{limit}` when it fires), and it waits at most `StackDownTimeout` + 30 s for `run` to exit after its `outcome` event, then stops it and collects `out/` anyway.
  - `runs/<task>/` is replaced at start and removed after (kept with `--keep-workspace`); a cancelled run is destroyed and reports nothing, as the worker reports nothing then.

## `factory-agent supervise` (PR 3)

```text
factory-agent supervise [--config <agent.yaml>] [--log-level info] [--log-format text] [--driver tart] [--warm-base factory-agent-warm] [--keep-workspace] [--state-dir <dir>]
```

Today's worker loop (`cmd/factory-agent/worker.go`'s `runAgentLoop`, shared: registration by join token and the device flow, the lease long-poll, heartbeats, lending and its SIGHUP reload, drain on the first SIGTERM, abort on the second) with the workspace runner. In order:

1. **The config and the probe.** The harnesses run in the VM, so the host has nothing of theirs to probe: a configured harness is offered when its login is in `<state-dir>/logins` (else it's dropped with "run `factory-agent login codex`", and with none left supervise exits 1). Labels are advertised as configured; there's no host tooling check.
2. **`host check`** (`internal/agent/host`, the config's `workspace` block, driver tart). When a step isn't ok or skipped, supervise prints those steps and exits 1 with "run `factory-agent host init --driver tart`". It never runs init.
3. **One supervisor per user:** an exclusive non-blocking flock on `<host state dir>/supervise.lock` (`~/Library/Application Support/factory-agent`, whatever `--state-dir` says, since Tart's VMs are the user's), held for the process's life; when another supervise holds it, exit 1 with "another factory-agent supervise is running", before the cleanup lists or destroys anything.
4. **The orphan cleanup:** the driver's `List`, then `Destroy` of each workspace name (`ws-tsk_…`; any other name is left alone). Nothing runs at start, so each is a killed supervisor's. A failure stops the start.
5. **The runner:** `workspace.NewRunner` with the tart driver (`Bin` host check's tart, `WarmBase` the flag's, `Log` stderr at `--log-level debug`), the host pipeline (`WorkRoot`, the adapters, the skills cache; `GitAuthHelper`), `StateDir`, `Spec{}` (6 vCPUs, 8 GiB) and `--keep-workspace`.
6. **Registration** with `lending.maxConcurrent` forced to 1, at start and on SIGHUP.

`--state-dir` (default `~/Library/Application Support/factory-agent`, `login`'s) holds `logins/`, `runs/` and `gateway-ca/`; host check always reads the default one. `worker`'s `--stub-result` isn't offered: it would skip the VM.

## Tests

- PR 1: the full lifecycle against the fake tart (golden `testdata/lifecycle.golden`); the network reads and each failure (no or two default routes, IPv6, no `src`, `src` equal to the gateway, loopback, `tart run` exiting, the boot timeout), each leaving no `tart run`; names refused by every call before tart runs; `Put`'s entries, modes and owner, and its refusals; `Exec`'s exit code, stdin, and both cancel paths (TERM honoured; TERM ignored, then KILL and the local stop); hostile `Get` streams (traversal, absolute, symlink out, FIFO, not gzip, over the cap); `Stop` and `Destroy` on running, stopped and missing VMs. `ExecRunner` on `sh`.
- PR 2: a fake `Driver` (its `Exec` runs `vmrun.Main` against the bundle directory, or a scripted event stream), the gateway on loopback, a fake dispatcher: success, failure, cancel, throttled, a hostile outcome.
- PR 3: usage; the `host check` gate on the fake Mac (Softnet missing); a missing login; the orphan cleanup (only `ws-tsk_…`); one lease end to end: a dispatcher that registers and leases harnesstest's task, the real gateway on loopback, a fake driver whose `Exec` writes a done outcome, then the completion, the transcript upload, `maxConcurrent: 1` and a drain.

## The owner's run (manual)

On the Mac, from a checkout of this branch (the dispatcher up, and `~/.config/factory-agent/agent.yaml` pointing at it with `codex` and/or `claude-code` under `harnesses`):

1. `make tart-up` (builds `bin/factory-agent` and `bin/factory-agent-linux-arm64`, then `host init --driver tart`: one sudo prompt for Softnet and pf; the base and warm VMs take a while), then `bin/factory-agent host check`: every step ok (pf may be skipped).
2. Run `claude setup-token` on its own and copy the token it prints, then `pbpaste | bin/factory-agent login claude` (not a direct pipe); and `bin/factory-agent login codex` (reads `~/.codex/auth.json`; `--auth-json <path>` for another).
3. `bin/factory-agent supervise --log-level debug` (add `--keep-workspace` to keep the VM, stopped, and `runs/<task>/` for a look), register through the device-flow prompt, then dispatch a task (`bin/factory dispatch --event event.json --server …`). Watch for the five items below; `tart list` shows `ws-tsk_<id>` while it runs and nothing after.

## UNVERIFIED (the one list, for the owner's Mac run)

1. `tart exec`'s exit status is the remote command's (`ExitError.Code`); Tart's `Exec.swift` throws its exit code as `ExecCustomExitCodeError`, mapped elsewhere.
2. `tart exec`'s stdout carries `Get`'s gzip stream byte for byte.
3. `ip -4 route get <gateway>` in the warm base prints `src <the VM's leased address>`, the address Softnet lets it send from (the gateway's client pin).
4. The pid-file cancel: the remote `kill -s TERM` reaches `factory-agent run` and it writes its outcome.
5. `tart exec` hands argv to the guest as it is (no shell joins it), so the `sh -c` scripts of `Put`, `Exec` and the cancel arrive intact.
