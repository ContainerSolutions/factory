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

## Then: a shippable factory (Gustavo, 2026-10-27)

The final list. In this order. **The goal:** the factory ships as a product,
and our own prod instance runs at `factory.container-solutions.com` for compliance-framework,
from the catalog in its own repo. Several dispatchers must be able to run at once, and the whole
thing must stay inside a cloud free tier. That's why state moved to object storage (5).

Items 5–7 shipped on 2026-10-28: see IMPROVEMENTS_DONE.md.

### 8. The factory catalog in its own repo

Move `catalog/` to a new repo under compliance-framework (for example
`compliance-framework/factory-catalog`), with its `factory rules test` fixtures run in that
repo's CI. Point `DefaultCatalogRepo` (`internal/controllers/catalogsource.go`) at it. This repo
keeps only the test catalogs.

### 9. Run the dispatcher in the cloud, inside the free tier

The cheapest setup that works: compare GCP Cloud Run and AWS Lambda (behind API Gateway or a
function URL) against the free tiers, with state in 5's bucket (`serve --state-url gs://…|s3://…`). Then deploy it at
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
