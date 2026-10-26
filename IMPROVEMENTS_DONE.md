# Factory improvements: done

The record of what was decided and shipped. **Open work is in [IMPROVEMENTS.md](IMPROVEMENTS.md)**;
read that one when planning. This file is history: the why behind past changes, and PR numbers to
find them. Kept on the `improvements` branch with IMPROVEMENTS.md (never merged to main).

## The two streams of 2026-10-21

| Stream | Spec | State |
|---|---|---|
| **P1. Each task runs in a VM workspace** | `design/proposals/P1-agent-workspaces.md` | The Mac track (phases 1–8) shipped by 2026-10-24, and the Linux track (phase 9) by 2026-10-26. Phase 6 was skipped for the PoC; phase 10 is open. |
| **P2. The dispatcher keeps its state in a file** | `design/proposals/P2-dispatcher-state-file.md` | Shipped: `internal/store/filestore`, `serve --state-dir`, `factory state`. |

What P1 replaced:
- **A, "a hard sandbox per agent":** one VM per task, with the harness's own sandbox off, and
  credentials held by a gateway outside the VM.
- **B, "per-task environments in the TaskSpec":** `workspace.environment`, run by the in-VM
  runtime, with versions resolved at compile time.

## What the first live runs taught us (2026-10-20/21)

| # | Symptom | Stopgap | Real fix |
|---|---|---|---|
| 1 | codex's sandbox denied writes to `.git` | #81 | P1: no harness sandbox in the VM |
| 2 | a hook planted in `.git` could run on the host | #82 | P1: git runs in the VM |
| 3 | plan-epic's read-only sandbox blocked `$FACTORY_RESULT` | #87 | P1 |
| 4 | codex's MCP approval prompt in a headless run | #87 | P1 |
| 5–6 | no yarn, so checks and docs builds never ran | #85 (`not-run`) | P1: a toolchain in the base VM |
| 7–8 | UC2 had no stack for screenshots; STYLE.md required local-dev | #88 (temporary API/UI) | P1 §4: environments |
| 9 | skills called `factory-agent stack env`, which isn't on a run's PATH | #90, #91 (closed) | ccf-skills v0.9.0 reads `FACTORY_STACK_*` |
| 10 | local-dev TTY error when driven headlessly | local-dev#127 | — |
| 11 | dispatcher port clash with local-dev | #78/#79 (port 8800) | P1: own network per VM |
| 12 | an environment failure reported as `blocked` | #80 | kept |
| 13 | claude rejected `--json-schema` with an `allOf` root | #84 | kept |
| 14 | review didn't start: created PR not linked | #83 | kept |
| 15 | item stuck in Running | #86 | kept |
| 16 | redispatch reused the old prompt | #89 (recompile) | kept |
| 16a | state lost on restart | — | P2 |
| 16b | the harness held `FACTORY_TOKEN` and `GH_TOKEN` | — | P1 gateway |
| 18 | a review waited 10 min with one agent | — | #137 |

Row 17, missing App installations, is still open: IMPROVEMENTS.md, group 1 (f).

## Stopgap PRs

| PR | Keep? |
|---|---|
| #80, #83, #86, #89 | yes: general fixes |
| #84 | yes, while claude takes `--json-schema` |
| #81, #82, #85, #87, #88 | no: removed in P1 phase 10 (listed there) |
| #90, #91 | closed, superseded by P1 |

## P1 Mac track (2026-10-22 → 2026-10-24)

| Phase | PRs |
|---|---|
| 1 contracts | #100 (`workspace.environment`), #101 (lease credentials, `runToken`), #102 (enrollment) |
| 2 host init and base VM | #103, #104, #130 (codex's code-mode host) |
| 3 `factory-agent run` in the VM | #105–#116, #129 (envrun) |
| 4 gateway | #117–#123 |
| 5 supervise and the tart driver | #125, #126, #127 |
| 6 run tokens and enrollment | skipped: the gateway injects the agent token |
| 7 environment in the catalog | #131 (web: closed), #132 (compiler), #133 (catalog), #134 (GIF tools), ccf-skills #24 (v0.9.0) |
| 8 first real runs | fixes: #136 (bundle off noexec `/run`), #137 (anti-affinity per harness, `--anti-affinity-wait`), #138 (collect only the outcome files), #139 (claude structured-output retries), #140 (the clean-slate environment) |

Phase 8 results:
- **UC2:**
  - plan-epic `single`, then build-single, implement, docs-review and fix, on mock-docs#14, #18
    and #21;
  - the GIF case worked after #138 and #139;
  - two lost outcomes were replaced by hand-written events.
- **UC1:** train workflows#137 → mock-docs#22. release-docs ran with the showcase environment at
  api 0.21.1 / agent 0.9.2, then docs-review-verify approved it against its own stack.
- **Verified by the runs:**
  - claude's and codex's placeholder logins through the gateway;
  - the name-constrained CA accepted by claude, codex, git, Node and Python;
  - the model-path allow-lists, with no refusals seen.

Decisions taken on the way:
- **The gateway holds only the factory's own credentials and the model logins,** never
  environment secrets (2026-10-23).
- **The environment is local-dev's plugin-less clean slate** (`make demo-up-slate`); agents
  configure `slate-agent` through overlays (#140).
- **The reviewer App had to be installed on `compliance-framework/local-dev`,** for
  docs-review-verify's read token.

## Group 1: operability (2026-10-26)

One stack, merged:
- #163: the contract;
- #164: event replay (only rules that never fired act; 409 `replay-history-gone`);
- #166: close and delete a work item;
- #167: App installation checks, `reasonDetail`;
- #169: the environment log;
- #162: binary-only base VM updates;
- #165: ChatGPT refresh, with at most one 401-driven refresh a minute;
- #168: flaky tests;
- #170: web for all of it.

Then two fixes from the first runs after it:
- #172: `NODE_USE_ENV_PROXY`, so corepack and yarn reach the registry through the gateway;
- #173: implement ships ccf-review, and codex links each plugin's `bin/` so lisa finds `ccfr`.

Verified on real runs (2026-10-26, from the Mac's supervisor records):
- **#162:** the base VM got the new binary without a rebuild. `basevm.json`'s `builtAt` predates
  the binary, but its `agentSHA256` matches it.
- **#173:** codex implement runs ran `ccfr detect` from the linked `bin/`.
- **#172:** corepack's yarn 1.22.22 download works through the gateway, and claude-code docs
  reviews run normally with `NODE_USE_ENV_PROXY=1`.

## P1 phase 9: the Linux track (kata), 2026-10-26

- **#174:** the kata driver's LLD (`workspace-kata.md`) and the Q8 dry run. A warm Docker disk
  replaces P1's registry mirror.
- **#175:** the per-OS state dir and the platform step.
- **#176:** base.sh for amd64, and the image's build context.
- **#177, #178:** host init's install steps (nerdctl-full, Kata) and its services (containerd,
  buildkitd, the workspace firewall).
- **#179, #181, #182:** the kata root helper and its verbs.
- **#180:** `vmexec`, the drivers' shared exec layer.
- **#183, #184:** the kata workspace driver and its integration tests.
- **#185, #186:** host init's `kataimage` and `katawarm` steps.
- **#187:** `supervise --driver kata`, with concurrency above 1.
- **#188, #189:** IPv4-only image builds, and login on Linux.
- **#190:** the `local` Pool lets an agent run three tasks.

## P1 phase 10, stack 1: the common part (2026-10-26)

Merged before the phase 9 follow-ups started, because both touch `cmd/factory-agent` and
`internal/agent/config`:
- **#192:** supervise is the only runtime. `worker` and `--runtime host` are gone; the shared
  lease loop moved to `agentloop.go`.
- **#193:** the host stack, `localDev` and the label probes are gone. The config decode is
  strict, so a leftover `localDev` key in `agent.yaml` now fails.
- **#194:** the worker and local-stack LLDs fold into supervise and workspace-run.
- **#195:** the prompts lift "never install" (#85's stopgap): agents install what the repo's
  checks need through its package manager, never system tools, and never change lockfiles.
  Every TaskSpec pins ccf-skills `v0.10.0`.
- **ccf-skills#25 (`v0.10.0`):** HEADLESS.md rule 9, and docs-release runs `yarn install
  --frozen-lockfile` before building.

## Other items closed

- **C. Anti-affinity:** kept, but only for the same harness, with a configurable wait (#137).
- **A work item outliving its closed issue or PR:** `close-on-primary-closed` and `closed-gate`
  (#133), and Done on the board (#131).
- **`build-single`** moved from the owner's local catalog to main (#133).
- **UI, the owner's list of 2026-10-24:**
  - catalog tabs and a detail pane (#141–#143, #146, #153);
  - work-item Graph, LLM tasks and Timeline tabs (#144);
  - exact effect → event links (#147–#149);
  - graph fixes and layout (#151, #152, #154, #156, #159–#161).
- **codex's second writable checkout getting only its `.git`:** moot, there's no codex sandbox in
  the VM.
