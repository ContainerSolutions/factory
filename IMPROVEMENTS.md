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

**Status (2026-10-27):** (a) is ContainerSolutions/factory#223 (proposal P3 + ADR-0009),
reviewed with ccf-review to no open finding, waiting for Gustavo's approval and his answers to
P3's two questions (the ~$2–3/month cost; push the workstation state or start fresh). (b) is
P3's four-PR rollout, which starts once #223 and the org move (item 4) are both merged.

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
- **Timed work must be considered (from P3, #223).** Some work runs on a clock, not because a
  request arrived:
  - debounces;
  - the lease expiry of a silent agent, so its task is retried;
  - scheduled rules (`factory.schedule.tick`) and approval expiry;
  - the scheduler's `Refresh`: expired agent tokens, offline agents;
  - the retention sweep, log compaction, the App installation check and the work-item resync.

  With CPU allocated only during requests (Cloud Run's request billing, Lambda), these and the
  leader's loops run only while some request is in flight. An external clock is needed: Cloud
  Scheduler or EventBridge calling an authenticated public `POST /tick` that runs what is due
  before answering (today's `/ops` listener is loopback-only). Other options are
  always-allocated CPU, which costs about as much as long polls, or a VM, where the loops just
  run and no tick is needed. P3's §Agents poll briefly lists these.
- Lambda caps a request at 15 minutes, and agents' lease and heartbeat calls need short
  requests.
- More than one instance can run at once, which 5(a) makes safe.
- The secrets (App keys, the session key) live in the cloud's secret manager, never in the
  image.

Part of 9's design: **the bucket's access policy** (from P3):
- The bucket is private: GCS public access prevention, S3 Block Public Access.
- Only the dispatcher's identity (Cloud Run's service account or Lambda's role) can read, write,
  list and delete, on that bucket or prefix only. An optional read-only identity serves
  `factory state --state-url` from a laptop.
- Keep GCS's 7-day soft delete, or turn on S3 versioning, as the guard against an accidental
  delete.
- Never add an age-based lifecycle rule on `log/`: P3 deletes log entries itself, under its
  retention rule.
- Pick a region inside GCS's free storage tier (for example `us-central1`).
- `serve` doesn't check any of this, so the deployment docs must say how to set it.
