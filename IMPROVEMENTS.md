# Factory improvements: open work

The factory's remaining work, and nothing else. Done work, dropped items and the reasons behind
past changes are in [IMPROVEMENTS_DONE.md](IMPROVEMENTS_DONE.md). Both live on the
`improvements` branch, which is never merged to main.

> **We are in the closing phase (decided 2026-10-27).** Finish each workstream below; don't
> defer work into this file. A finding in a PR is either fixed in that PR (or the next one of
> its stack) or dropped, with the reason in the PR body. Add an item here only when Gustavo
> asks for it. **Other agents push to this file too:** `git pull` right before editing, keep
> edits to your own workstream's section, and push straight away.

How to work: read AGENTS.md first. PRs go through lisa as `ccf-lisa[bot]` with
`LISA_REPO=ContainerSolutions/factory` (the repo moved; lisa's App auth doesn't follow the old
name's redirect) and are reviewed with ccf-review. Gustavo merges. When a workstream ships,
record it in IMPROVEMENTS_DONE.md with its PR numbers and delete its section here.

## 1. Real-host checks (Gustavo)

Shipped work that only fakes have tested. These are runs, not PRs; Gustavo runs them after
phase 10 merged (2026-10-27), from the order the Mac session gave him. Record each result in
IMPROVEMENTS_DONE.md.

| # | What | How | Pass when |
|---|---|---|---|
| T1 | **ChatGPT refresh (#165)**, on the Mac or the workstation | **1.** Stop `supervise`, then `cp -p <state>/logins/codex.json{,.bak}`. **2.** Find the key paths without printing values: `jq -r 'paths(scalars)\|map(tostring)\|join(".")' <state>/logins/codex.json`. Set only the access token to an invalid string with `jq` (keep the refresh token), keeping mode 0600. **3.** Start `supervise` and redispatch any codex task. **4.** Afterwards run `codex login` on the host, because the old refresh token in `~/.codex/auth.json` is spent. | The task succeeds with no re-import. The run's egress log shows `chatgpt.com` 401, then the resent 200. `jq .lastRefresh` on the file shows a new time. `stat` shows 0600. The new access token is a JWT with `exp`. |
| T2 | **`yarn install` and `yarn build` in a VM (#172)** | A release-docs or implement run on mock-docs (phase 10 lifted "never install"). | The run's checks say `build: pass`, not `not-run`. Its transcript (the run page, or `GET /runs/<id>/transcript`) has `factory.egress` lines for `registry.yarnpkg.com` and `registry.npmjs.org`. |
| T3 | **The egress log records what the gateway forwards** | Any recent run's transcript in the UI, filtered for `factory.egress`. The VM's local copy doesn't have these lines: the host adds them. | The model host (`api.anthropic.com` or `chatgpt.com`), `github.com` and `api.github.com` appear, with the credential column filled and no token values. |
| T6 | **An agent-only change on kata** | Rebuild `factory-agent` and restart `supervise`. The kata driver puts the binary into each workspace at start (`AgentBinary`), so nothing needs to be rebuilt. | `host check` stays ok, and the next workspace runs the new binary. |

T4 and T5 passed (IMPROVEMENTS_DONE.md).

Real-host checks from stacks 3 and 4 (#197–#217), split by machine.

**Linux workstation (kata):** run from the Linux box session's runbook. Each PR body has the
details. The checks are:
- #214: `host init --only kataimage` without the helper fails at once.
- #204: a declined privileges prompt skips the image steps.
- #202: Ctrl-C during `kataimage` and `katawarm` leaves nothing running.
- #217: the workspace hostname resolves.
- #197: the nft table follows a non-default `workspace.gatewayPort`.
- #208: a task's `limits` size its VM.
- #215: Ctrl-C of supervise leaves no `kata-helper`.
- T6: an agent-only change on kata.

**Mac (tart):** everything below, on the Mac at `main` (#217 or later). Stop `supervise` first.
`<state>` is `~/Library/Application Support/factory-agent`. A kept workspace is stopped: start
it with `tart run --no-graphics <ws> &` before `tart exec`, then `tart stop <ws>`.

- **M0, build and init:** `make tart-up`. It builds `bin/factory-agent` and
  `bin/factory-agent-linux-arm64`, then runs `host init --driver tart`. Expect `host check
  --driver tart` all ok.
- **M1, #199 and #216, size and outputs:**
  1. Start `bin/factory-agent supervise --keep-workspace` and dispatch any task. It finishes with
     its result and transcript. Those outputs come back through `vmexec.Get`, which unpacks
     them with `host.Extract`: #216's `execfile` path.
  2. `tart list` shows the kept `ws-tsk_<id>`. `tart get ws-tsk_<id>` shows the CPU and memory
     of `workspace.size` in your `agent.yaml`.
  3. *(#208 on tart)* Dispatch a task whose TaskSpec has `limits: {cpus: 2, memory: 4Gi}`. This
     needs a local catalog change. `tart get` then shows 2 CPUs and 4096 MB.
  4. Dispatch a task with a `limits.memory` larger than `workspace.size.memory`. Its log and
     progress say `capped limit=memory requested=… granted=…`, and `tart get` shows
     `workspace.size`.
- **M2, #217, the hostname resolves:** in a workspace, `tart exec ws-tsk_<id> sh -c 'hostname;
  getent hosts "$(hostname)"; sudo -n true'`. Pass: `getent` answers, and there's no "unable to
  resolve host" line. If it fails, base.sh needs #217's nsswitch change (`files myhostname
  dns`).
- **M3, #197, the pf anchor follows `workspace.gatewayPort`:**
  1. Set `workspace.gatewayPort: 3129` in `agent.yaml`, then run
     `bin/factory-agent host init --driver tart --agent-linux bin/factory-agent-linux-arm64`.
     The pf step is outdated and asks for sudo.
  2. `sudo pfctl -a com.compliance-framework.factory -sr | grep 3129` shows the rule.
  3. Start `supervise` and dispatch a task. It succeeds, since the harness reaches its model only
     through the gateway. While it runs, `lsof -nP -iTCP:3129 -sTCP:LISTEN` shows factory-agent.
  4. Put the port back to 3128 and rerun `host init`.
- **M4, #205, kind images load offline on arm64:**
  1. In `agent.yaml`, add `kindest/node:v1.35.0` to `workspace.warm.images`. Set
     `workspace.warm.kindImages: ["busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"]`.
  2. `rm "<state>/warmbase.json"`, then run `host init` (as in M3.1) to rebuild the warm base.
  3. In a workspace (`--keep-workspace`, then `tart run` as above), run as `admin`
     (`tart exec ws-tsk_<id> sudo -u admin -H sh -c '…'`):
     - `ls -l /var/lib/factory/kind-images/` shows each archive `-rw-r--r--`.
     - `tar -xOf /var/lib/factory/kind-images/<name>.tar index.json` names one manifest, for
       linux/arm64.
     - `kind create cluster --image kindest/node:v1.35.0`.
     - `kind load image-archive /var/lib/factory/kind-images/*.tar` succeeds as `admin`, with no
       sudo.
     - `kubectl run bb --image=busybox:1.37.0 --image-pull-policy=Never --restart=Never --
       sleep 30`.
     - `kubectl get pod bb` reaches Running, so the image was already present and nothing was
       pulled.
     - `kind delete cluster`.
  4. Afterwards, keep the warm config or revert it: revert `agent.yaml`, `rm
     "<state>/warmbase.json"`, then run `host init`.

Not testable without a catalog change: #173 for an implement run under claude, because
implement allows only codex. The claude side of the `ccfr` lookup, the plugin-dir fallback, is
exercised by every docs-review run.
- **Mac, also:** Softnet's per-VM anti-spoofing, which the gateway's client pin relies on.

## 2. Apply `environment.egress` in the gateway

The gateway already implements deny-by-default, `AllowHosts` and the tunnel SNI check, but
nothing applies a TaskSpec's `environment.egress` yet. Wire the field from the Task through
the run bundle into the workspace's gateway policy. This is the network half of P1's hard
sandbox.

## 3. Phase 6: run tokens and enrollment

- the run token (#101's `Lease.runToken`) and the lease credential bundle;
- enrollment (#102), replacing the join token and the device flow. Decide how an agent's owner
  is attributed when no person logs in. Then remove the device flow and the join-token file.

Today the gateway injects the agent token on the dispatcher's effects route.

## 4. The org move to `ContainerSolutions/factory`

The Go module path is still `github.com/compliance-framework/factory`, and
`DefaultCatalogRepo` (`internal/controllers/catalogsource.go`), the POC files and the docs
still name the old org; clones work only through GitHub's redirect. ccf-review's default owner
is still `compliance-framework`, so pass it full `ContainerSolutions/factory#N` refs.

## Then: a shippable factory (Gustavo, 2026-10-27)

The final list. Start it after 1–4, in this order. **The goal:** the factory ships as a product,
and our own prod instance runs at `factory.container-solutions.com` for compliance-framework,
from the catalog in its own repo. Several dispatchers must be able to run at once, and the whole
thing must stay inside a cloud free tier. That's why state moves to object storage.

### 5. State in GCS / S3 instead of local files

Today P2's file store (`internal/store/filestore`, `serve --state-dir`) keeps state in local
files and assumes a single dispatcher writing them. This probably needs some rearchitecture,
broken down as:
- **(a) A new file layout** that suits object storage and more than one writer. Use small
  objects, one per record (or per append-only segment), each changed by a conditional write:
  S3's `If-Match`/`If-None-Match`, GCS's generation preconditions. Two dispatchers then can't
  overwrite each other, and the work-item locks and leases become conditional writes too. Write
  it up as a short design first: what each object holds, how listing and retention work, and
  what a second dispatcher does with a lost write.
- **(b) The upload itself:** an object-store backend behind the store interface
  (`internal/store/store.go`, with `storetest` as its conformance suite), with GCS and S3
  implementations. Keep the local-file backend for development.

### 6. Binaries and an install script for `supervise`

One command sets an agent up, for example
`curl -fsSL https://github.com/ContainerSolutions/factory/releases/latest/download/install.sh | bash`.
It:
- downloads the release's `factory-agent` for the OS and architecture and checks its checksum;
- runs `host init`;
- walks through the model logins (`factory-agent login claude|codex`);
- writes `agent.yaml`;
- installs supervise as a service (launchd on the Mac, systemd on Linux).

This needs a release workflow (darwin-arm64 and linux-amd64 binaries, with checksums). Once
factory releases, it switches to `release: true` in workflows' `repos.yaml`. That's a
`workflows` change, so Gustavo approves it.

### 7. Ship the dispatcher

- Ship what `make poc-up` runs (`deploy/poc/run.sh`, which runs `factory serve` with the web UI
  built in, `make build-release`) as release binaries.
- Ship a container image of the dispatcher (`factory serve`), for example on ghcr.io, built in
  the same release workflow.

### 8. The factory catalog in its own repo

Move `catalog/` to a new repo under compliance-framework (for example
`compliance-framework/factory-catalog`), with its `factory rules test` fixtures run in that
repo's CI. Point `DefaultCatalogRepo` (`internal/controllers/catalogsource.go`) at it. This repo
keeps only the test catalogs.

### 9. Run the dispatcher in the cloud, inside the free tier

The cheapest setup that works: compare GCP Cloud Run and AWS Lambda (behind API Gateway or a
function URL) against the free tiers, with state in 5's bucket. Then deploy it at
`factory.container-solutions.com`, with the GitHub Apps' webhooks pointed there. Constraints to
design for:
- Both scale to zero. The dispatcher's background loops (controllers, debounces, lease expiry,
  the App installation check) then need a trigger, such as Cloud Scheduler or EventBridge
  calling a tick endpoint, or they move into request handling.
- Lambda caps a request at 15 minutes, and agents' lease and heartbeat calls need short
  requests.
- More than one instance can run at once, which 5(a) makes safe.
- The secrets (App keys, the session key) live in the cloud's secret manager, never in the
  image.
