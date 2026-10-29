# Factory improvements: open work

The factory's remaining work, and nothing else. Done work, dropped items and the reasons behind
past changes are in [IMPROVEMENTS_DONE.md](IMPROVEMENTS_DONE.md). Both live on the
`improvements` branch, which is never merged to main.

> **We are in the closing phase (decided 2026-10-27).** Finish each workstream below; don't
> defer work into this file. A finding in a PR is either fixed in that PR (or the next one of
> its stack) or dropped, with the reason in the PR body. Add an item here only when Gustavo
> asks for it. **Other agents push to this file too:** `git pull` right before editing, keep
> edits to your own workstream's section, and push straight away.

## Ways of working (read before starting)

A new session can pick up an item from this file and AGENTS.md alone. The rules, all
Gustavo's:

**Roles.**
- The lead session plans, writes a brief per PR (or per stack), and runs **lisa** in a subagent
  to implement it.
- It reviews each PR with **ccf-review**, triages the findings itself (no asking), and approves
  under Gustavo's account.
- **Gustavo only merges.** Drive every PR to an approval without asking him to triage.

**lisa (the `lisa:lisa` skill), as the bot.**
- Set `LISA=<ccf-skills plugin cache>/lisa/<version>/skills/lisa/lib/lisa`,
  `LISA_REPO=<owner/repo>` and `LISA_BRANCH=fx/<slug>`. Keep the App variables (`LISA_APP_ID`,
  `LISA_PRIVATE_KEY_PATH`) as the environment has them.
- `"$LISA" whoami` must say `auth: github-app`, `commits_as: ccf-lisa[bot]`, and
  `repo_access` must name the target repo. Otherwise stop: **never push as Gustavo**.
- **The clone.** lisa works in its own clone under `~/.lisa/<owner>-<repo>/…`, never in
  Gustavo's checkout. Before committing or rebasing there, set:
  - `user.name "ccf-lisa[bot]"`;
  - `user.email "286799724+ccf-lisa[bot]@users.noreply.github.com"`;
  - `notes.rewriteRef refs/notes/commits`.
- Before every push, `git log --format='%an | %cn' origin/main..HEAD` must show only the bot,
  as author **and** committer.
- **Stacks:** make them with **gh stack**, in lisa's stack mode, never with bases chained by
  hand. The lead links existing PRs with `gh stack link <numbers>`.
- **The ccf-lisa App** is installed on ContainerSolutions/factory and on compliance-framework.
  A new repo must be covered by its installation (Gustavo's step), or lisa can't push there.
  Its auth doesn't follow a renamed repo's redirect: always use the current `owner/repo`.

**PRs.**
- Conventional-commit titles. One concern per PR, under 1000 lines (CORE-SIZE-001: split
  rather than label).
- The commit message ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. The
  PR body ends with `🤖 Generated with [Claude Code](https://claude.com/claude-code)`.
- The body covers what changed, verification (saying plainly what CI can't check), and
  "Dropped, with reasons".
- Contracts (`api/`, `internal/apis/v1alpha1`, `schemas/`) go in their own PR, merged first.

**Lean PoC.**
- Build what the goal needs: no speculative tests or hardening.
- In triage, only Blockers and Must-fixes that matter block.
- **Closing phase:** a finding is fixed in its PR, or in the next one of its stack, or it is
  dropped with the reason in the body. Nothing is deferred into this file.

**ccf-review.**
- Run `ccfr` from the ccf-review plugin (`<plugin cache>/ccf-review/<version>/bin/ccfr`), in
  this order: `scope`, `workspace`, `classify`, `render`, then
  `publish --event APPROVE --confirm`.
- Dispositions are one of: accepted, rejected, resolved-in-stack, disputed, answered or
  deferred.
- Skip the checkpoints.

**Safety (non-negotiable).**
- **Never run the real `claude` or `codex`**, not even through a test. Put a guard dir first on
  `PATH` for every `go` and `make` run: stub `claude` and `codex` scripts that print a refusal
  and `exit 97`. Fake them in tests.
- Never run `tart`, `sudo`, `factory-agent host init`, `make smoke-*`, `make tart-up|kata-up`,
  `launchctl`, `systemctl`, or `deploy/install.sh` outside a stubbed temp `HOME`. Gustavo runs
  the real-host checks: give him copyable commands.
- **Never read or print credentials.** That covers `deploy/poc/.env`, `~/.codex`, `~/.claude*`,
  `~/.factory*`, the values in `~/.config/factory-agent/*`, App keys and tokens: print key names
  only.
  - Filter push output with `sed -E 's/ghs_[A-Za-z0-9_.-]+/ghs_REDACTED/g'`, and don't print
    remote URLs.
  - Use the dispatcher's CLI token only when needed, and never store or print it.
- No follow-up session or task cards. Run a needed follow-up in the same session (a lisa agent,
  then ccf-review). When it's unclear whether one matters, ask Gustavo.
- Outward-facing steps are Gustavo's unless he says otherwise: creating a repo, installing an
  App, DNS, cloud accounts and billing, publishing a release.

**Releases.**
- Manual and standalone: each pipeline is its own workflow, on `release: published` only.
- No release-please or bots, and no compliance-framework release mechanism.
- CI keeps compliance-framework/workflows' `ci-go-service.yml`, pinned by SHA.

**This file.**
- It lives on the `improvements` branch, never merged; that branch is pushed as Gustavo, not as
  the bot.
- `git pull` right before editing, then push at once, and copy both files to the repo root for
  reading.
- When an item ships, move it to IMPROVEMENTS_DONE.md with its PR numbers, and delete it here.

## Then: a shippable factory (Gustavo, 2026-10-27)

The final list. In this order. **The goal:** the factory ships as a product,
and our own prod instance runs at `factory.container-solutions.com` for compliance-framework,
from the catalog in its own repo. Several dispatchers must be able to run at once, and the whole
thing must stay inside a cloud free tier. That's why state moved to object storage (5).

Items 5–7 shipped on 2026-10-28: see IMPROVEMENTS_DONE.md.

### 8. The factory catalog in its own repo

Move `catalog/` (272 files: groups, pools, prompts, providers, queues, rules, rulesets, schemas,
taskspecs, templates, testdata, plus `catalog_test.go`) to a new repo under compliance-framework,
for example `compliance-framework/factory-catalog`. Its CI runs the catalog checks, and the
dispatcher reads it from there by default. This repo keeps only test catalogs.

**Where things are today:**
- **The default source.** `DefaultCatalogRepo = "https://github.com/ContainerSolutions/factory"`
  is in `internal/controllers/catalogsource.go:33`, with `DefaultCatalogRef` `main` and
  `DefaultCatalogPath` `catalog`. `serve`'s `--catalog-repo|--catalog-ref|--catalog-path`
  default to these (`cmd/factory/serve.go:123`), and `cmd/factory/serve_test.go:505` checks
  those defaults.
- **Private repos.** The catalog is read by `internal/catalog/gitsource`. Its `Load` and `Head`
  take no credential, so a **private** catalog repo would need `HeadAuth`'s `Auth` wired
  through catalog-sync. Today only `compile.go`'s skills refs use `HeadAuth`. A public repo
  needs nothing.
- **What CI runs.** `make catalog` (in `make reviewable`, and in CI's `make-targets`) runs
  `factory validate --dir catalog` and `factory rules test --catalog catalog --coverage`, both
  when `catalog/` exists.
- **Tests that read the shipped catalog.** Each must move to a test catalog or go with the
  catalog:
  - `cmd/factory/deploykit_test.go:30` (`shippedCatalog`): keeps `deploy/poc/.env.example` in
    step with the catalog's secret refs;
  - `internal/agent/harness/claudecode/schema_test.go:88` and
    `internal/agent/pipeline/bundle_test.go:51`: `catalog/schemas/results/*.json`;
  - `internal/agent/worker/dispatcher_test.go:29`: `catalog/testdata/poc`;
  - `catalog/catalog_test.go`.
  The test catalogs already in this repo are `internal/catalog/testdata/poc` and
  `catalog/testdata/poc`.
- **The docs.** `deploy/poc/README.md` §4 ("Catalog") and `run.sh`'s catalog flags describe
  `catalog/` of this repo, as does the local-copy recipe (`~/factory-poc-catalog`, with the
  real App IDs patched into `providers/github-public.yaml`). ADR-0006 is "catalog in git".
- **What the CLI assumes.** `factory validate` and `factory rules test` default to `./catalog`.
  The new repo either runs them with `--dir .` and `--catalog .`, or keeps the catalog under
  `catalog/` there too (`--catalog-path catalog` stays the default). That's a choice for the
  plan.

**The new repo's CI needs the `factory` binary.** It can download a release asset
(`factory-linux-amd64`, with SHA256SUMS) at a pinned tag, or `go install` it from this repo at
a pinned ref. A pin keeps catalog CI from breaking when factory changes; bumping it is a
manual PR.

**Done when:**
- the new repo holds the catalog and its fixtures, and its CI is green;
- `DefaultCatalogRepo` (and the path, if it changed) point at it;
- this repo's tests use only test catalogs, and `catalog/` is gone from it;
- the README's §4 and the local-copy recipe are updated;
- a dispatcher started with the defaults loads the new repo's catalog (Gustavo checks this on
  his PoC).

**Ask Gustavo first:**
- the repo's name, and whether it's public (a private one needs the auth wiring above);
- that he creates it and adds it to the ccf-lisa App's installation;
- branch protection, and the two approvals for Provider changes the README mentions;
- whether the catalog stays under `catalog/` in the new repo.

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

**Where things are today (for 9):**
- `serve --state-url gs://bucket/prefix|s3://bucket/prefix` (#246, P3) keeps the state in a
  bucket and elects one leader. Every instance serves the API and the scheduler's tick.
- Credentials come from the cloud SDKs' default chains: GCS's ADC, and AWS's.
  `make e2e-objectstore` checks it against fake-gcs-server and MinIO.
- The image is `ghcr.io/containersolutions/factory:<tag>` (public). Its entrypoint is
  `factory`, its default command `serve`, and it runs as uid 65532.
  `deploy/poc/README.md` §"Run the image" lists the env vars `serve` needs.
- `--ops-addr` is loopback-only today, and there is no `POST /tick`.
- Background loops start in `cmd/factory/serve_app.go` (from :205): the leader's controllers
  and sweeper, and every instance's scheduler tick and installation check.
- `design/proposals/P3-dispatcher-state-in-object-storage.md` covers it:
  - §Agents poll briefly (:551), and the work done inside requests (:603);
  - :908: the deployment, the tick endpoint and the bucket policy are this item's.
  - Agents already poll briefly (#237), for platforms that cap request time.
- The GitHub Apps (ccf-factory, ccf-factory-reviewer) and the OAuth App are Gustavo's. He
  points their webhooks and callback URLs at the new host.

**Plan it as:**
1. a short design: the platform comparison against the free tiers, the tick, the secrets, the
   bucket policy and the cost (P3's style; in `design/proposals/` or as an ADR);
2. the code it needs, for example an authenticated `POST /tick` and any serve flags, as its own
   PRs or stack;
3. the deployment, as docs and IaC or scripts in this repo: Gustavo runs them, since they touch
   his cloud account.

**Done when** `factory.container-solutions.com` serves the UI over HTTPS, its state is in the
bucket, the Apps' webhooks reach it, an agent enrolls against it and runs a task, and the
monthly bill is within the free tier.

**Ask Gustavo first:**
- GCP or AWS, and which account or project;
- who controls DNS for container-solutions.com;
- whether a small always-on VM is acceptable, if it's cheaper than scale-to-zero plus a
  scheduler;
- the region.

