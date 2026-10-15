# LLD — Opt-in headless mode for ccf-skills (SKL)

> Workstream: [SKL](../../definitions.md#skl-ccf-skills-bump) · Tasks: SKL-S1-T01…T03 · Repo: `compliance-framework/ccf-skills`
> Decision Q8: change skills **in place, backwards-compatibly**; headless is **opt-in**.

## The contract (`HEADLESS.md` at the repo root)

A skill runs headless **iff** `FACTORY_HEADLESS=1`. Otherwise every existing behaviour is unchanged.

| Variable | Meaning when headless |
|---|---|
| `FACTORY_HEADLESS=1` | no human at the keyboard |
| `FACTORY_HARNESS` | `codex` \| `claude-code` |
| `FACTORY_TASK_ID`, `FACTORY_API`, `FACTORY_TOKEN` | the factory run; `factory` CLI is on `PATH` |
| `FACTORY_OUT`, `FACTORY_RESULT` | scratch output dir; where the final result JSON goes |
| `FACTORY_STACK_URL`, `FACTORY_STACK_API` | local CCF stack, when up |

Rules every skill follows in headless mode:

1. **Never prompt.** Replace "ask the user", "wait for confirmation", `AskUserQuestion` with: decide if within the task's allowed effects; else write `{"status":"needs_input","question":…}` to `$FACTORY_RESULT` and stop.
2. **No loops/schedulers.** No `CronCreate`, no watch loops; one pass, then exit (the factory's rules provide iteration).
3. **Writes via the Effector.** GitHub writes go through `factory …` (or the factory MCP tools); never `gh pr create`, `gh api -X POST`, etc. Git pushes via `factory push` / `factory stack push`.
4. **Portable.** The headless path uses only `SKILL.md` + scripts/CLIs — no Claude-only plugin features (hooks, slash commands, subagents) so Codex can run it.
5. **Structured result.** Finish by writing the result JSON (`factory result --file …`).
6. **Tickets from GitHub Issues** (Q7): skills that read `./tickets/{ID}.md` accept a GitHub issue ref; the agent pre-writes `./tickets/<owner>-<repo>-<n>.md` (title, body, labels, comments) when headless.

Each changed `SKILL.md` gets a short section, identical shape everywhere:

```markdown
## Headless mode (CCF Factory)

Applies only when `FACTORY_HEADLESS=1` (see /HEADLESS.md). Differences from the interactive flow:
- <step N>: <what changes>
- Result: write `{…}` to `$FACTORY_RESULT` (schema: <name>).
```

## Per-skill changes

| Skill | Headless differences | Task |
|---|---|---|
| `lisa` | one-shot always (no `CronCreate`); `lib/lisa` write subcommands delegate to `factory …` when `FACTORY_API` is set (`pr create`, `pr comment`, `thread reply`, `label add`, `push`, `stack push/rebase`); identity is the factory's (no `LISA_APP_ID` use); stack mode honours `LISA_BASE` as today; result `{pr, branch, threadsResolved, stack[]}` | SKL-S1-T02 |
| `ccf-review:review` | phase 1/2 console briefs → written to `$FACTORY_OUT/brief.md` (not shown); triage step auto-accepts findings with severity ≥ should-fix, drops nits; **Questions** → outcome `needs-human` (never answered by the agent); publish without confirmation via `factory review …` (inline comments file); `--again` behaviour unchanged; result `{findings, prs[]{pr, event}}`; respects an extra `--packs` arg so the factory can force `docs-site` | SKL-S1-T02 |
| `resolve` | reads review threads from GitHub (not `./tickets/<type>/<ticket>/reviews/NNN.md`) when headless; "ask me to verify" → run the repo's checks itself; replies via `factory thread reply/resolve` | SKL-S1-T03 |
| `ccf-test` | base URLs from `FACTORY_STACK_URL`/`FACTORY_STACK_API`; credentials from the stack's seeded admin (local-dev defaults), never from `.env` | SKL-S1-T03 |
| `sidebyside` | output dir from caller (`--out`); no branch checkout of api/ui (uses the running stack); screenshots only | SKL-S1-T03 |

Untouched in the PoC: `bugreport`, `bugfix`, `plugin`, `policy-review` (they get the convention later).

## Backwards-compatibility checks (in each PR body)

- Interactive run of the changed skill with `FACTORY_HEADLESS` unset: identical prompts/steps (diff of the skill's interactive sections = none, except the appended headless section).
- `ccf-review:rules lint` and the `skills-repo` pack pass (`SKILL-FM-001`, `SKILL-PATH-001`, …).
- lisa's existing tests (`lisa/tests` if present) pass.
