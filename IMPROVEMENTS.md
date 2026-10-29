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

### 8. The CCF catalog in its own repo (compliance-framework/factory-catalog)

Move `catalog/` to **`compliance-framework/factory-catalog`** (private, created, empty; ccf-lisa
can push there). The dispatcher reads it from there by default, with an App token. The move
also gives CCF's prod flows a home. The design was agreed with Gustavo on 2026-10-29; the
draft is in that session's scratchpad (`ccf-catalog-design.md`). The decisions:

- **Prod only.** The new repo has no dev RuleSets (dev was validated in this repo). `dev-*` and
  mock-docs fixtures stay here, in a test catalog.
- **Canary.** The first wave of every new RuleSet is `compliance-framework/docs` only. Later
  waves are catalog PRs that widen `scope.repos`.
- **People merge.** Every merge rule stays excluded.
- **Layout.** The catalog stays under `catalog/` in the new repo (`--catalog-path` default
  unchanged).
- **CI is binary-only.** It downloads `factory-linux-amd64` at a pinned release (SHA256SUMS)
  and runs `factory validate --dir catalog` and `factory rules test --catalog catalog
  --coverage`. `catalog/catalog_test.go` doesn't move. `rules test` takes over the checks
  that matter (PR 2 below), and the dev-safety tests go away with the dev RuleSets.
- **Procedures are prompts only:** no new ccf-skills skills.

**The flows** (packs; RuleSet scope `["compliance-framework/docs"]` at first):

1. **issue-impl** (FLOW 1, replaces docs-epic). It starts in two ways:
   - an issue opened by a `maintainer`/`member` (and not a bot), unless it has `factory:skip`;
   - or `factory:queue` added by a member, which is how a triaged issue gets promoted.

   The run: `plan-issue` (plan-epic's procedure, any repo) → `implement` per sub-issue, stacked
   → `review` of the stack → `implement` address-review, up to 3 rounds → needs-human.
   - **By repo kind:** the checks and the prompt's per-kind parts (the docs screenshot
     recipe) come from RuleSet defaults (`repoKind: docs|go|ui|plugin|policies|helm`,
     `reviewChecks`).
   - **TaskSpecs:** `implement` and `review` are each one TaskSpec. `docs-review`, `code-review`
     and `implement-code` aren't split out.
   - **docs:** `announce-ptbr-live`, a UC2 one-off, is dropped.
2. **triage** (FLOW 2): an issue opened by tier `anyone`, not a bot → `triage`.
   - **The run:** read-only, with no effects of its own. The repo's issue plus api/ui/agent are
     read-only.
   - **Outcomes:** `triaged|needs-info|duplicate|security|invalid`.
   - **Its schema, `triage.json`:** kind, components, summary, impact (users, severity),
     `breakingChange` (likely, why), value, effort, `duplicateOf`, related, questions,
     `suggestedLabels` (an enum), `nextStep`.
   - **Rules:**
     - post the `triage-report` Template and the labels (`factory:triaged` plus the suggested
       ones);
     - `security` → `needs-human` and a neutral comment pointing to SECURITY.md;
     - re-triage when the reporter answers a `needs-info`, up to 3 times.
3. **bump-fix** (FLOW 3). New trust tier `release-bot` (`bots: ["ccf-release-bot[bot]",
   "renovate[bot]"]`).
   - **Tracking:** `record-bot-pr` sets `bot: true` on the bot's `pr.opened|updated|labeled`.
   - **Triggers:**
     - `needs-human` added by `release-bot` → `bump-fix {mode: assess}`;
     - `pr.checks.completed` failure on a `bot` item → `bump-fix {mode: fix}`, at most 3.
   - **The run:**
     - read each major's changelog;
     - build, run `make test`/`lint`, and the integration target if one exists;
     - make the smallest source fix. A test changes only when it no longer compiles, never to
       relax an assertion, and every test change is listed;
     - push to the bot's branch and comment `bump-report`.
   - **Outcomes:** `fixed|no-impact|blocked|needs-human`.
   - **Its schema, `bump-fix.json`:** pr, deps[] (name, from, to, major, breaking[], callSites),
     checks, testChanges[], risk, commits.
   - **After a fix:** `review` runs. The factory also adds `needs-human` and removes
     `ccf-bump:automerge`, so `ccf-bump merge` never merges factory-changed code.
   - **Accepted limit:** ccf-bump force-pushes its branch on a same-day rerun.
4. **release-docs** (FLOW 4): moves as is, with `poc-*` renamed `ccf-*` and queue `poc` renamed
   `impl`.
5. **review-human** (people's PRs, on every repo in scope):
   - `review` runs on non-bot, non-factory, non-draft PRs;
   - for tier `anyone` it only comments, never approves.

**Shared.**
- **Trust tiers:** `maintainer` (org admins), `member` (new Group, org members),
  `release-bot`, `anyone`.
- **Queues:** `impl` (2 at a time, 40 runs a day), `triage` (2, 30), `bumps` (1, 15).
- **`closed-gate`'s `on`:** add `issue.labeled`, `issue.commented` and `pr.labeled`.
- **Cancels and counts:** `close-on-primary-closed` cancels the new TaskSpecs, and
  `escalate-needs-human` counts `review` rounds.
- **State:** each pack keeps its own state keys (`triage*`, `bot`, `bump`) because item state
  is shared by RuleSets.
- **The `needs-human` label:** it is both the factory's escalation label and CCF's bot-PR
  label (workflows/docs/attention.md). FLOW 3 only reacts to the release bot adding it.

**Factory code** (from the code as it is today):
- **The catalog is read without credentials.** `internal/catalog/gitsource`'s `Load`/`Head`
  take no credential. Only `compile.go`'s skills use `HeadAuth`, and the Provider holding the
  App keys is inside the catalog. So `serve` needs a bootstrap credential:
  `--catalog-app-id N --catalog-app-key-ref ccf-factory-app/private-key`. It reads the existing
  `FACTORY_SECRET_CCF_FACTORY_APP_PRIVATE_KEY`.
- **The defaults.** `DefaultCatalogRepo`, `DefaultCatalogRef` and `DefaultCatalogPath` are at
  `internal/controllers/catalogsource.go:33`. The `serve` flags are at `cmd/factory/serve.go:112`,
  and `cmd/factory/serve_test.go:505` checks the defaults.
- **Tests that read `catalog/`:**
  - `cmd/factory/deploykit_test.go:30`;
  - `internal/agent/harness/claudecode/schema_test.go:88`;
  - `internal/agent/pipeline/bundle_test.go:51`;
  - `internal/agent/worker/dispatcher_test.go:29`;
  - `catalog/catalog_test.go`.

  `make catalog` runs `validate` and `rules test` when `catalog/` exists. The docs are
  `deploy/poc/README.md` §4, `run.sh`'s catalog flags and the local-copy recipe.

**The stack.** Each PR is written by lisa (as ccf-lisa[bot]) and reviewed with ccf-review
(proportional, at most 2 rounds), and Gustavo merges. Branches are `fx/cat-<n>-<slug>`, one
`gh stack` per repo.

*ContainerSolutions/factory, first:*
1. `feat(controllers): read a private catalog with an App installation token`:
   - catalog-sync mints a `contents: read` installation token for the catalog repo before
     each `Head` and `Load`, and `gitsource.Load` gains an `Auth`;
   - the `serve` flags above, with validation and their tests;
   - no token keeps today's anonymous behaviour.
2. `feat(cli): rules test renders prompts and templates and checks results`. For each
   fixture, it fills the prompt of every dispatch and the Template of every notify into the
   goldens, and validates the fixture's `run.result` against the TaskSpec's schema. This is
   what `catalog_test.go`'s `TestPrompts`, `TestTemplates` and `TestResultSchemas` did.
3. *(no PR)* Gustavo publishes a factory release containing 1–2; the catalog CI pins it.

*compliance-framework/factory-catalog:*

4. `chore: import the factory catalog`:
   - prod objects and fixtures only, renamed `poc-*` to `ccf-*`;
   - the real App IDs in the Provider;
   - CI with the pinned binary;
   - a README with the rollout waves.
5. `feat(shared)`: the member and release-bot tiers, the three queues, and the shared rules
   for the new events.
6. `feat(issue-impl)`: `plan-issue`, the unified `implement` and `review`, the rules,
   Templates and fixtures. The docs-epic rules are removed, and docs moves onto it.
7. `feat(triage)`: the TaskSpec, prompt, `triage.json`, its Templates, rules and fixtures.
8. `feat(bump-fix)`: the TaskSpec, prompt, `bump-fix.json`, `bump-report`, rules and fixtures.
9. `feat(review-human)`: people's-PR review generalised (`review`, comment-only for
   outsiders).

*ContainerSolutions/factory, last:*

10. `refactor!: drop the in-repo catalog; default to compliance-framework/factory-catalog`:
    - the tests move to test catalogs;
    - `make catalog` is dropped;
    - the README §4 and the local-copy recipe are updated.

**Gustavo's steps:**
- **Before 4:**
  - an initial commit on the empty repo's `main` (or say the bot may push one);
  - the ccf-factory App installed on factory-catalog (`contents: read`);
  - branch protection, and the two approvals for Provider changes.
- **After 3:** the factory release.
- **After 10:** start the PoC dispatcher with the defaults plus `--catalog-app-id` and check
  that it loads the new catalog.
- **To canary:** watch docs for a train, an issue and a bump. Then the waves:
  - api/ui/agent, or the small plugins first, by catalog PR;
  - all plugins;
  - `compliance-framework/*` minus factory-catalog.

**Done when:**
- the new repo holds the catalog and its CI is green;
- the dispatcher loads it by default with the App token;
- this repo has no `catalog/`;
- FLOWs 1–3 and people's-PR review are live on docs.

**Later, not this item:** a FLOW for new plugins and policies; the release-please major-gate
summary; the weekly backlog sweep.

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

