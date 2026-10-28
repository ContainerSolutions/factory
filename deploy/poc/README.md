# PoC run kit

How a maintainer goes from nothing to a running dispatcher and a registered agent for the PoC
([rollout.md](../../design/workstreams/poc-rollout/LLD/rollout.md)). The dispatcher
(`factory serve`) runs on the operator's machine behind an ngrok tunnel. Agents
(`factory-agent supervise`) run on their owners' machines. One dispatcher serves both environments:
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
| Enable Device Flow | no: agents are enrolled (§8) | no |
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
- **`FACTORY_OAUTH_CLIENT_ID` / `_SECRET`**: ccf-factory's client ID and client secret.
- **The two `…_PRIVATE_KEY`s**: the PEM files. `run.sh` sources `.env` with bash, so either paste the PEM inside double quotes or read it from a file outside the repo: `FACTORY_SECRET_CCF_FACTORY_APP_PRIVATE_KEY="$(cat /path/to/ccf-factory.pem)"`.
- **`FACTORY_PUBLIC_URL`**: the tunnel URL, `https://<name>.ngrok-free.dev`. It isn't a secret, but it's kept out of the public repo.

`run.sh` refuses a `.env` that is missing, not yours, or not mode 600, and names (never prints) any variable that's unset. `cmd/factory/deploykit_test.go` keeps `.env.example` in step with the catalog's secret references.

Agents don't use the join token any more: an admin enrolls each agent host (§8). `FACTORY_SECRET_LOCAL_POOL_JOIN_TOKEN` stays in `.env` while the catalog's Pool still names it.

## 4. Catalog

`factory serve` reads the catalog from git by default: `catalog/` of `https://github.com/ContainerSolutions/factory` at `main`, polled every minute. `run.sh` passes that (`--catalog-repo … --catalog-ref "${FACTORY_CATALOG_REF:-main}" --catalog-path catalog`), or `--catalog-dir "$FACTORY_CATALOG_DIR"` when that is set in `.env`.

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

The dispatcher listens on **8800**, not 8080 or 8000: local-dev's CCF stack binds those (its API and UI). `make poc-up POC_ADDR=127.0.0.1:<port>` picks another; if your `.env` sets `FACTORY_ADDR`, that wins, so update it too. The tunnel reaches only the public listener on port 8800: the UI, `/api/v1/*` and `/hooks/*`. The ops routes are on a separate loopback-only listener, and the policy denies their paths too. Set ccf-factory's webhook URL to `$FACTORY_PUBLIC_URL/hooks/github-public` and its callback URL to `$FACTORY_PUBLIC_URL/api/v1/auth/callback`.

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

`state events` lists only the events the dispatcher stored. An event is stored when a rule could fire for it (its type in a rule's `on`, from a provider and an object its RuleSet scopes; `where` aside), when a work item links its object, or when the factory's own App caused it. Any other event is dropped when it arrives: it isn't in `state events`, and `factory events replay` answers 404 for it. Its delivery is deleted at once, so the way back, once a rule or link covers it, is to redeliver the webhook from the App's delivery log (§7, "Webhook"; GitHub keeps about 3 days). The redelivery is filtered again against the catalog as it is then.

An unknown id prints the ids nearest to it by prefix. Nothing it prints is secret (the store holds no credential, and transcripts are redacted by the agent), so its output can be pasted into an issue. `jq` on `snapshot.json` (one object per line) and `journal.jsonl` works too.

## 7. Check it

Log lines to look for:

- `catalog swapped in`, then `catalog loaded`;
- `provider connected … webhook=<public url>/hooks/github-public`;
- `provider health` without `degraded`. A degraded provider means a wrong App ID or key.

`catalog rejected; keeping the live catalog` means an invalid catalog: `factory validate --dir` the directory.

`event dropped: no work item links its object and no rule could fire for it` (Info) is where "why didn't my rule fire?" starts for an event that isn't in `factory state events` (§6). It names the event's `type`, its object's `ref` and a `reason`: `no-rule` (no rule's `on` has that type for the provider) or `out-of-scope` (a rule's does, but its RuleSet's `scope` doesn't hold the object: the most common misconfiguration). `factory_events_dropped_total{provider,type,reason}` on `/metrics` counts them. To get a dropped event back after fixing the catalog, redeliver it from Recent Deliveries (below).

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

For each agent owner, on their machine (rollout.md §Agent onboarding). The dispatcher's admin enrolls the host for its owner, who needs no GitHub login on it.

The agent is `factory-agent supervise`: it runs each leased task in its own VM workspace behind a gateway that holds every credential, a tart VM on an Apple-silicon Mac ([supervise.md](../../design/workstreams/agent/LLD/supervise.md)) or a Kata microVM on Linux ([workspace-kata.md](../../design/workstreams/agent/LLD/workspace-kata.md)). Run every step from a checkout of this repository, as the owner's own user (not root; the workspaces are that user's).

### The one-line install (from a release)

Without a checkout, `install.sh` (attached to every release, §12) does steps 1 to 4 from the release's binaries, and writes the whole `agent.yaml` itself:

```bash
curl -fsSL https://github.com/ContainerSolutions/factory/releases/latest/download/install.sh | bash
```

It asks for each setting, showing its default (Enter takes it), then shows the `agent.yaml` and asks before writing it. With `--quiet` it asks nothing and takes every default; only the dispatcher's URL and the enrollment token have none, so they must be given:

```bash
curl -fsSL https://github.com/ContainerSolutions/factory/releases/latest/download/install.sh | bash -s -- --quiet --dispatcher https://factory.example.dev --enrollment-token-file ~/token
```

Every setting is a flag (after `bash -s --`) or an environment variable, the flag winning; the default mode asks only for the ones neither gives. Lists are comma-separated. `--help` lists them.

| Flag | Environment | Default |
|---|---|---|
| `--dispatcher URL` | `FACTORY_DISPATCHER` | none: **required**. https, or http on a loopback host |
| `--enrollment-token-file PATH` | `FACTORY_ENROLLMENT_TOKEN` (the token itself) | none: **required**, unless the host is already enrolled or `~/.config/factory-agent/enrollment-token` exists; the default mode asks for it (not echoed) |
| `--pool NAME` | `FACTORY_POOL` | `local` |
| `--host NAME` | `FACTORY_HOST` | the machine's short host name, lower-cased |
| `--harnesses LIST` | `FACTORY_HARNESSES` | `codex,claude-code` (at least one of the two) |
| `--codex-models LIST`, `--codex-default MODEL`, `--codex-efforts LIST` | `FACTORY_CODEX_MODELS`, `_DEFAULT`, `_EFFORTS` | `gpt-6.1-sol`, `gpt-6.1-sol`, `low,medium,high` |
| `--claude-models LIST`, `--claude-default MODEL`, `--claude-efforts LIST` | `FACTORY_CLAUDE_MODELS`, `_DEFAULT`, `_EFFORTS` | `claude-sonnet-5-5,claude-opus-5-5`, `claude-sonnet-5-5`, `low,medium,high,max` |
| `--max-concurrent N` | `FACTORY_MAX_CONCURRENT` | `1` (`lending: {unlimited: true, maxConcurrent: N}`) |
| `--cpus N`, `--memory NGi`, `--disk NGi` | `FACTORY_CPUS`, `FACTORY_MEMORY`, `FACTORY_DISK` | `6`, `8Gi`, `60Gi` (each VM workspace's `workspace.size`: 1 to 16, 2Gi to 64Gi, 20Gi to 500Gi) |

The defaults are `agent.yaml.example`'s, and the TaskSpecs' models match them. Change the codex model if your codex sign-in doesn't offer it (see `/model` in codex). Efforts are from `minimal`, `low`, `medium`, `high`, `max`, and a default model must be one of the models. Every value is checked before anything changes; a bad one is an error naming its flag. No flag takes the enrollment token itself (it would show in `ps` and the shell history), and the token is never printed.

It runs as you, on an Apple-silicon Mac (tart) or a Linux x86-64 host (kata), and:

1. checks the settings, and works out the enrollment token: none when the agent's state dir already holds a refresh token (the host is enrolled) or `~/.config/factory-agent/enrollment-token` exists; else `--enrollment-token-file`'s single line, `FACTORY_ENROLLMENT_TOKEN`, or a prompt, after printing the host's name and the `factory agent enroll --pool <pool> --owner <your GitHub login> --name <host>` command an admin runs to mint it;
2. downloads `factory-agent-<os>-<arch>` (on a Mac also `factory-agent-linux-arm64`, the VMs' binary) of the latest release, or of `FACTORY_VERSION=vX.Y.Z`, and refuses them unless they match the release's `SHA256SUMS`;
3. installs them to `~/.local/bin` (`FACTORY_PREFIX`), mode 0755;
4. writes `~/.config/factory-agent/agent.yaml` (0600) and the enrollment token file (0600). **An existing `agent.yaml` is kept unless `--force`** (the default mode asks, default no; `--quiet` keeps it): replacing it first copies it to `agent.yaml.bak-<UTC time>` (0600). A kept one isn't rewritten, and the other steps still run;
5. runs `factory-agent host init --driver tart --agent-linux …` or `--driver kata` (`--yes` with `--quiet`), which asks for sudo itself;
6. offers `factory-agent login claude` (paste the `claude setup-token` token) and `factory-agent login codex` (after `codex login`), each skippable; `--quiet` skips both and prints the commands;
7. installs supervise as a launchd user agent (`~/Library/LaunchAgents/com.containersolutions.factory-agent.plist`) or a systemd user service (`~/.config/systemd/user/factory-agent.service`; `loginctl enable-linger` keeps it running while you are logged out), logging to `supervise.log` in the agent's state dir.

The other flags, which combine with both modes: `--dry-run` prints every step and command and changes nothing (it downloads nothing either); `--no-service` skips step 7; `--force` replaces an existing `agent.yaml`. (`--yes` is replaced by `--quiet`.) To undo it, it prints the commands at the end: `launchctl bootout gui/$UID/com.containersolutions.factory-agent` and remove the plist (Mac), or `systemctl --user disable --now factory-agent` and remove the unit (Linux); then remove the binaries from `~/.local/bin`, and `~/.config/factory-agent` and the state dir if you want them gone. `factory-agent host init`'s own changes stay ([workspace-host.md](../../design/workstreams/agent/LLD/workspace-host.md)).

From a checkout instead:

1. **The enrollment token.** An admin of the dispatcher mints one for your host, with the CLI token:

   ```bash
   bin/factory agent enroll --pool local --owner <your GitHub login> --name <host> --server "$FACTORY_PUBLIC_URL"
   ```

   It prints the token once (`--ttl`, default 7d, at most 30d; `bin/factory agent enrollments [list|delete <id>]` lists and revokes them). Put it in a file only you can read:

   ```bash
   mkdir -p ~/.config/factory-agent && (umask 077 && cat > ~/.config/factory-agent/enrollment-token)
   ```

   Paste the token, press Enter, then Ctrl-D. `<host>` must be the agent's `host` (default: the machine's short host name, lower-cased).
2. **The config.** Copy `deploy/poc/agent.yaml.example` to `~/.config/factory-agent/agent.yaml`, then `chmod 600` it. Edit:
   - `dispatcher`: `$FACTORY_PUBLIC_URL`, or `http://127.0.0.1:8800` on the dispatcher's own machine;
   - the harnesses and models your plans allow. Change the example's codex model (`gpt-6.1-sol`) if your codex sign-in doesn't offer it (see `/model` in codex): a `gpt-5*` or `gpt-6*` model, in `models` and `default`. A configured harness is offered when its login is stored (step 3);
   - `lending`;
   - `workspace`, whose driver defaults to tart on a Mac and kata on Linux.

   Unknown keys are errors.
3. **The host and the model logins:** a Mac as below, or a Linux host as in [A Linux workspace host](#a-linux-workspace-host-kata).
   - `make tart-up` builds `bin/factory-agent` and `bin/factory-agent-linux-arm64`, then runs `host init --driver tart`: one sudo prompt for Softnet and pf; the base and warm VMs take a while (`HOST_INIT_FLAGS=--dry-run` prints the plan without changing anything). Then `bin/factory-agent host check`: every step ok (pf may be skipped).
   - The model logins, which only the gateway ever holds, in the agent's state dir (0600): run `claude setup-token` on its own and copy the token it prints, then `pbpaste | bin/factory-agent login claude`; and `codex login` (with ChatGPT), then `bin/factory-agent login codex`, which imports `~/.codex/auth.json`. Don't copy another machine's `auth.json`: both machines' gateways would refresh the same login, and each refresh spends the other's refresh token.
4. **Run the agent:**

   ```bash
   bin/factory-agent supervise
   ```

   It refuses to start while `host check` fails, and destroys the VMs a killed supervise left. `--keep-workspace` keeps each stopped VM and its run directory for a look. There is no separate `register` command. On first start it exchanges the enrollment token (spending it) and registers as `agt_<owner>-<host>`, the owner the admin named; it shows on the UI's Pools page (or `GET /api/v1/agents`).

   The agent token lives in memory; the refresh token it got is kept 0600 in the state dir, and every restart (or a refused agent token) re-registers with it, rotating it, with no login. An admin's deregistration (`DELETE /api/v1/agents/<id>`) revokes it: the agent then stops and says to enroll again. If supervise says its refresh or enrollment token was refused, mint a new enrollment, put it in `enrollment-token` and restart.

   It logs to stderr, as text by default (`--log-format json`, `--log-level debug`). It writes no log file, so redirect it yourself, for example `bin/factory-agent supervise 2>&1 | tee -a ~/factory-agent.log`.

### A Linux workspace host (kata)

On an Ubuntu x86-64 machine (KVM, cgroup v2, systemd, at least 120 GiB free on `/var/lib`) each VM is a Kata microVM, and several run at once ([workspace-kata.md](../../design/workstreams/agent/LLD/workspace-kata.md)). Docker, if installed, is left alone: the factory runs its own containerd. In place of step 3:

1. `make kata-up` builds `bin/factory-agent` and runs `bin/factory-agent host init --driver kata`: it asks before installing its root helper (`/usr/local/libexec/factory-agent/kata-helper`, with a sudoers rule that lets you run only it with `sudo -n`), then prompts for sudo once. `HOST_INIT_FLAGS=--dry-run` prints the plan and the sudo script without changing anything. Then `bin/factory-agent host check`: every step ok.
2. The model logins, which only the gateway ever holds. Both go to `~/.local/state/factory-agent/logins/` (0600).
   - claude: run `claude setup-token` on its own and copy its output, then `bin/factory-agent login claude`: it asks for the paste; paste, then press Enter and Ctrl-D (the token stays out of argv and the shell history). Where a clipboard tool is installed, `xclip -o -selection clipboard | bin/factory-agent login claude` or `wl-paste | bin/factory-agent login claude` does the same.
   - codex: install the codex CLI on this host ([github.com/openai/codex](https://github.com/openai/codex)), run `codex login` (with ChatGPT), then `bin/factory-agent login codex`, which imports `~/.codex/auth.json`. Don't copy another machine's `auth.json`: both machines' gateways would refresh the same login, and each refresh spends the other's refresh token. A host without a codex login runs without codex: supervise drops the harness with a warning (and exits when no configured harness has a login).
3. In the config (step 2), `workspace: {driver: kata}` (the default on Linux) and the `lending.maxConcurrent` you want: supervise caps it by memory, `(physical memory − 4 GiB) / workspace.size.memory`, and by 63, with a warning.

Then step 4. `sudo -n /usr/local/libexec/factory-agent/kata-helper list` shows the running VMs. The first run on a new host follows the checklist in [workspace-kata.md §The owner's run](../../design/workstreams/agent/LLD/workspace-kata.md#the-owners-run).

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
| Stop an agent | The owner presses Ctrl-C (or sends SIGTERM) to drain; a second one aborts the running task. SIGHUP re-reads `lending` only. There is no `factory-agent pause` command; to stop lending, change `lending` and send SIGHUP, or stop the agent. |
| Dispatcher logs | `deploy/poc/logs/factory-serve-<UTC time>.log`, JSON. Follow the latest with `tail -f "$(ls -t deploy/poc/logs/factory-serve-*.log \| head -1)" \| jq -c '{time, level, msg}'`. |
| Agent logs | supervise's stderr (§8 step 4). Each task's run directory is under `<agent state dir>/runs/<task>/`, removed after each run unless `--keep-workspace`. |
| Transcripts | `GET /api/v1/runs/<run_id>/transcript` (gzip JSONL), or the UI's run page. They are kept across restarts, as files under `<state dir>/transcripts/` (§6, State). |

## 11. Stopping

1. Pause globally (§10), so nothing new is dispatched.
2. Stop the agents with Ctrl-C: they drain.
3. Stop the dispatcher with Ctrl-C in the `make poc-up` terminal. It shuts down gracefully (`--shutdown-timeout`, 30 s by default), and the log ends with `shutdown complete`.
4. Stop ngrok.

A dispatcher restart keeps its state (§6, State): agents stay registered and running tasks carry on. Started with an empty state dir, it keeps nothing but what GitHub holds: work items come back from their markers, and agents must be enrolled again: their refresh tokens are refused (supervise then drops its own and says so), so put a new token from `bin/factory agent enroll` in `enrollment-token` and restart.

## 12. Releasing

Nothing releases on its own: no bot tags, bumps versions or drafts releases. A person releases by hand:

1. On GitHub, create a release (Releases → Draft a new release) with a new tag `vX.Y.Z` on the commit to release (`main`, usually), and publish it. The tag must be `vX.Y.Z` or `vX.Y.Z-<suffix>`; any other fails the workflows.
2. Publishing starts each `release-*.yml` workflow in `.github/workflows/`. Each is standalone, runs only on `release: published` and builds from the release's tag:

| Workflow | What it adds to the release |
|---|---|
| `release-binaries.yml` | the assets `make release-assets VERSION=<tag>` builds into `dist/`: `factory-<os>-<arch>` (the dispatcher and CLI, UI embedded) for linux/amd64, linux/arm64 and darwin/arm64; `factory-agent-<os>-<arch>` for darwin/arm64, linux/amd64 and linux/arm64 (linux/arm64 is also the Mac's VM binary, `host init --agent-linux`); `install.sh`, the agent host installer (§8); `SHA256SUMS` over them. |
| `release-image.yml` | pushes the dispatcher image, the root `Dockerfile` (`factory` with the UI, on Alpine with `git` and the CA roots, as uid 65532), to `ghcr.io/containersolutions/factory:<tag>` and `:latest`, for linux/amd64 and linux/arm64. `make image` builds it locally (`IMAGE`, default `factory:dev`) and never pushes. |

A workflow that fails can be re-run from its Actions page: the upload replaces the release's assets (`--clobber`). `make release-assets VERSION=v0.0.0-test` builds the same `dist/` locally (it needs Node, like `make build-release`); `cd dist && shasum -a 256 -c SHA256SUMS` checks it.

### Run the image

The image's entrypoint is `factory` and its default command `serve`; give `serve`'s flags after the image name, as `run.sh` does (§6). It holds no secret: every `FACTORY_*` variable of §3 comes from the environment at run time. Pass them by name from a shell that loaded `.env` (a PEM spans lines, which `docker run --env-file` can't carry). The state dir (§6, State) belongs on a volume at `/var/lib/factory`, owned by uid 65532 (a named volume is; a host directory must be `chown 65532` and mode 0700):

```bash
set -a; . deploy/poc/.env; set +a
docker run --rm -p 127.0.0.1:8800:8800 -v factory-state:/var/lib/factory \
  -e FACTORY_PUBLIC_URL -e FACTORY_OAUTH_CLIENT_ID -e FACTORY_OAUTH_CLIENT_SECRET \
  -e FACTORY_SESSION_KEY -e FACTORY_CLI_TOKEN \
  -e FACTORY_SECRET_GITHUB_PUBLIC_WEBHOOK_SECRET -e FACTORY_SECRET_CCF_FACTORY_APP_PRIVATE_KEY \
  -e FACTORY_SECRET_CCF_FACTORY_REVIEWER_APP_PRIVATE_KEY -e FACTORY_SECRET_LOCAL_POOL_JOIN_TOKEN \
  ghcr.io/containersolutions/factory:<tag> \
  serve --state-dir /var/lib/factory/state --log-format json
```

`--addr` defaults to `:8800`, the port the image exposes. The catalog defaults to `catalog/` of this repository at `main` (`--catalog-repo`, `--catalog-ref`, `--catalog-path`); `--catalog-dir` needs the directory mounted. The ops listener (`--ops-addr`, `/healthz`, `/readyz`, `/metrics`) must stay on loopback, so it is reachable only from inside the container.
