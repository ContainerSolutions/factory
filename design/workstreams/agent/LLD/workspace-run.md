# LLD — Workspace runtime: `factory-agent run` (AGT)

> Workstream: [AGT](../../definitions.md#agt-agent) · Tasks: P1 phase 3 · Packages: `internal/agent/runbundle`, `internal/agent/vmrun`, `internal/agent/envrun`, `internal/agent/pipeline`, `internal/agent/harness/{claudecode,codex}`, `internal/agent/effector`, `cmd/factory-agent`
> Design: [P1 §2](../../../proposals/P1-agent-workspaces.md#2-the-workspace-and-factory-agent-run), [§3](../../../proposals/P1-agent-workspaces.md#3-the-gateway-every-credential-outside-the-vm), [§4](../../../proposals/P1-agent-workspaces.md#4-the-environment-is-declared-in-the-taskspec), [§5](../../../proposals/P1-agent-workspaces.md#5-credentials-injected-by-the-gateway-never-in-the-workspace) · Phase 0: [Q4 (claude)](../../../proposals/P1-phase0-report.md#q4-does-claude-code-work-behind-a-tls-intercepting-gateway-with-a-placeholder-token), [Q5 (codex)](../../../proposals/P1-phase0-report.md#q5-can-the-gateway-carry-codexs-subscription-auth) · Base image: [workspace-host.md](workspace-host.md#the-vm-steps-p1-2)

`factory-agent run` is the runtime inside a VM workspace. It reads the **run bundle** the supervisor wrote, builds a `runner.Job` from it with every token empty, and runs the existing `Pipeline.Run` with different sources: the bundle for the prompt, context, schema and skills; git through the gateway, with no credential helper; an environment runner in place of the local stack; and a file in place of the transcript upload. The host runtime (`factory-agent worker`) keeps its behaviour byte for byte: every new `pipeline.Options` field's zero value is today's behaviour, and every existing pipeline, harness and worker test passes unchanged.

## Refinements of P1

- **No run token in the bundle.** P1 §2 lists "a run token: an opaque per-run identifier". The bundle carries the dispatcher's run id (`run_…`) instead, and no token of any kind: the gateway knows which task its workspace runs, since there is one gateway per workspace (Decision 7), so the VM needs no identifier to present.
- **No gateway URL in the bundle.** On tart the gateway's address is known only after the driver's `Start` (the VM's default route), so it's the exec-time flag `--proxy`. The bundle has only the gateway's CA.
- **`out/outcome.json`, not `out/result.json`**, is what the supervisor collects: the whole `runner.Outcome` (status, reason, throttle time, Result, transcript file), since a run can end without a Result.
- **The run's user is the base image's `admin`**, not root ([below](#the-runs-user)).

## The run bundle: `internal/agent/runbundle`

A new package that imports only `internal/apis/v1alpha1`, `internal/apiclient` (types only) and the standard library. It has no network code.

### Layout

```
/run/factory/
  bundle.json          runbundle.Manifest
  task.json            the v1alpha1.Task from the lease (never the Lease: no credential)
  prompt.md            GET /tasks/{id}/prompt, byte-exact (the VM re-checks spec.prompt.digest)
  context/<n>.json     apiclient.TaskContext for each non-repo context item that answered 200
  output-schema.json   GET /tasks/{id}/output-schema; absent when it answered 404
  skills/<plugin>/…    the materialised plugin dirs, as materialise() lays them out today
  gateway/ca.pem       the agent host's gateway CA (the public half; Decision 9)
  codex/account.json   runbundle.CodexAccount, only when the task's harness is codex
```

The VM's outputs are under the workspace root's `out/`: `/work/out/{outcome.json,transcript.jsonl.gz,environment.log}` (`runbundle.OutcomeFile`, `TranscriptFile`, `EnvironmentLog`).

### Types

```go
const Version = 1
const DefaultDispatcherURL = "http://dispatcher.factory.internal" // served by the gateway (phase 4)

type Manifest struct {
    Version    int
    RunID      string    // run_…; never a token
    LeaseID    string
    TaskID     string
    StartedAt  time.Time // the lease's start, for the record; limits use --remaining
    Dispatcher string    // FACTORY_API in the VM; default DefaultDispatcherURL
    Gateway    Gateway   // {CAFile string; NoProxy []string}
    Git        GitAccess // no token field
    Prompt       string        // "prompt.md"
    Context      []ContextFile // {Index int; File string; Gone bool}
    OutputSchema string        // "output-schema.json", or "" (404)
    Skills       []Skill       // {Name, Plugin, Dir}: harness.Skill, Dir relative to the bundle
    Codex        *CodexAccount // {AccountID, UserID, PlanType}; nil unless the harness is codex
}
type GitAccess struct {
    Provider, Host, Repo string
    Permissions map[string]string // the credential's permissions (contents: read|write, …)
    Reads []GitRead               // {Repo string; ReadRepos []string}: the read credentials' scopes
}
type Files struct { Task v1alpha1.Task; Prompt []byte; Context map[int][]byte; OutputSchema []byte; SkillsDir string; CA []byte; Codex *CodexAccount }
type Bundle struct { Dir string; Manifest Manifest; Task v1alpha1.Task; Prompt []byte; Context map[int]apiclient.TaskContext; OutputSchema []byte /* nil: 404 */; CAFile string }

func Write(dir string, m Manifest, f Files) error // atomic: writes dir + ".tmp", then renames it to dir
func Read(dir string) (*Bundle, error)            // Validates; caps each file at the pipeline's bound for it
func (m Manifest) Validate() error
```

### Rules

- **No token field anywhere.** `GitAccess` is the lease's `gitCredential` and `readCredentials` without their `token`; `task.json` is the Task, which by contract carries no credential (the lease carries them beside it). `CodexAccount` holds identifiers, not credentials (P1 §5). A test walks every exported type of the package with reflection and fails on a field named `*Token*`, `*Secret*`, `*Key*` or `*Password*`. At run time `Write` and `Read` scan every string of the manifest and `task.json` for token shapes and refuse a match (naming the field, never the value); the scan covers the external type (`v1alpha1.Task`) the name walk stops at. A match refuses the bundle, so the shapes are the real tokens', between non-identifier boundaries, and a repo `acme/sk-platform` or a host `sk-registry.example.com` passes: GitHub's `gh[opsur]_` (36+ alphanumerics) and `github_pat_` (82+), Anthropic's `sk-ant-` and OpenAI's `sk-proj-`/`sk-svcacct-`/`sk-admin-` keys (80+) and legacy `sk-` (48), the factory's `rtk_`, `enr_` and `arf_` (exactly 43 base64url characters) and `agt_<id>.<43>`, a JWT, a `Bearer`/`Basic` credential and a private key block. A context file's content is not shape-scanned: it is untrusted provider text, where a token-shaped example in an issue body is no credential of the run's, and refusing the bundle over it would only make runs fail. `PrepareBundle` checks every byte it writes for the job's real tokens instead ([below](#pipelinepreparebundle-host-side)).
- **`Write`** refuses an existing `dir` (the supervisor writes each run's bundle once), removes a leftover `dir.tmp`, writes everything into `dir.tmp` (directories 0700, files 0600, a skill's executable files 0700, as the pipeline's `materialise` does), fsyncs the files, then renames `dir.tmp` to `dir`. A failure removes `dir.tmp`. The driver's `Put` copies the tree to `/run/factory`, owned by root and read-only to everyone (files 0444, directories and executable files 0555), so the run's user, `admin`, can read it and not change it (phase 5).
- **`Read`** opens the bundle through an `os.Root`, so no manifest path leaves it, reads `bundle.json`, `task.json`, the prompt, the context files and the schema, each capped at the bound the pipeline applies to it on the host (the prompt 1 MiB, `maxPrompt`; a context item 4 MiB, `maxContext`; the schema 1 MiB, `maxSchema`; `bundle.json` and `task.json` 1 MiB), and runs `Validate`. `Write` refuses the same sizes, so an oversize item fails on the host before a VM exists, never in it. It doesn't read the skills: they were bounded when they were materialised (run-pipeline.md §Skills: 50 MB and 5,000 entries per plugin).
- **`Validate`** checks: `Version == 1`; `RunID` starts with `run_` (a `rtk_` value is refused by name: it's the run token's prefix); `TaskID` and `LeaseID` are set and `TaskID` is `task.json`'s; `Dispatcher` is an absolute `http`/`https` URL without user info, query or fragment; every file path is relative, clean and without `..`; each `Context` index is a non-repo item of the Task's `spec.context`, listed once, with a `File` exactly when it isn't `Gone`; each `Skills` entry's `Dir` is under `skills/` and names one plugin; `Gateway.NoProxy` entries are hosts (`v1alpha1.ValidateHost`) or CIDRs; `Codex` is set exactly when `spec.harness.resolved.name` is `codex`, with all three fields non-empty. Every failure is a `*runbundle.ValidationError{Field, Message}`.
- **The outcome** (`outcome.go`): the wire `Outcome{Status, Reason, ThrottledUntil, Result, Transcript}`, where `Transcript` is the transcript's file name under `out/` (or empty). `Validate` checks `Status` and `Reason` against `runner.Statuses()` and `runner.Reasons()` (the one list of each), and allows `ThrottledUntil` only on a throttled outcome. `WriteOutcome` writes through a temp file and a rename. `ReadOutcome(outDir, now)` refuses an `outDir` that isn't a real directory (a symlink included) and an `outcome.json` that isn't a regular file with one link (opened `O_NONBLOCK`, then fstat'd, so a FIFO can't block it), caps it at 2 MiB (`MaxOutcomeBytes`), validates it and clamps its throttle (`ClampThrottle`; [trust model](#trust-model)).
- **The events** (`events.go`): `Event{Type, At, Start, Step, Progress, Usage, Outcome}`, one pointer per type; `Encoder` (one JSON line per event, at most 64 KiB, `MaxEventBytes`; serialised by a mutex, since the Sink is called from several goroutines) and `Decoder` (refuses a line over the cap, skips unknown types, so a newer VM can talk to an older supervisor, and refuses negative or non-finite usage, as the dispatcher does). `StepOf` cuts a step's error at 8 KiB (`MaxStepError`) on a UTF-8 boundary, so the step event still fits its line. A decoding error names the field and the JSON kind, never the value.

## `factory-agent run`

`cmd/factory-agent/run.go` (one line in `main.go`) calls `vmrun.Main(ctx, args, stdout, stderr) int`.

```
factory-agent run --proxy http://<gateway>:<port> [--bundle /run/factory] [--work /work]
  [--state-dir /var/lib/factory-agent/run] [--remaining 54m10s] [--system-setup on|off]
  [--down-timeout 3m] [--log-level info] [--log-format json|text] [--harness-bin codex=/path (hidden)]
```

- **`--proxy`** is required: an absolute `http` URL with a host and port and nothing else.
- **`--work`** is the workspace root (`pipeline.Options.Root`); it must be empty or absent. **`--state-dir`** holds what the runtime writes outside the workspace: the environment's scripts, the docker client config, the combined CA bundle. When either is absent and its parent isn't writable by the run's user, system setup creates it with `sudo -n install -d -o <uid> -g <gid> -m 0700`; with `--system-setup off` the run's user must be able to create it (tests pass temp dirs).
- **`--remaining`** is how much of the task's wall clock is left, measured by the supervisor (no clock skew between the agent host and the VM). Absent: the whole `limits.maxWallClock`. An explicit `--remaining` that is zero or negative is a usage error (exit 2) naming the flag: a supervisor with no time left doesn't launch the run, and a computed `0` never becomes a whole wall clock (decided 2026-10-22, PR #115's review).
- **`--system-setup`**: `on` runs the privileged part of [system setup](#system-setup-vmrunsystemgo); `off`, the default, only builds the environment (a plain container, a test). There is no automatic mode (decided 2026-10-22, PR #114's review): `sudo -n true` also succeeds on a workstation within sudo's timestamp or a CI runner with passwordless sudo, where a guessed `on` would put the gateway's intercepting CA in the system store and restart dockerd. The phase 5 supervisor, which knows it launched a workspace VM, always passes `on`; `--system-setup auto` is a usage error that says so.
- **`--harness-bin name=path`** (hidden, repeatable) overrides a harness binary; tests use it for the fake harnesses.
- **stdout is the event stream; stderr is slog** (`--log-format json` by default, since the supervisor captures it).

### Sequence (`vmrun.Main`)

1. Parse the flags (exit 2 on a usage error).
2. `runbundle.Read(--bundle)`; a failure is exit 3, with the error on stderr, no events and no outcome. Then send `start {version, runId, taskId}`.
3. `Setup(ctx, manifest, proxy, mode)` ([system setup](#system-setup-vmrunsystemgo)). A failure is the outcome `failed{environment-error}` (exit 0): the environment can't be reached without it. The pipeline hasn't run, so vmrun writes that outcome itself, creating `<work>/out/`.
4. `JobFromBundle(b, remaining)` (`job.go`): `Lease` rebuilt from the manifest and `task.json`, with `Git` a `runner.Credentials` holding `GitAccess`'s metadata and empty tokens, `API` nil, `AgentToken` nil, `Dispatcher` the manifest's, and `StartedAt = now + remaining − wall`, where `wall` is the task's `maxWallClock` (else `pipeline.DefaultWallClock`) and `remaining` is capped at `wall`. The pipeline's deadline, `StartedAt + wall`, is then `now + remaining`.
5. `adapters(b)` (`adapters.go`) builds only the task's harness, with `Isolation: harness.IsolationVM` and a `config.Harness` made from the task's resolved harness (no owner config dir or home: the VM has none).
6. `pipeline.New` with the workspace options ([below](#pipeline-changes-internalagentpipeline)), then `Run(ctx, job, sink)`, where `sink` (`events.go`) writes `progress` and `usage` events.
7. `Finished` writes `out/outcome.json` and sends `outcome`; the environment's `down` runs after it, in the pipeline's cleanup. When `Run` returns an error instead of an Outcome (it does so before any step, for an invalid `maxWallClock`), `Finished` isn't called and vmrun writes `failed{harness-error}` itself, as in step 3.

### Event stream

One `runbundle.Event` per line on stdout:

| Type | Payload | When |
|---|---|---|
| `start` | `{version, runId, taskId}` | first, once the bundle is read (an invalid bundle gets no events) |
| `step` | `{name, state: ok\|error, ms, error?}` | each `OnStep` call: the pipeline's steps and envrun's sub-steps |
| `progress` | a `v1alpha1.ProgressEvent` | each `Sink.Progress` |
| `usage` | `runner.UsageDelta` in `apiclient.UsageDelta`'s wire shape | each `Sink.Usage` |
| `outcome` | `runbundle.Outcome` | once, from `Finished` |

**The outcome comes before `down`.** `outcome.json`, the transcript file (when there is one: as on the host, a run that was cancelled or failed before its harness started has none) and the `outcome` event are all written before the environment's `down` runs, so a slow or hung `down` never costs the run its outcome. After the `outcome` event the files under `out/` are final, apart from `environment.log`, which `down` still appends to; the supervisor may `Get` them at once and destroy the VM after its own down budget. Only `step` events for `environment.down` can follow `outcome`.

### Exit codes and signals

- **0:** an outcome was written, whatever its status (`failed` included). **2:** usage. **3:** the bundle is invalid. **4:** the outcome couldn't be written (the supervisor then has only the events).
- **The first SIGTERM** (or SIGINT) cancels the run's context: the pipeline ends `failed{cancelled}`, the outcome is written, and `down` runs with `min(--down-timeout, 30s)`. **A second SIGTERM** skips `down`, or cancels a running one, and `run` exits. The supervisor SIGKILLs after its grace period and destroys the VM anyway.
- `vmrun.downGuard` wraps the `pipeline.Environment` to apply this: its `Down` derives its context from the current budget when it's called.

### The run's user

The run runs as the base image's `admin` user (decided 2026-10-22) (`WS_USER` in `internal/agent/host/provision/base.sh`; [workspace-host.md](workspace-host.md#the-vm-steps-p1-2)), as `tart exec` gives it. It's root whenever it needs to be: `admin` has passwordless sudo, so the LLM can `sudo apt-get install` (Decision 5), and system setup uses `sudo -n`. `admin` is in the `docker` group, and gh's pinned extensions are installed for it. It also avoids running claude as root ([risk 2](#risks-and-open-questions)), with a user the base already has rather than a new one. A run as root (a plain-container test) still works: it then sets `IS_SANDBOX=1` for claude, and doesn't use `sudo`.

## Pipeline changes (`internal/agent/pipeline`)

### New options

Every zero value is today's host behaviour.

```go
Root        string            // the exact workspace root (VM: /work); not with WorkRoot; must be empty or absent; never removed
GitAuth     GitAuth           // GitAuthHelper (zero: the gitcred socket) | GitAuthGateway (no socket, no helper config)
Env         map[string]string // added to commonEnv: proxies, CA variables, the GH_TOKEN placeholder, DOCKER_CONFIG, …
Prompts     PromptSource      // default APIPrompts; VM: BundlePrompts{B}
Schemas     SchemaSource      // exists; VM: BundleSchemas{B}
Skills      SkillSource       // default FetchSkills (today's skillsTree, fetchTree and materialise); VM: PrebuiltSkills{Dir, List}
Environment Environment       // the environment runner (P1 §4); nil on the host
Transcripts TranscriptStore   // default APITranscripts; VM: FileTranscript{Path}
Finished    func(runner.Outcome)                         // at the end of finish(), before cleanup
OnStep      func(name string, d time.Duration, err error) // after each step
```

`New` refuses `Root` together with `WorkRoot` (exactly one is required), a relative `Root`, and an `Env` key the pipeline sets itself (`HOME`, `TMPDIR`, `XDG_CONFIG_HOME`, `XDG_CACHE_HOME`, `PATH`, the `GIT_*` and `GCM_*` names `commonEnv` sets, and every `FACTORY_*` name `buildEnv` sets, among them every `FACTORY_STACK_*` one), so a collision is an error instead of a silently lost value. It also refuses an `Env` key that could add a credential helper or change what git, the loader or the shell runs: every `GIT_*` name but `GIT_SSL_CAINFO` and `GIT_SSL_CAPATH` (so `GIT_CONFIG_PARAMETERS`, `GIT_ASKPASS`, `GIT_SSH_COMMAND`, `GIT_PROXY_COMMAND`, `GIT_DIR`, `GIT_WORK_TREE`, `GIT_EXEC_PATH`, …), `SSH_ASKPASS`, `LD_*`, `DYLD_*`, `BASH_ENV` and `ENV`: gateway mode's "no helper at all" and the host's "only the in-memory helper" rest on it. The error names the variable, never its value. Every name of [system setup](#system-setup-vmrunsystemgo)'s table is accepted. A run whose `Job.API` is nil refuses to start while any source is an API one, naming it, instead of failing on a nil client mid-run.

### Sources (`sources.go`)

```go
type PromptSource interface {
    Prompt(ctx context.Context, job *runner.Job) ([]byte, error)
    Context(ctx context.Context, job *runner.Job, i int) (c apiclient.TaskContext, gone bool, err error)
}
type SkillSource interface {
    Skills(ctx context.Context, job *runner.Job, dst string) (dir string, list []harness.Skill, err error)
}
type TranscriptStore interface {
    Store(ctx context.Context, job *runner.Job, gz []byte) (ref string, err error)
}
```

- `renderPrompt` keeps the digest check against `spec.prompt.digest`, the preamble, the fencing and the tickets; only the two GETs move behind `PromptSource`. `APIPrompts` is today's code; `BundlePrompts` serves the bundle's files (a `Gone` item is the 404 case).
- `FetchSkills` is today's `skillsTree`/`fetchTree`/`materialise`, moved behind the interface with the cache and its per-entry lock (the `skillFetcher` extraction). The fetch needs the run's git (its credential helper, its environment), so the pipeline runs it itself: `materialiseSkills` recognises `FetchSkills` (a value or a pointer) and runs the `skillFetcher`, and `FetchSkills.Skills` called on its own is an error. `PrebuiltSkills` returns the bundle's `skills/` and the manifest's list as they are; the workspace's `Skills` is then `/run/factory/skills`, read-only, which both adapters only read (codex copies the skill folders into its `CODEX_HOME`).
- `buildTranscript`'s redaction and 20 MB cap stay shared; only the upload moves behind `TranscriptStore`. `APITranscripts` is today's `PUT /runs/{id}/transcript`; `FileTranscript` writes the gzip to its path (atomically) and returns an empty ref, since the supervisor uploads. The VM's redactor knows no run token (it has none), so only the patterns apply; the supervisor redacts again with the real ones ([trust model](#trust-model)).

### Gateway mode

With `GitAuth: GitAuthGateway`:

- `startCredentials` doesn't call `gitcred.Listen`, and `commonEnv` sets neither `FACTORY_GIT_CREDENTIAL_SOCKET` nor the helper's `GIT_CONFIG_*` entries. No other helper can answer: `GIT_CONFIG_NOSYSTEM=1` and the scratch `GIT_CONFIG_GLOBAL` stay. git reaches GitHub through `HTTPS_PROXY`, and the gateway adds the credential for the repo in the path.
- `planMounts` is unchanged: it reads the credential's `Repo` and `Permissions.contents`, never its token, and the job's credential has an empty one.
- `buildEnv` already sets `FACTORY_TOKEN` only when `AgentToken != nil` and `GH_TOKEN` only from a non-empty token, so the VM's harness gets neither; `Options.Env`'s `GH_TOKEN` placeholder is what gh sees.
- **A job that carries a credential is refused (decided 2026-10-22).** Before any step, `Run` checks the job: a non-empty token in `Lease.GitCredential`, `Lease.ReadCredentials`, or `Git` (the current task and read tokens), a non-nil `AgentToken`, or a non-nil `API` (the client authenticated as the agent) ends the run `failed{harness-error}` with a `*pipeline.GatewayCredentialError{Field}` that names the field, never the value. The VM is untrusted and the bundle has no token field, so such a job is a supervisor bug that would otherwise hand the agent a token through `GH_TOKEN` or `FACTORY_TOKEN`. The host (`GitAuthHelper`) is unchanged.

`Options.Env` is applied in `commonEnv` after the owner's allow-list, so the pipeline's own git (the clones) gets the proxies and the CA bundle too.

### Root, Finished and OnStep

- **`Root`**: `makeWorkspace` lays the usual tree (`repo/`, `ro/`, `out/`, `home/`, `tmp/`, `bin/`; `skills/` only with `FetchSkills`, in either mode, since no other source fills it) directly in it, and cleanup never removes it, so `out/` survives for the supervisor (the VM is destroyed anyway). An absent `Root` is created 0700 (its parent must exist); an existing one must be an empty directory, not a symlink, and keeps its mode. "A leftover workspace of the same task is replaced" doesn't apply: a non-empty `Root` fails the `workspace` step. `PrepareBundle` refuses a pipeline with `Root` or `GitAuthGateway`: the bundle is made on the host.
- **`Finished`** is called once at the end of `finish()`, after the transcript is stored and before cleanup (the environment's `Down`, the credential socket, the workspace removal). On the host it's nil.
- **`OnStep`** is called after each step with its name, its duration and its error, redacted with the run's redactor (its `Error()` is the redacted text; it unwraps to the step's error, so `errors.Is` still matches): `workspace`, `credentials`, `clone`, `stack`, `environment`, `skills`, `schema`, `prompt`, `env`, `prepare`, `harness`, `repair`, `finish`. Today's step list calls `buildEnv` `environment`; it becomes `env`, so the new step can take the name (its error prefix changes from `environment:` to `env:`). envrun reports its own sub-steps through the same callback: `environment.setup`, `environment.up`, `environment.ready`, `environment.down`.

## The environment step

### `pipeline/environment.go`

```go
type Environment interface {
    Up(ctx context.Context, ws harness.Workspace, t v1alpha1.Task) (map[string]string, error) // setup, up, ready
    Down(ctx context.Context) error                                                          // best effort
}
const ReasonEnvironmentError = "environment-error" // in internal/agent/runner, beside the other reasons
```

- **When:** after `clone` and the host's `stack` step, before `skills`, when `task.Spec.Environment != nil || len(task.Spec.Versions) > 0` (the contract of [#100](https://github.com/compliance-framework/factory/pull/100): `spec.environment` copied from the TaskSpec's `workspace.environment`, and `spec.versions` resolved at compile time).
- **On the host** (`Environment == nil`) the step doesn't run the scripts: it logs a warning and adds a `factory.environment-skipped` line to the transcript, and the run goes on. Scripts written for a VM (root through sudo, Docker, ports) aren't run on the owner's machine.
- **In the VM** it calls `Up`; an error ends the run `failed{environment-error}` before the harness starts. The returned map (the `exposes` and the `FACTORY_VERSION_*` variables) is merged into the harness's environment by `buildEnv`, as `stackEnv` is today.
- **`Down`** runs in cleanup whenever `Up` was called, even if it failed part-way (an `up` that failed may have started containers), with `context.WithoutCancel(ctx)` and no deadline of its own: the Environment owns its budget (envrun's `DownTimeout`, which vmrun's `downGuard` shortens after a SIGTERM).

### `internal/agent/envrun`

`envrun.Runner` implements `pipeline.Environment`. `envrun.Options{StateDir, Env map[string]string, DownTimeout, Clock, Logger, OnStep, Redact func([]byte) []byte}`; `DownTimeout` defaults to `pipeline.StackDownTimeout` (3 min).

- **Scripts.** Each of `setup`, `up` and `down` that is non-empty is written to `<state>/env/<name>.sh` (0700) and run as `/bin/bash --noprofile --norc -eo pipefail <file>`, in its own process group, in `ws.Root`. Cancel sends SIGTERM to the group and SIGKILL 5 s later, as `clirun` does for the harnesses. **TaskSpec authors write for bash with `-e` and `pipefail`**: a failing command or pipeline stage stops the script.
- **Their environment** is `commonEnv` (the owner's allow-list, the scratch `HOME` and `TMPDIR`, git without a helper, and `Options.Env`'s proxies and CA variables) plus `FACTORY_TASK_ID`, `FACTORY_KIND_IMAGES` (when `/var/lib/factory/kind-images` holds an archive, below) and `FACTORY_VERSION_<COMPONENT>` for each `spec.versions` entry (the component upper-cased, `-` as `_`; `v1alpha1.EnvVersionPrefix`). The workspace's `bin/` isn't on their `PATH`: the `factory` CLI is the harness's, not the environment's. The scripts get the scratch `HOME`, not `/home/admin`.
- **Output** (stdout and stderr together) goes to `out/environment.log`, redacted line by line, capped at 10 MiB (then a `[truncated]` line); a failed script's error carries the last 8 KiB, and the Result's summary its first 500 bytes, as for every pipeline failure.
- **Ready.** Each `ready.http` URL is polled with a direct `http.Client` (`Transport.Proxy` nil: these are the VM's own `localhost` services, never the gateway's) every 2 s, each request bounded by 10 s, until it answers 2xx or 3xx (redirects aren't followed: a 3xx counts). Every URL must pass within `ready.timeout`, else 15 minutes (`DefaultReadyTimeout`), on the injected clock.
- **Exposes** are returned as they are: literal values, no expansion, names already checked by `v1alpha1` (`FACTORY_[A-Z0-9_]+`, not reserved, not `FACTORY_VERSION_*`).
- **Down** runs the `down` script once, bounded by `DownTimeout` and the context, whichever ends first (vmrun sets `DownTimeout` from `--down-timeout`, and `downGuard` only shortens it); its failure is logged and reported as the `environment.down` step's error, never as the run's outcome.
- **kind images.** The warm base saves `warm.kindImages` as archives under `/var/lib/factory/kind-images/` ([workspace-host.md](workspace-host.md#the-vm-steps-p1-2)). Only the environment's `up` knows when its kind cluster exists and when its rollouts start, so envrun doesn't run `kind`: it exports the directory as `FACTORY_KIND_IMAGES`, and the TaskSpec's `up` runs `kind load image-archive "$FACTORY_KIND_IMAGES"/*.tar --name <cluster>` between creating the cluster and deploying to it (Phase 0 Q6: the in-cluster agent's pull passed its rollout deadline).

## System setup (`vmrun/system.go`)

`Setup(ctx, m Manifest, proxy *url.URL, mode SystemSetup) (map[string]string, error)` returns the map that becomes `pipeline.Options.Env` and `envrun.Options.Env`:

| Variable | Value |
|---|---|
| `HTTPS_PROXY`, `HTTP_PROXY`, `https_proxy`, `http_proxy` | `--proxy` (both cases: curl ignores upper-case `HTTP_PROXY`, and some tools read only one case) |
| `NO_PROXY`, `no_proxy` | `localhost,127.0.0.1,::1,.localhost,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,.svc,.cluster.local`, then `m.Gateway.NoProxy` |
| `SSL_CERT_FILE`, `GIT_SSL_CAINFO`, `CURL_CA_BUNDLE`, `REQUESTS_CA_BUNDLE` | `<state>/ca-bundle.pem`: the system roots (`/etc/ssl/certs/ca-certificates.crt`) followed by the gateway CA (re-encoded, below) |
| `NODE_EXTRA_CA_CERTS` | the bundle's `gateway/ca.pem` (Node adds it to its own roots; Phase 0 Q4) |
| `GH_TOKEN` | `harness.Placeholder`: gh refuses to start without a token, and the gateway replaces it on api.github.com GETs |
| `FACTORY_RUNTIME` | `workspace` |
| `DOCKER_CONFIG` | `<state>/docker`, with a `config.json` that has **no** `proxies` key (decided 2026-10-22, risk 13): containers get no proxy variables, so calls between compose services by name stay direct. Containers therefore have no internet egress; image pulls still work through dockerd's own proxy below |
| `XDG_DATA_HOME` | the run user's own `~/.local/share` (from the passwd entry): the pipeline's scratch `HOME` would hide the gh extensions the base installed for `admin` |

`config.json` and the CA bundle are written before the environment's `setup` runs.

**The gateway CA** (`gateway/ca.pem`, at most `runbundle.MaxCABytes`) must be one or more PEM `CERTIFICATE` blocks with only whitespace around and between them: each block must decode from the start of what's left (`pem.Decode` otherwise skips a malformed block, such as a key whose type or END line is broken, and returns the certificate after it), carry no headers, parse with `x509.ParseCertificate` and be a CA (`BasicConstraintsValid` and `IsCA`). Anything else fails setup. What's written to the CA bundle and the system store is `pem.EncodeToMemory` of the accepted blocks, never the file's bytes.

With `--system-setup on`, in order and each with `sudo -n` (none as root):

1. install the gateway CA as `/usr/local/share/ca-certificates/factory-gateway.crt` and run `update-ca-certificates`, for tools that read only the system store;
2. read `/etc/docker/daemon.json` with `sudo -n cat` (a root-only file works; an absent one is `{}`, a file that isn't a regular file or is over 1 MiB fails) and write it with `proxies.{http-proxy, https-proxy, no-proxy}`, merged into the file's other keys;
3. `systemctl restart docker`, then poll `docker info` until it answers (60 s).

Both system files are replaced atomically: `install -m <mode>` to `.<name>.factory-tmp` in the target's directory (the mode of the file it replaces, `0644` when there's none), then `mv -f` over the target, so a run killed midway leaves the old file whole.

`off` does none of these: a plain container has no systemd or dockerd, and tests run unprivileged. Any failure in `on` mode fails the run `environment-error`.

## Harnesses under `IsolationVM`

`harness.Isolation` is a new adapter option (`claudecode.Options.Isolation`, `codex.Options.Isolation`): `IsolationSandbox` (zero, the host: the harness's own OS sandbox, as today) or `IsolationVM` (the VM is the boundary, P1 §2 step 3). `harness.Placeholder` is the one placeholder credential value (`factory-placeholder-not-a-credential`): it matches none of the redactor's token patterns, and the gateway replaces whatever value it finds, so it never has to recognise it.

### claude-code

- **Skipped:** `sandboxSupport` (Probe and Prepare), `checkConfigDir`, and Probe itself (the supervisor never probes inside the VM). `CLAUDE_CONFIG_DIR` is `<home>/.claude`, a scratch directory in the workspace's `HOME`.
- **Settings:** no `sandbox` block and no `permissions` block (`sandboxSettings` isn't called); `enabledPlugins: {}`, `effortLevel` and the PreToolUse hook stay. The hook keeps its `git push` and gh-write rules, so pushes still go through `factory push` (#83) and gh writes through the Effector, and it gets `--write /` in a writable workspace. A read-only workspace keeps `--disallowedTools Write,Edit,MultiEdit,NotebookEdit`.
- **Environment:** `CLAUDE_CODE_OAUTH_TOKEN=harness.Placeholder` on a subscription run, `ANTHROPIC_API_KEY=harness.Placeholder` on an API-key run (the gateway sets `x-api-key`); nothing is read from `Environ`, and `USER` isn't set (there's no Keychain). A task without a resolved auth runs on the subscription: the VM has no owner's environment or login to tell the mode from. `IS_SANDBOX=1` only when the run is root (**UNVERIFIED**: what claude needs for `bypassPermissions` as root; a constant in `flags.go`).

### codex

- **Skipped:** Probe, the `auth.json` symlink, `[sandbox_workspace_write]` and its `writable_roots`/`gitDirs` (and the `home/.cache` writable root), and `workDirFor`: the workdir is the pipeline's `WorkDir`, else the workspace root. `AGENTS.md` is the preamble and, for a read-only checkout, a line saying the task may not change it, since nothing in the VM stops a write (the gateway refuses the push).
- **`auth.json`** (0600, a new file in the scratch `CODEX_HOME`, never written over an existing one) is `placeholderAuth(account, now)`, in exactly the shape Phase 0's run used (Q5), with no key added or left out: `OPENAI_API_KEY` null; `tokens.id_token` an unsigned JWT (header `{"alg": "none", "typ": "JWT"}`) whose claims are `email` (`placeholder@example.invalid`), `exp` (`4102444800`, 2100-01-01) and the `https://api.openai.com/auth` claim with `chatgpt_account_id`, `chatgpt_user_id` and `chatgpt_plan_type`; `tokens.access_token` and `tokens.refresh_token` `harness.Placeholder`; `tokens.account_id` the account id; `last_refresh` the injected clock's now, so codex never refreshes. The id_token's signature segment is Phase 0's non-empty dummy (`x`), not empty: codex decodes the JWT without verifying it, but its decoder may refuse an empty segment (read from `codex-rs/login`, not run). The key names and the fixed values are constants in `flags.go` marked as Phase 0's, to be re-checked on the first VM workspace run (`make smoke-codex` runs only the host path); a golden file pins the bytes.
- **The account** is `codex.Options.Account` (a `*runbundle.CodexAccount`), which `vmrun`'s `adapters(b)` sets from the bundle's `codex/account.json`; the adapter reads no file to find it, and Prepare refuses a VM run without all three identifiers. Where the supervisor gets them for `BundleExtras.Codex` is phase 5's (the owner's login on the agent host carries them in its id_token's claims).
- **Refresh belongs to the gateway.** With `last_refresh` fresh, codex, which refreshes a login days old (read from `codex-rs/login`; Phase 0's run made no call to `auth.openai.com`), doesn't refresh during a run. Were it to try (a 401 because the gateway's token expired), it would post the placeholder refresh token to `auth.openai.com`, which refuses it: the run fails on auth, and nothing in the VM is a credential. The gateway refreshes the real token from the owner's login before it expires (phase 4).
- **Invocation:** `--sandbox danger-full-access` (`sandboxDangerFullAccess`). `[mcp_servers.factory]` keeps `default_tools_approval_mode = "approve"`; with full disk access and `approval_policy = "never"` codex wouldn't ask anyway (harness-adapters.md, gap 3).
- **An API-key run is refused** in the VM (Prepare): codex's `OPENAI_API_KEY` path has no gateway injection in the PoC.

## The Effector in the workspace runtime

- **No `FACTORY_TOKEN`:** the client sends no `Authorization` header, which already works; the gateway adds the run token for `dispatcher.factory.internal`. The default HTTP client honours `HTTP_PROXY` (`http.ProxyFromEnvironment`), and the dispatcher's name isn't in `NO_PROXY`. A 401's message becomes "the dispatcher refused the run's credentials" in both runtimes (it named the agent token).
- **Git-side ops** run git as today, minus the credential helper: `factory push` reaches github.com through `HTTPS_PROXY`, and the gateway refuses a push to a repo the task may not write (P1 §3).
- **`FACTORY_RUNTIME=workspace` forces the stack git fallback:** `useGHStack` returns false, as if `--git` were given, because gh-stack's GitHub API writes would be refused by the gateway. The op records `via: git`.
- **`FromEnv` fails closed on the runtime:** it accepts `FACTORY_RUNTIME` unset (the host) or `workspace`; any other value (a typo in the VM) is a usage error (exit 2) naming the variable and the accepted values, rather than the host path trying gh stack and failing at the gateway. In the workspace runtime a non-empty `FACTORY_TOKEN` is a usage error too, naming the variable and never its value: the workspace Effector holds no credential, so a token that reached the VM fails every effect instead of being sent. On the host a task's `exposes` setting `FACTORY_RUNTIME=workspace` therefore makes the Effector refuse to start whenever the agent token is set, which fails closed; reserving the name is risk 12's contract PR.

## `pipeline.PrepareBundle` (host side)

```go
type BundleExtras struct { CA []byte; Dispatcher string; Codex *runbundle.CodexAccount }
func (p *Pipeline) PrepareBundle(ctx context.Context, job *runner.Job, dir string, x BundleExtras) error
```

Phase 5's entry: the supervisor calls it for each lease, before it creates the VM.

- Fetches the prompt, each non-repo context item and the output schema through the pipeline's own `Prompts` and `Schemas` (on the host, the `API*` defaults), with the bounds and refusals of the prompt and `schema` steps: the prompt's digest, the prompt 1 MiB, a context item 4 MiB and the schema 1 MiB, which are `runbundle`'s `MaxPromptBytes`, `MaxContextBytes` and `MaxSchemaBytes` (the pipeline's `maxPrompt`, `maxContext` and `maxSchema` are defined from them), and a schema that doesn't compile or bundle. A gone context item is listed `Gone`; a 404 schema is omitted.
- Materialises the skills with the pipeline's `skillFetcher` (its git and skills cache, as `FetchSkills` in a run) into a scratch directory, `WorkRoot/.bundle-<id>`: a fixed name, so a leftover from a crashed call is replaced, as `makeWorkspace` replaces a run's (the leading dot keeps it apart from every task's workspace). `runbundle.Write` copies its `skills/` into `dir`, and the scratch directory is removed on every return. Host git fetches only ccf-skills at a pinned commit, never an agent checkout (P1 §2's "materialised from the supervisor's cache"). The fetch goes through the gitcred socket, which serves the job's task and read tokens (a skills repo may be private): PrepareBundle starts it as the run's `credentials` step does and closes it before it returns, so it lives only for the call.
- Strips the lease's credentials into `GitAccess` (repo, host, permissions, read scopes; no token).
- Checks every byte it will write (the manifest, `task.json`, the prompt, the context items, the schema, the CA, the codex account, the skill files) for an exact match of each of the job's real secrets (the git token and the read tokens, the agent token, a run token when the job carries one), and refuses a hit, naming the file but never the token. It is an exact match, not a shape scan: a token-shaped example in an issue body is no secret of the run's.
- Checks `ctx` is not done, then calls `runbundle.Write`. Its errors name the step that failed (`prompt:`, `schema:`, `credentials:`, `skills:`, `bundle:`, …) and are redacted with the run's redactor, as a host step's are. A failure before the VM exists fails the run as the matching host step would (`harness-error`).

## Trust model

**The VM is untrusted.** The LLM is root in it, and the pipeline, the Result check and the redaction now run there too. The design keeps that safe by what never enters the VM, not by what runs in it:

- **The bundle holds no secret,** so a VM that leaks it leaks the task's own data, which the agent can read anyway. Git, GitHub API, model and dispatcher credentials exist only in the gateway's memory (P1 §5).
- **Everything the VM returns is data:** the events, `outcome.json`, the transcript and `environment.log`. The supervisor (phase 5) re-validates the Result against the schema it fetched itself (`pipeline.ValidateResult`), re-redacts the transcript with the real tokens, enforces the caps (the outcome 2 MiB, events 64 KiB a line, the transcript 20 MB) and only then uploads.
- **A VM's throttle is clamped on the host:** `ReadOutcome` (and the supervisor, with `ClampThrottle`, for an `outcome` event it acts on) moves a `throttledUntil` later than `now + 6h` (`MaxThrottle`) to that, and a past or zero one to `now + 1m`, so one VM can't park the agent host for every task. A longer real limit (codex's weekly cap) throttles again each 6h window.
- **Usage from the VM is forgeable,** and so is the VM's own `maxCostUSD` check; `maxWallClock` is enforced from outside by the supervisor's deadline.
- **`ro` checkouts are advisory** in the VM; the gateway's refusal of `git-receive-pack` is the guarantee, as GET-only api.github.com is for GitHub writes and the dispatcher's server-side checks are for effects.

## What other phases rely on

### Phase 2b's base image

Checked against [workspace-host.md](workspace-host.md#the-vm-steps-p1-2):

| Needed by phase 3 | The base (phase 2b) |
|---|---|
| `factory-agent` on `PATH` | `/usr/local/bin/factory-agent`, sha256-checked at build |
| bash, git, `update-ca-certificates` | Ubuntu 24.04's bash; apt's `git` and `ca-certificates` |
| systemd dockerd reading `/etc/docker/daemon.json` | docker-ce from Docker's repo, enabled; `admin` in `docker` |
| curl ≥ 7.86 (CIDRs in `NO_PROXY`) | Ubuntu 24.04's curl 8.5 |
| the pinned claude and codex | `/usr/local/bin/claude` (2.1.304), `/usr/local/bin/codex` (rust-v0.163.0) |
| root when needed | `admin` with passwordless sudo |
| gh with its extensions | gh and the pinned `gh-pr-review`, `gh-stack` for `admin` (found through `XDG_DATA_HOME`) |
| kind images | the warm base's `/var/lib/factory/kind-images/*.tar` (`FACTORY_KIND_IMAGES`); the base has the empty directory |

### Phase 4's gateway

Specified in [gateway.md](gateway.md); in short:

- An explicit forward proxy: absolute-form HTTP and CONNECT.
- `dispatcher.factory.internal` maps to the real dispatcher, with `Authorization` replaced by `Bearer <runToken>`; the VM only needs `POST /tasks/{id}/effects`, and it's the only route the gateway opens ([gateway.md](gateway.md#the-dispatcher)).
- github.com: Basic auth by the repo in the path; `git-receive-pack` refused for repos that aren't rw for the task.
- api.github.com: GET only, `Authorization` replaced.
- Anthropic: the bearer replaced, or `x-api-key` set. chatgpt.com: the bearer and `chatgpt-account-id` replaced.
- **Always replace, never match placeholders.**

### Phase 5's supervisor

- Calls `PrepareBundle`, `Put`s the bundle, then execs `factory-agent run --proxy <gateway> --remaining <left>` (as `admin`: no `sudo`).
- Decodes the events and relays `progress` and `usage` to the worker's `runner.Sink`; `step` events go to its log.
- Cancels with SIGTERM, then SIGKILL after its grace, then `Destroy`.
- Collects `out/` after the `outcome` event or the exit, and treats it as untrusted ([trust model](#trust-model)). Exit 3 or 4 without an outcome is `failed{harness-error}`.

## Tests

- **`fakegateway`** (`internal/agent/internal/fakegateway`, test-only): an explicit forward proxy. For `dispatcher.factory.internal` (absolute-form HTTP) it strips `Authorization`, injects `Bearer rtk_test…` and forwards to the test dispatcher. For CONNECT to github.com and api.github.com it terminates TLS with a per-test CA, injects Basic auth by the repo in the path, refuses `git-receive-pack` for repos that aren't rw and refuses non-GET on the API. Other CONNECTs are tunnelled. It records every request and has `AssertNoClientCredential(t)`: no request arrived with a real credential.
- **`harnesstest`** gains `GatewayJob()` (a job with empty tokens and a nil API) and `Bundle(t, h)` (a bundle from `PrepareBundle` against its dispatcher and git server).
- **`vmrun`'s tests** have a `TestMain` that dispatches `fx` to `effector.Main`, so the test binary is the `factory` CLI.

The layers:

1. **Pipeline gateway mode:** no socket and no `FACTORY_TOKEN`; the bundle's prompt equals the API's (a golden); `Finished` runs before `Down`; `Root` isn't removed; the host's step list and errors are unchanged but for `env`.
2. **envrun:** a table over setup, up, ready and down: `-eo pipefail` failures, a timeout on the fake clock, a 3xx being ready, the proxy not used for ready, the log's redaction and cap, the 8 KiB tail, `FACTORY_VERSION_*` names, `down` after a failed `up`, cancel killing the process group.
3. **The adapters' `IsolationVM` goldens:** claude's settings, MCP config and environment; codex's `config.toml`, flags and `auth.json`.
4. **vmrun end to end, in process (CI):** `PrepareBundle`, then `vmrun.Main` with the fake adapter doing `factory push` and `factory comment` through the fakegateway, and the no-credential check: the harness's environment dump and a walk of `work/`, `state/` and the bundle for the real tokens and `ghs_`, `rtk_` and `sk-` shapes. Cases: success, repair, invalid twice, throttled, `--remaining 1s`, SIGTERM (once, and twice during `down`), a bad bundle (exit 3), and an events golden (`-update`).
5. **A plain container** (opt-in: `FACTORY_E2E_CONTAINER=1`, `make e2e-run-container`): the linux binary and the fakegateway in a Debian container, `--system-setup off`, the environment's scripts run for real.

## Delivery

A gh stack on phase 2's (about the line counts, without generated code):

1. docs(agent): this LLD (~300).
2. refactor(agent): the pipeline sources (prompt, context, transcript), `OnStep`, `Finished` (~350).
3. feat(agent): runbundle: the bundle, the outcome and the event stream (~600).
4. feat(agent): `PrepareBundle`, the bundle sources, the skillFetcher extraction, harnesstest's `Bundle` (~800).
5. test(agent): fakegateway (~500).
6. feat(agent): pipeline gateway mode: `Root`, `GitAuth`, `Env` (~600).
7. feat(agent): the environment runner (envrun, `pipeline/environment.go`, `ReasonEnvironmentError`) (~850). **Needs #100 merged.**
8. feat(harness): claude-code `IsolationVM` (~450).
9. feat(harness): codex `IsolationVM`, with the `auth.json` golden (~500).
10. feat(effector): the workspace runtime: the 401 message and the stack git fallback (~120).
11. feat(agent): vmrun system setup: the CA, the proxies, dockerd and the docker client (~350).
12. feat(agent): `factory-agent run`: vmrun, the job, the events, the adapters, the end-to-end test (~1000–1100: a size exception, or split off the events and the job).
13. test(agent): `factory-agent run` in a plain container (~300).

## Risks and open questions

1. **The pipeline now runs in the untrusted zone.** The supervisor re-validates everything, and usage reported from the VM is forgeable ([trust model](#trust-model)).
2. **claude as root:** avoided by running as `admin` ([the run's user](#the-runs-user)). `IS_SANDBOX=1` stays unverified for a root run (the container layer); decide at the first smoke run whether the VM ever needs it.
3. **The run token's scope** is wider than the VM needs (prompt, context, schema and transcript are fetched or uploaded on the host). Phase 6 could narrow it to effects.
4. **Skills are materialised on the host,** as P1 says: host git fetches only ccf-skills, never an agent checkout.
5. **gh stack in the VM** is forced to the git fallback.
6. **`environment.log` stays out of the transcript;** whether phase 5 uploads it is open.
7. **`NO_PROXY` CIDRs** need curl ≥ 7.86 (the base has 8.5), and the docker proxy config must exist before `setup` (it does: system setup runs first).
8. **The scripts run under bash `-eo pipefail`,** with the scratch `HOME` rather than `/home/admin`. Check that against local-dev's scripts in phase 7.
9. **A host runtime with an environment** skips it with a warning. **Decided (2026-10-22):** a supervisor advertises the capability label `vm-workspace`, and a TaskSpec with `workspace.environment` requires it (phase 6 adds the label; phase 7's catalog TaskSpecs require it), so such tasks are only scheduled to VM workspaces.
10. **The codex `auth.json` shape is from Phase 0,** pinned as constants and re-checked by `make smoke-codex`.
11. **Clocks:** `--remaining` avoids skew, and the supervisor enforces the hard deadline from outside.
12. **`ReservedEnvNames`** should add `FACTORY_RUNTIME` and `FACTORY_KIND_IMAGES`, so `exposes` can't set them: a small contract PR.
13. **Containers and the proxy. Decided (2026-10-22):** the docker client gets no `proxies` (see `DOCKER_CONFIG` above), because they'd send calls between compose services by name (`http://api:8080`) to the gateway, which refuses private addresses. dockerd keeps its proxy for image pulls. Containers that need the internet aren't supported for the PoC; revisit in phase 7 if local-dev's showcase needs it (per-service `HTTPS_PROXY` set by the TaskSpec's `up`).
