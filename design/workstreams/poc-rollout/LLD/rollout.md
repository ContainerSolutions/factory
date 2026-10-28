# LLD — PoC rollout and runbooks (RUN)

> Workstream: [RUN](../../definitions.md#run-poc-rollout) · Tasks: RUN-S0…S4 · Success criteria: [10 §PoC scope](../../../10-poc.md#poc-scope)

## Environments

| Env | Docs target | Trains | RuleSets | Purpose |
|---|---|---|---|---|
| **dev** | `mock-docs` | mock trains (`repos.mock.yaml`) + `factory dispatch` fake events | `dev-release-docs`, `dev-docs-epic` | build and debug every flow safely |
| **prod** | `docs` | real trains (`repos.yaml`) | `poc-release-docs`, `poc-docs-epic`, `poc-docs-human-review` | the PoC demonstration |

Both run from the **same dispatcher** and catalog; RuleSets decide the target.

## GitHub Apps

Created by an org owner (RUN-S0-T02). Settings → Developer settings → GitHub Apps → New:

| Setting | `ccf-factory` (author + events) | `ccf-factory-reviewer` |
|---|---|---|
| Webhook | **active**, URL `https://<tunnel-host>/hooks/github-public`, secret = random 32 bytes | inactive |
| Repository permissions | Contents RW, Pull requests RW, Issues RW, Checks R, Actions R, Deployments R, Metadata R | Contents RW, Pull requests RW, Issues R, Checks R, Metadata R |
| Organization permissions | Members R | — |
| Events | Issues, Issue comment, Pull request, Pull request review, Pull request review comment, Pull request review thread, Release, Deployment status, Check suite, Sub issues | — |
| Install on | `docs`, `mock-docs`, `workflows`, `mock-*` | `docs`, `mock-docs` |
| Device flow | not needed: agents are enrolled ([P1 §6](../../../proposals/P1-agent-workspaces.md#6-enrollment-replaces-the-device-flow)) | — |
| User authorization (UI login) | enable "Request user authorization (OAuth) during installation": **no**; use its OAuth client for UI login (callback `https://<tunnel-host>/api/v1/auth/callback`) | — |

Neither App needs Commit statuses: Observe derives `item.pr.checks` from the head commit's check suites (Checks R), reading the commits with Contents R.

Neither App is ever given a ruleset **bypass**. `ccf-release-bot` and `ccf-repo-admin` are never used by the factory.

## Secrets

Stored in the operator's password manager / a local `deploy/poc/.env` (gitignored, 0600) on the dispatcher host only:

| Name | Source |
|---|---|
| `FACTORY_SECRET_CCF_FACTORY_APP_PRIVATE_KEY`, `…_REVIEWER_APP_PRIVATE_KEY` | App private keys (PEM) |
| `FACTORY_SECRET_GITHUB_PUBLIC_WEBHOOK_SECRET` | webhook secret |
| `FACTORY_OAUTH_CLIENT_ID`, `FACTORY_OAUTH_CLIENT_SECRET` | `ccf-factory` OAuth client |
| `FACTORY_PUBLIC_URL` | the ngrok URL (config, **not a secret**; kept in `.env` only so the public repo doesn't carry it) |
| `FACTORY_SESSION_KEY`, `FACTORY_CLI_TOKEN` | `openssl rand -hex 32` |
| `FACTORY_SECRET_LOCAL_POOL_JOIN_TOKEN` | `openssl rand -hex 32`; shared with agent owners out of band |

## Dispatcher hosting

PoC: Gustavo's machine (or a small VM) running `factory serve` (built with `make build-release`), exposed with an **ngrok** tunnel on a stable `ngrok-free.dev` domain, forwarding to `localhost:8800` (set up 2026-10-16; the hostname lives in `deploy/poc/.env` as `FACTORY_PUBLIC_URL`, not in this public repo). No inbound ports. The tunnel only reaches the public listener (`:8800`: UI, `/api/*`, `/hooks/*`); the unauthenticated ops routes (`/healthz`, `/readyz`, `/metrics`) live on the localhost-only ops listener (`127.0.0.1:9090`), so they're never public. As defence in depth, the kit's ngrok Traffic Policy (`deploy/poc/ngrok-policy.yml`, passed with `--traffic-policy-file`) also denies those three paths. Webhooks are unaffected by ngrok's free-plan browser warning page; the UI login shows it once per browser. Long term: Cloud Run (Q12).

`deploy/poc/` (RUN-S1-T01): `README.md`, `.env.example` (names only), `run.sh` (loads `.env`, `exec factory serve --public-url "$FACTORY_PUBLIC_URL" --catalog-repo … --retention 2160h`), ngrok instructions (`ngrok http --url=$FACTORY_PUBLIC_URL --traffic-policy-file=deploy/poc/ngrok-policy.yml 8800`), `agent.yaml.example`. `deploy/poc/.env` is gitignored and `0600`.

## Agent onboarding (RUN-S2-T01)

1. `go install github.com/ContainerSolutions/factory/cmd/factory-agent@<ref>`.
2. The model logins the VM workspaces' gateway adds: `claude setup-token`, then `factory-agent login claude` (paste the token); `codex login` on this machine, then `factory-agent login codex`.
3. Host tooling: `git` (the host fetches the skills for each run bundle) and the workspace driver: `factory-agent host init` (tart on a Mac, kata on Linux). The harnesses, Docker and the docs tools ship in the workspace image; no `gh` login is needed.
4. Run the agent as a **dedicated OS user** (e.g. `factory-agent`) with **no `gh` login and no personal GitHub credentials** in its home. Place the enrollment token file (`factory agent enroll`, run by the dispatcher's admin), write `agent.yaml` (from example), set `lending`.
   The admin who mints the enrollment names its owner; supervise exchanges the one-time token for a refresh token kept 0600 in its state dir, so no GitHub login happens on the agent. Tasks only ever get the factory's per-task App tokens.
5. `factory-agent probe` → check the harnesses' logins; `factory-agent supervise` (or a launchd/systemd unit from the kit).

## Use case 1 runbook

| Step | Action | Expect |
|---|---|---|
| 1 | `factory dispatch --event deploy/poc/events/mock-train.json` (dev RuleSet, mock-docs) | `release-docs` task leased by an agent (its local-dev stack comes up in the task's VM); PR on mock-docs; `docs-review` runs on the **other harness** |
| 2 | Seed a wrong default in a fixture train's docs (or let the review find a real one) | review requests changes citing `DOCS-ACC-001`; `implement address-review` fixes; re-review approves |
| 3 | Run a real **mock train** in `workflows` (`workflow_dispatch`, manifest `repos.mock.yaml`, not dry run) | train closes `train:done` → dev flow runs end to end on mock-docs |
| 4 | Restart the dispatcher mid-flow | UI shows the same work item/state after the restart (from the state dir; nothing is rebuilt from markers); flow continues (criterion 6) |
| 5 | Switch on prod (`poc-release-docs` already loaded); wait for the monthly train | one docs PR on `docs`, reviewed and approved; humans merge (merge rule excluded) |

## Human-PR review

Enable `poc-docs-human-review` (already in catalog; confirm `docs` App installed). Open a test PR as a human → review within minutes; nothing else happens (criterion 4).

## Use case 2 runbook

1. On `mock-docs`: open "Add i18n with a PT-BR translation" as an org admin → `plan-epic` → sub-issues → stack of PRs → one stack review → (merge excluded) approve.
2. Repeat on `docs`.
3. RUN-S4-T01 (two approvals) enables merges → re-run or continue: stack merges bottom-up, Pages deploys, `ptbr-live` comment, epic closed (criterion 3).

## Operating the PoC

| Need | How |
|---|---|
| Stop everything | UI → pause global (or `factory dispatch` a `pause` command); kill switch label `factory:stop` on an item |
| Stop an agent | owner: `factory-agent pause` / Ctrl-C (drain) |
| Inspect a decision | UI → work item → "why" |
| Re-run with another harness | UI → run → re-dispatch with harness override |
| Logs | dispatcher JSON logs (`run.sh` → file); agent logs under `~/.factory-agent/logs` |

## Success criteria checklist

| # | Criterion | Evidence |
|---|---|---|
| 1 | train → merged docs PR, no human action | PR link, merged by `ccf-factory-reviewer` after RUN-S4 |
| 2 | wrong claim caught against running version | review thread with `DOCS-ACC-001` + fix commit |
| 3 | i18n epic → PT-BR live | Pages URL, closed epic |
| 4 | human PR reviewed, nothing else | PR timeline |
| 5 | Codex writes, Claude reviews; exact effective config | Run pages |
| 6 | restart loses nothing | before/after screenshots of the work item |
| 7 | no credentials stored/sent beyond scoped git tokens | code review (`FACTORY-CRED-001`) **plus the canary integration test** (below) |

### Credential canary test (criterion 7)

An automated integration test (`make test-credentials`, run in CI against fakes and once on the real PoC setup):

1. Start `factory serve` and one `factory-agent supervise` with **canary values** for every secret: App private keys, webhook secret, OAuth client secret, session key, CLI token, an enrollment token and the refresh token it yields, and a fake Anthropic/OpenAI key in the worker's environment.
2. Route dispatcher ↔ agent and dispatcher ↔ GitHub (fake) traffic through a recording proxy.
3. Run a fake task that performs effects and produces a transcript, **and** drive a UI login end to end: `/api/v1/auth/login` → fake GitHub authorize → `/api/v1/auth/callback` → token exchange. The proxy records **full request URLs** (query strings included), not just bodies and headers.
4. Assert that **no canary appears** in:
   - recorded URLs, bodies and headers, except each canary's one legitimate channel: the App key never leaves the dispatcher (only signed JWTs do), the enrollment and refresh tokens appear only in `POST /api/v1/agents` (and the refresh token in its response), and the OAuth client secret appears **only** in the token-exchange request to GitHub (never in a redirect URL or browser-facing response);
   - dispatcher and agent logs;
   - the store dump and uploaded transcripts;
   - the harness process environment (`/proc/<pid>/environ` or `ps eww`);
   - the task workspace files.

