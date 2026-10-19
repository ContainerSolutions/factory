# LLD — GitHub provider (GH)

> Workstream: [GH](../../definitions.md#gh-github-provider) · Tasks: GH-S1-T01…T04 · Packages: `internal/provider/github`, `internal/provider/tokens`
> Interface: [contracts.md §Provider interface](../../foundation/LLD/contracts.md#provider-interface)

## Apps

| App | Role | Webhook | Permissions (repository) | Events |
|---|---|---|---|---|
| `ccf-factory` | `author` identity **and the only event source** | **yes** → `https://<public-url>/hooks/github-public` | contents RW, pull requests RW, issues RW, metadata R, checks R, actions R, deployments R | `issues`, `issue_comment`, `pull_request`, `pull_request_review`, `pull_request_review_comment`, `pull_request_review_thread`, `release`, `deployment_status`, `check_suite`, `sub_issues` |
| `ccf-factory-reviewer` | `reviewer` identity | **no** | contents RW (merge/rebase), pull requests RW, issues R, metadata R, checks R | none |

Org-level: members R on `ccf-factory` (to resolve `role: admin` and teams). Installations: `docs`, `mock-docs`, `workflows` (read-only usage), mock repos (dev). Full creation steps: [rollout.md §GitHub Apps](../../poc-rollout/LLD/rollout.md#github-apps).

## Auth and tokens

```go
type Minter interface {
    Token(ctx context.Context, identity string, repos []string, perms map[string]string, ttl time.Duration) (Credential, error)
}
```

- JWT from App ID + private key (`ghinstallation`), installation ID resolved per org and cached.
- `POST /app/installations/{id}/access_tokens` with **`repositories: [name]` and `permissions: {…}`** — the body is always populated; a request without `repositories` is a programming error (`panic` in tests, error in prod) → *unscoped tokens are impossible by construction*.
- Git credentials for agents: `rw` repo token with the TaskSpec permissions (subset of the identity's), on that repo only; read repos: public ones get **no token** (`publicReads`), private ones a separate read-only token each (`contents: read`, one repo per token). The scheduler asks for each with its own `MintGitCredential` call; the request shapes and how private is decided are in [contracts.md §Refinements](../../foundation/LLD/contracts.md#refinements-fnd-s1-t03).
- Cache key `(identity, sorted repos, sorted perms)`; reuse until 5 min before expiry (GitHub caps at 1 h).
- Dispatcher-side API calls use the same minter with the identity the effect requires.

Implemented in GH-S1-T01 as `internal/provider/tokens`; the shape differs from the sketch above, see [§Decisions taken in GH-S1-T01](#decisions-taken-in-gh-s1-t01).

## Ingress and normalisation

`Conn.Handler()` for `POST /hooks/github-public`:

1. Verify `X-Hub-Signature-256` (HMAC-SHA256, constant time) → 401 on failure.
2. **Accept before acknowledging:** `sink.Accept(RawEvent)` stores a `Delivery` record (`gh-<X-GitHub-Delivery>`, raw headers and body, `status: received`) in the store; a `Create` conflict means a duplicate delivery → 200 no-op. Only after the record exists does the handler return `202` (within 1 s). Normalisation runs from the store (provider controller watches `Delivery`), never from an in-process channel.
3. **Event identity is separate from delivery identity.** One delivery can normalise into several Events (e.g. `check_suite` → one per PR). Each gets `ID = gh-<delivery>/<n>` with a stable `n` (outputs sorted by object ref), so store dedupe keeps all of them.
4. **Restart gap.** The PoC store is in memory, so accepted-but-unprocessed deliveries are lost if the process dies. On startup, after rebuild, the provider lists the App's recent webhook deliveries (`GET /app/hook/deliveries`, covering the last 24 h) and ingests any it hasn't seen, through the same `Accept` path. State-bearing changes are additionally healed by the work-item resync (level-triggered).

Mapping (GitHub event/action → neutral type; facts added under `event.*`):

| GitHub | Neutral type | Facts |
|---|---|---|
| `issues.opened` / `closed` / `labeled` | `issue.opened` / `issue.closed` / `issue.labeled` | `issue{title, labels, body (by ref)}`, `label` |
| `issues.closed` in `compliance-framework/workflows` with a `<!-- train-state` block | `issue.closed` | **`train{month, manifest, dryRun, repos[]{name, versions, bump}}`** (extractor `ccf-train`: parses the hidden JSON; `bump` computed from each repo's previous final release vs released version). The train writes `dry_run` with `omitempty`, so **real trains omit it**: the extractor maps `dry_run` → `dryRun` and **defaults it to `false`**; likewise `repos` → `[]`. A body without `manifest` yields no `train` fact. Fixtures use a real train body (with and without `dry_run`) |
| `issue_comment.created` | `issue.commented` (or PR comment → same type with `object.kind: pr`) | `text` (by ref) |
| `pull_request.opened` / `synchronize` / `ready_for_review` / `labeled` / `closed` (merged) / `closed` | `pr.opened` / `pr.updated` / `pr.ready_for_review` / `pr.labeled` / `pr.merged` / `pr.closed` | `pr{draft, head, base}`, `label` |
| `pull_request_review.submitted` | `pr.review.submitted` | `review{state (lower-case), commit}` |
| `check_suite.completed` | `pr.checks.completed` (one per associated PR; IDs `gh-<delivery>/<n>`) | `checks{state}` |
| `release.published` | `release.published` | `release{tag, previousTag, semver{…, bump}, prerelease}` (`previousTag` = previous **final** release by semver) |
| `deployment_status` (success/failure) | `deploy.succeeded` / `deploy.failed` | `deploy{environment, sha, url}`; **linked to work items** whose merged PR's merge commit is an ancestor-or-equal of `sha` (checked via compare API, newest 20 merged PRs) |
| anything else | dropped (metric) | |

Actor (`event.actor`):

| Field | Resolution |
|---|---|
| `id` | `sender.login` |
| `isBot` | `sender.type == "Bot"` |
| `isFactory`, `identity` | sender is `ccf-factory[bot]` → `author`; `ccf-factory-reviewer[bot]` → `reviewer` |
| `tier` | first matching Provider `trust` rule: Group (`orgRole admin` via `GET /orgs/{org}/memberships/{user}` → `role == admin`, cached 10 min), `authorAssociation`, `bots`; else `anyone` |

Untrusted text (issue/PR bodies, comments) is stored as **content refs** (`github://owner/repo/issues/42#body`, `…#comment-123`), not inline; CEL gets `event.text`/`issue.body` only for **trusted** sources needed by rules (the train issue in `workflows`) — the normaliser inlines bodies only when the actor tier is `maintainer` or `bot` from the allow-list; otherwise the field is the ref string.

## Observe, list and projection

`Observe(ref)` — one GraphQL query per object:

- Issue: state, labels, sub-issues, linked PRs, comments count, the factory marker comment.
- Deploy: for work items with merged PRs, the latest `github-pages` deployment of the default branch → `item.deploy{state, sha, url, containsMerge}` (`containsMerge`: every merge commit of the item's PRs is an ancestor of `sha`, via the compare API). docs' Pages workflow cancels in-progress deploys, so only the latest one matters.
- PR: `isDraft`, `headRefOid`, `baseRefName`, labels, `reviews(last:20){author, state, commit{oid}}` → `approvedBy[]{identity, commit}`, `reviewThreads(first:100){isResolved}` → `unresolvedThreads`, `commits(last:1){statusCheckRollup{state}}` → `checks{state, settled}`, merged/mergedAt/mergeCommit, author (factory or not).

`List(scope)` — open issues and PRs in scope repos updated in the last 90 days (search API, paginated), plus objects carrying the marker.

`Project(ref, p)`:

- **Marker comment** — one per (work item, object), authored by the `author` App, upserted (find by `<!-- factory:wi ` prefix, edit if digest differs):

  ```markdown
  <!-- factory:wi v=1 id=wi_01JA… state=release:drafting,train:compliance-framework%2Fworkflows%23123 runs=release-docs:1 links=github-public:compliance-framework%2Fdocs%2357 -->
  🏭 Tracked by CCF Factory — [view](https://<public-url>/workitems/wi_01JA…)
  ```

  Values URL-encoded; structured values (e.g. `plan`) as base64url JSON with prefix `b64:`; max 60 KB (else state stored only in the dispatcher and a warning logged).
- **Labels** — ensure the projection's factory-owned labels; never remove labels the factory doesn't own.

## Effects

| Op | API | Idempotency |
|---|---|---|
| `comment`, `pr.comment` | `POST /repos/{o}/{r}/issues/{n}/comments` | body ends with `<!-- factory:effect <key> -->`; search last 100 comments for the key before creating |
| `label.add` / `label.remove` | `POST/DELETE …/labels` | naturally idempotent |
| `create-issue` | `POST /repos/{o}/{r}/issues` | key in body marker; search issues by marker (`in:body`) first |
| `sub-issue.add` | `POST /repos/{o}/{r}/issues/{n}/sub_issues` | 422 "already" → success |
| `close` | `PATCH …/issues/{n} {state: closed}` | idempotent |
| `link-duplicate` | comment + label `duplicate` + close | per sub-op |
| `pr.open` | `POST /repos/{o}/{r}/pulls` | look up open PR with same head first |
| `pr.update` | `PATCH …/pulls/{n}` (title/body) | idempotent |
| `pr.retarget` | `PATCH …/pulls/{n} {base}` | idempotent |
| `pr.merge` | `PUT …/pulls/{n}/merge` (method from repo settings; `sha` = observed head) | 405 "already merged" → success; 405 review-required → fall back to `label.add` of the repo's merge label if configured (Q5), result `labelled-for-merge` |
| `review.comment` / `review.request-changes` / `review.approve` | `POST …/pulls/{n}/reviews` (with `commit_id` = head, inline `comments[]`) | key in review body marker; skip if a review with the key exists |
| `thread.reply` / `thread.resolve` | GraphQL `addPullRequestReviewThreadReply` / `resolveReviewThread` | reply key marker; resolve idempotent |

Rate limits: respect `X-RateLimit-Remaining`/`Retry-After`; exponential backoff (max 5 tries) for 5xx/secondary limits; effects return `retryable` errors to the API, which surfaces 503 to the agent.

## Tests

- `testdata/webhooks/*.json` (recorded, scrubbed) → golden normalised Events, one per mapping row (incl. a real train-state body from `workflows`).
- HMAC failure, duplicate delivery, `202` only after `Accept` succeeded (a failing sink returns `503`, so GitHub retries).
- One `check_suite` delivery with 3 PRs → 3 Events with distinct IDs.
- Startup redelivery: deliveries listed by the fake GitHub but unseen are ingested once.
- Token body assertions (repositories always set), cache expiry with fake clock.
- Observe golden parsing; marker parse/print round-trip; projection idempotency.
- Effects against `httptest` GitHub; replay with same key creates nothing.

## Decisions taken in GH-S1-T01

- **`Minter.Token(ctx, provider.GitCredentialRequest) (provider.Credential, error)`**, not `Token(ctx, identity, repos []string, perms, ttl)`. Taking the request means `GitCredentialRequest.Validate` is always the first check, and one repo per token is in the type: a `Repo` string, never a list. The minter refuses (`ErrInvalid`, before any GitHub call): whatever `Validate` refuses; **any `ReadRepos`**, since #29 every read repo gets its own `ReadOnlyRequest` and a multi-repo token would also have to fit one installation; **an empty permission set**, since GitHub would then grant everything the installation has; unknown permission names or levels (`read` < `write` < `admin`); a repo that isn't `owner/name`; and an unknown identity. The access-token body is built only by a constructor that needs one repo name and at least one permission, and neither JSON field has `omitempty`. So "a request without `repositories`" can't happen, and there is no test-only `panic` (AGENTS.md: no `panic` outside `main`).
- **The identity's permissions are its installation's.** `AppIdentity` has no permission list (contracts unchanged), so the ceiling is the `permissions` GitHub reports for the installation, from the installation lookup. A request asking for more is refused locally, with the missing permissions named. If the cached installation says no, the minter looks it up again once before refusing, so a permission granted since then is seen without waiting for the cache to expire.
- **Installation lookup: `GET /repos/{owner}/{repo}/installation`**, cached per (identity, owner) for `InstallationTTL` (default 1 h). It works for organisation and user accounts alike (dev mock repos may live under a user), where `/orgs/{org}/installation` works only for organisations. The entry is dropped when the token call answers 401, 404 or 422 (App reinstalled, repo removed from the installation).
- **JWT via `ghinstallation.AppsTransport`**, as the LLD says. It is used only for its signing transport; its own installation `Transport` and token cache aren't, because they key a token by installation rather than by (identity, repo, permissions). The private key is parsed with the standard library (PKCS#1 or PKCS#8) so that a bad key fails `Connect` with an `ErrInvalid` that carries no key material. ghinstallation stamps the JWT with the wall clock (iat now−30 s, exp iat+2 min), so the JWT doesn't follow the injected clock; caches and credential expiry do.
- **Expiry.** `Credential.ExpiresAt = min(GitHub's expires_at, now + min(TTL, 1 h))`. A cached token is reused while `now < expires_at − 5 min`, whatever the new request's TTL, and its `ExpiresAt` is then bounded by GitHub's expiry. Expired entries are pruned at each mint.
- **Concurrent mints share one call** per cache key, and concurrent lookups share one per (identity, owner). The flight is written in the package because `golang.org/x/sync` isn't on the allow-list. If the caller running the call gives up (its context ends), the callers waiting on it try again under their own contexts instead of inheriting its cancellation.
- **Errors.** GitHub error responses are `*tokens.APIError{Op, StatusCode, Message (truncated), RequestID}`, and `errors.Is` maps them: 401 → `tokens.ErrUnauthorized`, 403 → `tokens.ErrForbidden`, 404 → `provider.ErrNotFound`, 422 → `provider.ErrInvalid`. The provider package has no auth errors, so the two new ones live in `tokens`. These are retried, as a `*provider.RetryableError` wrapping the `APIError`: 429, a 403 that is a rate limit (`Retry-After`, `X-RateLimit-Remaining: 0` with its reset, or a "rate limit" message, with `RetryAfter` set from them), 5xx, and transport failures. The caller's own cancellation stays a context error.
- **Defence in depth.** A token whose response lists other repos than the one asked for is refused and not cached. The App clients never follow redirects, so a redirect can't carry the JWT to another host. Debug logs name the identity, repo, permissions, installation and expiry, never a token or JWT, and a test checks this.
- **Adapter skeleton (`internal/provider/github`).** `NewAdapter(Options{HTTPClient, Clock, Logger, Groups})`, with `Type() == "github"`. `Connect` validates the spec, resolves each identity's `privateKeyRef` through the `SecretResolver` and builds the minter. It makes **no GitHub call**, because the provider controller connects every Provider one after another at startup. The credential's `Host` comes from `apiURL` (`api.github.com` → `github.com`, else the GHE host), and `Provider` is the Provider's name. Methods not built yet return `provider.ErrUnsupported` with TODOs naming the task that builds them: `Recover`, `Normalise` and `ResolveGroup` are GH-S1-T02, `Observe`, `List` and `Project` are GH-S1-T03, and `Apply` is GH-S1-T04. `Handler` answers 503 until GH-S1-T02, so GitHub redelivers. The adapter isn't registered in `serve` yet (GH-S1-T04).
- **Health.** `Conn.Health()` doesn't block: it aggregates each identity's last outcome. That outcome comes from `Conn.CheckHealth(ctx)` (`GET /app` with each App's JWT, concurrently) or from a mint whose result says something about the identity: success, 401/403, or retryable. Refused requests, repos the App isn't installed on, and abandoned checks don't count. The result is `unknown` before any outcome, `ok` when every identity is fine, `down` when all fail and `degraded` in between, with a secret-free message per failing identity. `serve` should call `CheckHealth` at connect and on resync (GH-S1-T04).
- **`internal/provider/github/githubtest`** is the httptest GitHub. It verifies every App JWT (RS256 against the App's public key, at most 10 minutes of validity), records raw bodies, injects failures per route and can hold token calls (`Gate`) for the dedupe tests. GH-S1-T02..T04 extend it.
- **Dependencies:** `github.com/bradleyfalzon/ghinstallation/v2` v2.19.0, which pulls in `github.com/golang-jwt/jwt/v4` v4.5.2, `github.com/google/go-github/v88` v88.0.0 and `github.com/google/go-querystring` v1.2.0, all indirect. go-github isn't used directly yet. The newest major is v93, but ghinstallation v2.19.0 pins **v88**, so GH-S1-T02..T04 should use v88 to link one copy, unless ghinstallation moves first.
