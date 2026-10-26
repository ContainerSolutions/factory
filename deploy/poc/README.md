# PoC run kit

How a maintainer goes from nothing to a running dispatcher and a registered agent for the PoC
([rollout.md](../../design/workstreams/poc-rollout/LLD/rollout.md)). The dispatcher
(`factory serve`) runs on the operator's machine behind an ngrok tunnel. Agents
(`factory-agent worker`) run on their owners' machines. One dispatcher serves both environments:
the `dev-*` RuleSets target `compliance-framework/mock-docs` and the `poc-*` RuleSets target
`compliance-framework/docs`.

| File | What it is |
|---|---|
| `.env.example` | every variable the dispatcher reads, names and comments only. Copy it to `.env` (gitignored, 0600). |
| `run.sh` | loads `.env`, checks it, builds, then runs `factory serve` (`make poc-up`). |
| `ngrok-policy.yml` | the tunnel's Traffic Policy: it denies `/healthz`, `/readyz` and `/metrics`. |
| `agent.yaml.example` | a commented `factory-agent` config. |
| `events/mock-train.json` | the closing event of a real mock release train (`workflows#132`), for the first dev dispatch (mock-docs only). |
| `logs/` | the dispatcher's JSON logs, one file per start (gitignored). |

The dispatcher's state lives outside the repository, in `~/.factory/state` (§6, State).

Commands run from the repository root unless a step says otherwise.

## 1. Prerequisites (dispatcher host)

- **Go**, the version in `go.mod`, and **Node `^20.19` with npm `^10.1`** (`web/.nvmrc`, `web/package.json`): `make build-release` builds the UI and embeds it.
- **git**. `compliance-framework/ccf-skills` is private: the dispatcher resolves each TaskSpec's skills ref with `git ls-remote` using a read-only token it mints as ccf-factory (factory#68), so ccf-factory must be installed on `ccf-skills` (§2). The host needs no GitHub login of its own; don't give the dispatcher host's git your personal credentials for this.
- **ngrok** (agent v3) with an account and a stable `ngrok-free.dev` domain. Run `ngrok config add-authtoken` once.
- `openssl`, `curl`, and `jq` (optional, for reading the logs).
- Admin on the `compliance-framework` org, to create and install the Apps.

## 2. GitHub Apps

An org owner creates two Apps: Organization settings → Developer settings → GitHub Apps → New GitHub App (rollout.md §GitHub Apps; done for the PoC on 2026-10-16).

| Setting | `ccf-factory` (author, the only event source) | `ccf-factory-reviewer` |
|---|---|---|
| Webhook | **active**, URL `https://<tunnel-host>/hooks/github-public`, secret: `openssl rand -hex 32` (it goes in `.env` too) | inactive |
| Repository permissions | Contents RW, Pull requests RW, Issues RW, Checks R, Actions R, Deployments R, Metadata R | Contents RW, Pull requests RW, Issues R, Checks R, Metadata R |
| Organization permissions | Members R (resolves the `maintainers` and `readers` Groups) | none |
| Events | Issues, Issue comment, Pull request, Pull request review, Pull request review comment, Pull request review thread, Release, Deployment status, Check suite, Sub issues | none |
| Install on | `docs`, `mock-docs`, `workflows`, `mock-*`, `ccf-skills`, `local-dev` | `docs`, `mock-docs`, `ccf-skills` |
| Enable Device Flow | **yes**: agents register with the owner's device-flow login | no |
| Callback URL (UI login) | `https://<tunnel-host>/api/v1/auth/callback`. "Request user authorization (OAuth) during installation": **no**. | none |

- **Never** give either App a ruleset bypass.
- **No Commit statuses permission.** Observe reads a PR's commits (Contents R) and its head's check suites (Checks R) for `item.pr.checks`; it doesn't read commit statuses, so neither App needs Commit statuses. A CI that only posts statuses doesn't count there (GitHub Actions reports check suites).
- **Private repos.** `ccf-skills` and `local-dev` are private, so a run gets a read-only token for them, minted with the App of its TaskSpec's identity. Each App needs the private repos its runs read:
  - `ccf-skills`: **both** Apps, since every TaskSpec uses skills (docs-review and merge run as the reviewer).
  - `local-dev`: **ccf-factory**, for release-docs (identity `author`), which mounts it read-only.

  A missing installation leaves the task `Pending` with reason `git-credential`.
- **mock-docs was created after the first installs.** If an installation uses "Only select repositories", add `mock-docs` to **both** installations: Organization settings → GitHub Apps → the App → Configure.
- For each App, generate a private key (App settings → General → Private keys) and note the **App ID** (top of the General page). For ccf-factory, also note the **Client ID** and generate a **client secret**.
- **The App IDs go in the catalog, not in `.env`.** `catalog/providers/github-public.yaml` has the placeholders `author: { appID: 1, … }` (ccf-factory) and `reviewer: { appID: 2, … }` (ccf-factory-reviewer). App IDs aren't secret. §4 says where to set them.

## 3. Secrets

```bash
cp deploy/poc/.env.example deploy/poc/.env && chmod 600 deploy/poc/.env
```

Fill it in with an editor. Each line of `.env.example` says what goes there:

- **`FACTORY_SESSION_KEY`, `FACTORY_CLI_TOKEN`, `FACTORY_SECRET_LOCAL_POOL_JOIN_TOKEN`**: `openssl rand -hex 32` each. `FACTORY_SECRET_GITHUB_PUBLIC_WEBHOOK_SECRET` is the value you set on ccf-factory's webhook (also `openssl rand -hex 32`).
- **`FACTORY_OAUTH_CLIENT_ID` / `_SECRET`**: ccf-factory's client ID and client secret. The client ID also serves the agents' device flow.
- **The two `…_PRIVATE_KEY`s**: the PEM files. `run.sh` sources `.env` with bash, so either paste the PEM inside double quotes or read it from a file outside the repo: `FACTORY_SECRET_CCF_FACTORY_APP_PRIVATE_KEY="$(cat /path/to/ccf-factory.pem)"`.
- **`FACTORY_PUBLIC_URL`**: the tunnel URL, `https://<name>.ngrok-free.dev`. It isn't a secret, but it's kept out of the public repo.

`run.sh` refuses a `.env` that is missing, not yours, or not mode 600, and names (never prints) any variable that's unset. `cmd/factory/deploykit_test.go` keeps `.env.example` in step with the catalog's secret references.

Share the join token with agent owners out of band, never in a repo or an issue.

## 4. Catalog

`factory serve` reads the catalog from git by default: `catalog/` of `https://github.com/compliance-framework/factory` at `main`, polled every minute. `run.sh` passes that (`--catalog-repo … --catalog-ref "${FACTORY_CATALOG_REF:-main}" --catalog-path catalog`), or `--catalog-dir "$FACTORY_CATALOG_DIR"` when that is set in `.env`.

`main` works as is once the real App IDs are merged into `catalog/providers/github-public.yaml` and `ccf-skills` has the tag the TaskSpecs pin (`ref: v0.8.0`). A Provider change needs two approvals. Until both are true, run from a local copy:

```bash
cp -R catalog "$HOME/factory-poc-catalog"
```

Set the real App IDs (replace `<CCF_FACTORY_APP_ID>` and `<CCF_FACTORY_REVIEWER_APP_ID>`):

```bash
sed -i.bak -e 's/appID: 1,/appID: <CCF_FACTORY_APP_ID>,/' -e 's/appID: 2,/appID: <CCF_FACTORY_REVIEWER_APP_ID>,/' "$HOME/factory-poc-catalog/providers/github-public.yaml" && rm "$HOME/factory-poc-catalog/providers/github-public.yaml.bak"
```

While `ccf-skills` has no `v0.8.0` tag, pin the skills to a branch. With a ref that doesn't resolve, every dispatch is retried forever (`reconcile failed; retrying … resolve skills …`):

```bash
sed -i.bak 's/ref: v0\.8\.0/ref: main/' "$HOME"/factory-poc-catalog/taskspecs/*.yaml && rm "$HOME"/factory-poc-catalog/taskspecs/*.yaml.bak
```

```bash
go run ./cmd/factory validate --dir "$HOME/factory-poc-catalog"
```

Expect `valid (dir:…): 0 errors, 11 warnings`. The warnings are the expected CAT1200 overlaps between the dev and prod RuleSets. Then set `FACTORY_CATALOG_DIR=<your home>/factory-poc-catalog` (an absolute path) in `.env`. The directory is re-read every minute too.

## 5. Tunnel

In its own terminal, with `FACTORY_PUBLIC_URL` in the environment (for example `export FACTORY_PUBLIC_URL=https://<name>.ngrok-free.dev`):

```bash
ngrok http --url="$FACTORY_PUBLIC_URL" --traffic-policy-file=deploy/poc/ngrok-policy.yml 8800
```

The dispatcher listens on **8800**, not 8080 or 8000: local-dev's CCF stack binds those (its API and UI), and release-docs and docs-review bring that stack up on this machine. `make poc-up POC_ADDR=127.0.0.1:<port>` picks another; if your `.env` sets `FACTORY_ADDR`, that wins, so update it too. The tunnel reaches only the public listener on port 8800: the UI, `/api/v1/*` and `/hooks/*`. The ops routes are on a separate loopback-only listener, and the policy denies their paths too. Set ccf-factory's webhook URL to `$FACTORY_PUBLIC_URL/hooks/github-public` and its callback URL to `$FACTORY_PUBLIC_URL/api/v1/auth/callback`.

## 6. Start the dispatcher

```bash
make poc-up
```

`make poc-up` runs `deploy/poc/run.sh`, which:

1. loads and checks `.env` (§3);
2. runs `make build-release` (`web/` with `npm ci`, then `go build -tags webui`). Use `SKIP_BUILD=1 make poc-up` to reuse `bin/factory`;
3. runs `bin/factory serve --addr 127.0.0.1:8800 --ops-addr 127.0.0.1:9090 --public-url "$FACTORY_PUBLIC_URL" <catalog flags> --retention 2160h --state-dir <FACTORY_STATE_DIR, default ~/.factory/state> --log-format json --anti-affinity-wait <FACTORY_ANTI_AFFINITY_WAIT, default 10m>`, plus any arguments given to `run.sh` (for example `deploy/poc/run.sh --log-level debug`). `FACTORY_ADDR` in `.env` overrides `--addr`.

The JSON log goes to the terminal and to `deploy/poc/logs/factory-serve-<UTC time>.log` (one file per start).

### State

The dispatcher keeps its store in `FACTORY_STATE_DIR` (default `~/.factory/state`, created 0700): `snapshot.json`, `journal.jsonl` (every change since the snapshot, fsync'd), `transcripts/` and a `LOCK`. Tasks, runs, transcripts, events, decisions, budgets, agent registrations and leases survive a restart, and an agent that keeps heartbeating across one finishes its run. The start-up log line `store: loaded the state dir` says what was loaded. The work-item rebuild from the GitHub markers still runs, and adds only the work items the state doesn't hold.

`factory serve` refuses to start when the dir or a file in it can be read by others (it names the `chmod`), when another dispatcher has it open, or when it can't load it (an unknown format, or a corrupt journal line, with its number). To start clean, stop the dispatcher (§11) and move the dir aside, keeping it as evidence: `bin/factory state reset --yes` (it refuses while a dispatcher has the dir open, and prints where it moved it: `<dir>.<UTC time>`). The next start creates an empty one, and work items come back from the markers.

`factory state` answers "why did this run fail?" from the state dir, with no server, API or token: it reads the files read-only and takes no lock, so it works while the dispatcher runs, and on a copy of the dir. `--state-dir` defaults to `$FACTORY_STATE_DIR`, else `~/.factory/state`, as for `run.sh` (it doesn't read `.env`: if `.env` sets `FACTORY_STATE_DIR`, pass that dir with `--state-dir`). Tables by default; `--json` prints the stored objects.

```bash
bin/factory state tasks [--phase Failed] [--spec implement] [--workitem W] [--since 24h]  # id, spec, phase, reason, work item, created, last run
bin/factory state task tsk_…                 # spec, inputs, harness, status, runs with their Result, and the decision that dispatched it
bin/factory state run run_… [--transcript]   # the Result in full, usage, progress; --transcript prints the transcript (JSON Lines)
bin/factory state workitem wi_…              # links, state, counters, decisions (rule, verdict, actions) and tasks
bin/factory state events [--since 1h] [--type pr.opened] [--workitem W]   # with whether the rule controller processed each
bin/factory state log [--since 10m]          # the journal (changes since the last snapshot) as a timeline, with task phase changes and run ends
```

An unknown id prints the ids nearest to it by prefix. Nothing it prints is secret (the store holds no credential, and transcripts are redacted by the agent), so its output can be pasted into an issue. `jq` on `snapshot.json` (one object per line) and `journal.jsonl` works too.

## 7. Check it

Log lines to look for:

- `catalog swapped in`, then `catalog loaded`;
- `provider connected … webhook=<public url>/hooks/github-public`;
- no `pool join token not set`;
- `provider health` without `degraded`. A degraded provider means a wrong App ID or key.

`catalog rejected; keeping the live catalog` means an invalid catalog: `factory validate --dir` the directory.

The ops listener answers on the dispatcher host only, with no auth:

```bash
curl -s http://127.0.0.1:9090/healthz; curl -s http://127.0.0.1:9090/readyz; curl -s http://127.0.0.1:9090/metrics | grep '^factory_' | head
```

`/healthz` answers `ok` and `/readyz` answers `ready`.

The API takes the CLI token as a bearer token. Load just that one variable into your shell, without printing it:

```bash
export FACTORY_CLI_TOKEN="$(sed -n 's/^FACTORY_CLI_TOKEN=//p' deploy/poc/.env)"
```

```bash
curl -s -H "Authorization: Bearer $FACTORY_CLI_TOKEN" http://127.0.0.1:8800/api/v1/providers | jq '.items[] | {name: .provider.metadata.name, health}'
```

Expect `"status": "ok"`. Then:

- **Webhook:** ccf-factory's settings → Advanced → Recent Deliveries. Redeliver one, or wait for the next event. `202` means it was accepted, `200` a duplicate, and `401` a webhook secret that doesn't match `.env`.
- **UI:** open `$FACTORY_PUBLIC_URL` and sign in with GitHub. ngrok's free-plan warning page shows once per browser. Any `compliance-framework` org member can read (the `readers` Group). Controls (pause, cancel, re-dispatch, resync) need an org admin (`maintainers`).

## 8. Agent onboarding

For each agent owner, on their machine (rollout.md §Agent onboarding). The owner must be a `compliance-framework` org admin: the Pool's owners are the `maintainers` Group.

1. **A dedicated OS user** (for example `factory-agent`), with no `gh` login and no personal GitHub credentials in its home. Tasks get only the factory's per-task App tokens (`GH_TOKEN`). On macOS: `sudo sysadminctl -addUser factory-agent -password -` (it prompts for the password). On Linux: `sudo useradd -m factory-agent`. Run every step below as that user, for example `sudo -iu factory-agent`.
2. **Install the agent.** `go install github.com/compliance-framework/factory/cmd/factory-agent@main` puts it in `$(go env GOPATH)/bin`. Or `make build` in a checkout, which gives `bin/factory-agent`.
3. **Base tooling.** The worker refuses to start without `git`, `gh`, `jq`, `python3` and the `gh` extensions:

   ```bash
   gh extension install agynio/gh-pr-review && gh extension install github/gh-stack
   ```

   No `gh auth login` is needed. On Linux, Claude Code's sandbox needs `bubblewrap` and `socat`; macOS uses `sandbox-exec`.
4. **Labels.** Each label is probed at start.
   - `has-local-stack`: Docker running with at least 8 GB of memory, compose v2, and a `local-dev` checkout (with its `.env`) set as `localDev`.
   - `gif-tooling`: `npm install -g playwright && npx playwright install chromium`, plus `brew install gifski vhs ffmpeg`.

   No TaskSpec requires them any more: release-docs and docs-review-verify declare their environment (local-dev's showcase), which a VM workspace (`factory-agent supervise`) brings up.
5. **Harness logins**, once, into dedicated config dirs. You run these; the factory never does.
   - Codex: `CODEX_HOME=~/.factory-agent/codex-home codex login --device-auth`.
   - Claude Code: `CLAUDE_CONFIG_DIR=~/.factory-agent/claude claude`, then `/login` once. The alternative is `claude setup-token`, with `CLAUDE_CODE_OAUTH_TOKEN` in the worker's environment.

   Keep `ANTHROPIC_API_KEY` out of the worker's environment unless you mean API-key billing. release-docs, implement, plan-epic and merge ask for codex; docs-review asks for claude-code. The full use case 1 needs both.
6. **The join token**, from the dispatcher operator, in a file only you can read:

   ```bash
   mkdir -p ~/.config/factory-agent && (umask 077 && cat > ~/.config/factory-agent/join-token)
   ```

   Paste the token, press Enter, then Ctrl-D.
7. **The config.** Copy `deploy/poc/agent.yaml.example` to `~/.config/factory-agent/agent.yaml`, then `chmod 600` it. Edit:
   - `dispatcher`: `$FACTORY_PUBLIC_URL`, or `http://127.0.0.1:8800` on the dispatcher's own machine;
   - `localDev`;
   - the harnesses and models your plans allow. The codex model in the example (`gpt-6.1-sol`) is a placeholder: put `<a gpt-5* or gpt-6* model your codex sign-in offers>` (see `/model` in codex) in `models` and `default`;
   - `lending`.

   Unknown keys are errors.
8. **Probe.** This prints the harnesses, labels, dropped items and base tooling. It never calls the dispatcher:

   ```bash
   factory-agent probe
   ```

   To see the stack a train would bring up, without running anything:

   ```bash
   factory-agent stack up --train compliance-framework/workflows#132 --dry-run
   ```

9. **Smoke the harnesses** (maintainers, from a repo checkout as the agent user; never in CI). Each one runs a trivial task end to end through the real harness. They default to the dedicated dirs above; see `make help` for the overrides. smoke-codex has no default model: set `FACTORY_SMOKE_CODEX_MODEL` to a gpt-5* or gpt-6* model your codex sign-in offers (see `/model` in codex).

   ```bash
   FACTORY_SMOKE_CODEX_MODEL=<a gpt-5* or gpt-6* model your codex sign-in offers> make smoke-codex
   ```

   ```bash
   make smoke-claude
   ```

10. **Run the worker:**

    ```bash
    factory-agent worker
    ```

    There is no separate `register` command. On first start the worker prints `To register this agent, open https://github.com/login/device and enter the code XXXX-XXXX`. Approve it as the owner. The agent then registers as `agt_<login>-<host>` and shows on the UI's Pools page (or `GET /api/v1/agents`).

    The user token is used once and never stored, and the agent token lives in memory. **Every worker restart means a new device login**; a dispatcher restart doesn't, since it keeps the agent's (hashed) token in its state dir (§6, State), unless it starts on an empty one.

    The worker logs to stderr, as text by default (`--log-format json`, `--log-level debug`). It writes no log file, so redirect it yourself, for example `factory-agent worker 2>&1 | tee -a ~/factory-agent.log`.

### A Linux workspace host (`factory-agent supervise`, kata)

`factory-agent supervise` runs each leased task in its own VM behind a gateway that holds every credential, instead of on the host as `worker` does. On an Ubuntu x86-64 machine (KVM, cgroup v2, systemd, at least 120 GiB free on `/var/lib`) each VM is a Kata microVM, and several run at once ([workspace-kata.md](../../design/workstreams/agent/LLD/workspace-kata.md)). Docker, if installed, is left alone: the factory runs its own containerd. From a checkout, as the owner's own user (not root; the workspaces are that user's), with the join token as in step 6:

1. `make kata-up` builds `bin/factory-agent` and runs `bin/factory-agent host init --driver kata`: it asks before installing its root helper (`/usr/local/libexec/factory-agent/kata-helper`, with a sudoers rule that lets you run only it with `sudo -n`), then prompts for sudo once. `HOST_INIT_FLAGS=--dry-run` prints the plan and the sudo script without changing anything. Then `bin/factory-agent host check`: every step ok.
2. The model logins, which only the gateway ever holds. Both go to `~/.local/state/factory-agent/logins/` (0600).
   - claude: run `claude setup-token` on its own and copy its output, then `bin/factory-agent login claude`: it asks for the paste; paste, then press Enter and Ctrl-D (the token stays out of argv and the shell history). Where a clipboard tool is installed, `xclip -o -selection clipboard | bin/factory-agent login claude` or `wl-paste | bin/factory-agent login claude` does the same.
   - codex: install the codex CLI on this host ([github.com/openai/codex](https://github.com/openai/codex)), run `codex login` (with ChatGPT), then `bin/factory-agent login codex`, which imports `~/.codex/auth.json`. Don't copy another machine's `auth.json`: both machines' gateways would refresh the same login, and each refresh spends the other's refresh token. A host without a codex login runs without codex: supervise drops the harness with a warning (and exits when no configured harness has a login).
3. The config as in step 7, with `workspace: {driver: kata}` (the default on Linux) and the `lending.maxConcurrent` you want: supervise caps it by memory, `(physical memory − 4 GiB) / workspace.size.memory`, and by 63, with a warning. Steps 3 to 5 (the base tooling, the labels' prerequisites, the harness sign-ins) don't apply to the host: the harnesses run in the VMs, and a configured harness is offered when its login is stored (the config still names its `home` or `configDir`).
4. `bin/factory-agent supervise` (`--log-level debug`; `--keep-workspace` keeps each stopped VM and its run directory for a look). It registers like `worker` (step 10), refuses to start while `host check` fails, and destroys the VMs a killed supervise left. `sudo -n /usr/local/libexec/factory-agent/kata-helper list` shows the running ones.

The first run on a new host follows the checklist in [workspace-kata.md §The owner's run](../../design/workstreams/agent/LLD/workspace-kata.md#the-owners-run).

## 9. First dev dispatch (use case 1 on mock-docs)

`compliance-framework/workflows#132` is a real mock release train: it ran over `repos.mock.yaml` and finished on 2026-10-14, releasing mock-plugin-1 and mock-plugin-2 0.1.4. Its issue closed before the factory was listening, and a new mock train can't be started from `workflows` main today (`repos.mock.yaml` isn't there), so the dev trigger is that train's closing event, recorded, and injected with `factory dispatch`.

```bash
bin/factory dispatch --event deploy/poc/events/mock-train.json
```

It posts to `http://127.0.0.1:8800` by default (`--server`, or env `FACTORY_SERVER`) with `FACTORY_CLI_TOKEN` (§7), and prints the stored event's id (`inject/…`).

About the event:

- It is the `issue.closed` event GitHub would have sent for `compliance-framework/workflows#132`, with the train facts the GitHub adapter derives from the issue's train state (manifest `repos.mock.yaml`, ten mock repos, two patch releases). So only `dev-release-docs` fires, and it targets mock-docs. `cmd/factory/deploykit_test.go` checks that no `poc-*` rule fires and that no decision names `compliance-framework/docs`.
- The event has no id, so you can dispatch it again; each dispatch is a new event. Only one release-docs run per work item is active at a time.
- The agent reads the train's versions from the real issue, and escalations (`needs-human` plus a comment) land on that issue.

To see the decisions without storing anything:

```bash
jq '{event: (. + {id: "preview"})}' deploy/poc/events/mock-train.json | curl -s -H "Authorization: Bearer $FACTORY_CLI_TOKEN" -H 'Content-Type: application/json' -d @- http://127.0.0.1:8800/api/v1/catalog/dry-run | jq '.decisions'
```

What you should see (the UI's board and work-item pages, or `GET /api/v1/workitems` and `/api/v1/tasks`):

1. A work item linked to `workflows#132`, with state `release: drafting`, `manifest: repos.mock.yaml` and `train: …#132`.
2. A `release-docs` task for `compliance-framework/mock-docs`. It stays `Pending` with reason `no-capable-agent` until an agent offers codex `gpt-5*` or `gpt-6*`. Its VM workspace brings local-dev's showcase up at the train's versions, and the agent opens a PR on mock-docs.
3. A `docs-review-verify` task (claude-code, reviewer identity, with the same environment). Anti-affinity keeps a review off the agent that wrote the PR only when both run on the same harness (here release-docs is codex and the review claude-code, so it doesn't apply). When it does, with a single agent the review waits `FACTORY_ANTI_AFFINITY_WAIT` (`--anti-affinity-wait`, default **10 minutes**) before the author's own agent may take it; `0` turns anti-affinity off.
4. On changes requested, `implement` (codex, mode address-review) pushes a fix, and the review runs again, at most 3 implement runs per item. The 3rd changes-requested, or a `needs-human` or `blocked` outcome, escalates.
5. **It ends at the reviewer's approval.** `merge-release-docs` is excluded in every RuleSet until RUN-S4-T01. A human merge announces nothing, because `announce-release-docs` fires only after a factory merge run.

## 10. Operating

The commands below go to `POST /api/v1/commands` with the CLI token. The UI shows the same controls to maintainers. `factory dispatch` sends only events, not commands.

| Need | How |
|---|---|
| Stop everything | The UI's pause, or `curl -s -H "Authorization: Bearer $FACTORY_CLI_TOKEN" -H 'Content-Type: application/json' -d '{"type":"pause","target":{"scope":"global"}}' http://127.0.0.1:8800/api/v1/commands`. Resume with `"type":"resume"`. Scopes: `global`, `provider`, `queue` and `workitem`, each with `"name"` except global. |
| Stop one work item | Label any object linked to it `factory:stop`. Every action of that item is then suppressed. `factory:hold` and `do-not-merge` keep the merge rules off a PR. |
| Cancel or re-run a task | `{"type":"cancel","target":{"task":"<tsk_id>"}}`, or `{"type":"redispatch","target":{"task":"<tsk_id>"},"args":{"harness":{"name":"claude-code"}}}`; or from the UI's run page. |
| Re-read the catalog now | `{"type":"resync-catalog"}`, or the UI's Catalog page. |
| Inspect a decision | The UI's work item, then the decision's "why" drawer. |
| Stop an agent | The owner presses Ctrl-C (or sends SIGTERM) to drain; a second one aborts the running task. SIGHUP re-reads `lending` only. There is no `factory-agent pause` command; to stop lending, change `lending` and send SIGHUP, or stop the worker. |
| Dispatcher logs | `deploy/poc/logs/factory-serve-<UTC time>.log`, JSON. Follow the latest with `tail -f "$(ls -t deploy/poc/logs/factory-serve-*.log \| head -1)" \| jq -c '{time, level, msg}'`. |
| Agent logs | the worker's stderr (§8 step 10). Per-task workspaces are under `~/.factory-agent/work/<task>/`, removed after each run unless `--keep-workspace`. A failed stack up leaves make's output in `<state dir>/logs/stack-*.log` (state dir default `~/.factory-agent`; the newest 20 are kept), and the run's summary names the file. |
| Transcripts | `GET /api/v1/runs/<run_id>/transcript` (gzip JSONL), or the UI's run page. They are kept across restarts, as files under `<state dir>/transcripts/` (§6, State). |

## 11. Stopping

1. Pause globally (§10), so nothing new is dispatched.
2. Stop the workers with Ctrl-C: they drain.
3. Stop the dispatcher with Ctrl-C in the `make poc-up` terminal. It shuts down gracefully (`--shutdown-timeout`, 30 s by default), and the log ends with `shutdown complete`.
4. Stop ngrok.

A dispatcher restart keeps its state (§6, State): agents stay registered and running tasks carry on. Started with an empty state dir, it keeps nothing but what GitHub holds: work items come back from their markers, and agents must register again with a new device login.
