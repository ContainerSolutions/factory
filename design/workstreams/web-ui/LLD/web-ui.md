# LLD — Factory web UI (WEB)

> Workstream: [WEB](../../definitions.md#web-web-ui) · Tasks: WEB-S0-T01, WEB-S1-T01…T04 · Directory: `web/` only
> Design: [02 §Factory UI](../../../02-architecture.md#factory-ui) · Contract: [contracts.md §HTTP API](../../foundation/LLD/contracts.md#http-api)

## Stack and layout

Matches `compliance-framework/ui` conventions (read its `AGENTS.md`), so `ccf-review`'s `vue-app` pack applies unchanged.

| Concern | Choice |
|---|---|
| Framework | Vue 3.5 + TypeScript (strict), `<script setup>` |
| Build | Vite 6; `build.outDir = ../internal/webui/dist`, `emptyOutDir: true`; `base: '/'` |
| UI kit | PrimeVue 4 **unstyled** via Volt wrappers (vendored in `src/volt/`, same as `ui`) + Tailwind v4 |
| State / routing | Pinia, Vue Router (history mode; server has SPA fallback) |
| API client | `openapi-typescript` → `src/api/schema.d.ts` (generated from `../api/openapi.yaml`, `npm run gen:api`, CI checks freshness) + `openapi-fetch` client in `src/api/client.ts` (cookie session, `credentials: 'same-origin'`) |
| Live data | `EventSource` on `/api/v1/stream` (composable `useStream(topics)`, auto-reconnect with `Last-Event-ID`) |
| Mocks | MSW handlers in `src/mocks/` built from `../api/examples/*.json` + an SSE mock emitter; `npm run dev:mock` |
| Tests | Vitest + Vue Test Utils (co-located `*.spec.ts`), Playwright e2e over MSW (`e2e/`) |
| Quality | ESLint (same config as `ui`), Prettier (`package.json` key, same as `ui`), `vue-tsc --noEmit`; `npm run reviewable` = format check + type-check + lint + unit |

```text
web/
├── package.json  .nvmrc  vite.config.ts  vitest.config.ts  playwright.config.ts  eslint.config.js  tsconfig*.json
├── index.html
└── src/
    ├── main.ts  App.vue  router/index.ts
    ├── api/            # schema.d.ts (generated), client.ts, stream.ts
    ├── stores/         # session.ts, workitems.ts, runs.ts, pools.ts, catalog.ts
    ├── composables/    # useStream, usePolling, useConfirm
    ├── views/          # BoardView, WorkItemView, RunView, PoolsView, CatalogView, LoginView, NotFoundView
    ├── components/     # workitem/, run/, pools/, catalog/, shared/ (PageHeader, StatusPill, TimeAgo, JsonViewer, DecisionWhy)
    ├── volt/           # vendored Volt wrappers (not linted, same rule as ui)
    └── mocks/          # MSW handlers + fixtures
```

## Views

| Route | View | Data | Live |
|---|---|---|---|
| `/` | **Board** — columns by derived state (`needs-human` lane first, then Running, Waiting, Done last 7 d); filters: RuleSet, provider, repo, TaskSpec | `GET /workitems?…` | `workitems` topic |
| `/workitems/:id` | **Work item** — header (links across providers, state keys, counters), **timeline** (events, decisions, tasks, runs, effects merged by time), **DecisionWhy** drawer per decision: RuleSet, rule, `where` inputs (JSON), rail verdict, catalog SHA | `GET /workitems/{id}` | `workitems/{id}` |
| `/runs/:id` | **Run** — progress stream, transcript viewer (lazy, collapsible turns), effective harness config, usage, effects (linked), agent + owner, Result JSON | `GET /runs/{id}` | `runs/{id}` |
| `/pools` | **Queues and pools** — per queue: depth, running, pending with reasons (`budget`, `no-capable-agent`, `agent-throttled`, `lending`); per agent: owner, capabilities, **lending**, phase, throttle window, today's runs/tokens | `GET /queues`, `GET /pools`, `GET /agents` | `agents`, `tasks` |
| `/catalog` | **Catalog** — SHA + status, providers (health), RuleSets → selected rules (with params after overrides), TaskSpecs, queues, pools; **dry-run** panel (paste/pick a recorded event → `POST /catalog/dry-run` → Evaluation rendered with DecisionWhy) | `GET /catalog…` | — |
| `/login` | GitHub sign-in button → `/api/v1/auth/login` | `GET /auth/me` | — |

## Controls (WEB-S1-T04)

All go through `POST /commands`, behind a confirm dialog that shows the exact command JSON, visible only to `me.isAdmin`:

| Control | Where | Command |
|---|---|---|
| Pause / resume (global, provider, queue, work item) | header menu, pools view, work item | `{type: pause\|resume, scope, name}` |
| Cancel run | run view | `{type: cancel, task}` |
| Re-dispatch (optionally another harness/model) | run view | `{type: redispatch, task, harness}` |
| Resync catalog | catalog view | `{type: resync-catalog}` |

## Auth and session

- On load `GET /auth/me`: `401` → router redirects to `/login`; `{login, isAdmin}` stored in `session` store.
- Read-only for org members; controls hidden and the command buttons disabled when `!isAdmin` (server enforces anyway).

## Non-functional

- Lists paginate (cursor) and virtualise beyond 200 rows.
- Untrusted text (issue bodies in context previews, transcripts) rendered as **text**, never `v-html`; markdown rendering only through a sanitising renderer for Templates previews.
- Accessibility: keyboard navigation for board and dialogs; colour is never the only status signal (pill text).
- Bundle budget: < 500 KB gzipped initial JS.

## Tests

- Unit: every component with props/state variants; stores with MSW.
- SSE: mocked EventSource emitting progress, reconnect with backfill.
- e2e (Playwright, MSW): board → work item → DecisionWhy; run live progress; admin control sends the right body; non-admin sees no controls.
