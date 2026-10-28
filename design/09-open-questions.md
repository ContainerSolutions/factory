# 09 — Open questions and decisions

> Status: **decided** · Last updated: 2026-10-15

## Decisions

| # | Topic | Decision | Where it lands |
|---|---|---|---|
| — | Runtime | Pluggable. The PoC is one plain Go binary with registered agents only (ADR-0005). | [10](10-poc.md) |
| — | Harness | Agnostic. Codex is the default; Claude Code reviews and validates. In the PoC both run locally on their owners' subscriptions; beyond it, Claude uses org API keys (ADR-0008). | [10](10-poc.md#registered-agents-codex-on-subscriptions-claude-on-api-keys) |
| — | Central config | Git, in this repo (ADR-0006). | [03](03-domain-model.md) |
| — | Factory state | In-memory store first, rebuilt from provider projections (ADR-0001); now a durable store (a state dir, or object storage shared by several dispatchers) with no rebuild ([P3](proposals/P3-dispatcher-state-in-object-storage.md#no-rebuild-from-markers)). | [02](02-architecture.md#db-and-queue) |
| Q1 | Slack | **Out of the PoC.** When it comes, rules must be able to filter flexibly on who sent a message, expressions over its content, and commands (`@factory /review`, `@factory /implement`). The CEL environment and the generic rule model already allow this; nothing Slack-specific is designed now. | [Future work](08-roadmap.md#future-work-not-planned) |
| Q2 | What release docs cover | **Versioned docs, per repository** (each repo has its own release cycle). Each release covers:<br/>• release notes<br/>• API reference bound to that release<br/>• catalog entries<br/>• upgrade guides<br/>• feature description and user guide (what a new `CCF_*` flag is for, how the feature works, its default value, permissions needed). | [10](10-poc.md#versioned-docs-per-repository) |
| Q3 | Docs PR granularity | **One docs PR per release train.** The trigger is the train's own end signal. Switching to one PR per release is a one-rule change. | [10](10-poc.md#rules-use-case-1-pack-release-docs) |
| Q4 | Identities | **New GitHub Apps**, `ccf-factory` (author) and `ccf-factory-reviewer` (reviewer). Gustavo creates them and provides credentials for the PoC. | [10](10-poc.md#provider-group-pool-queue) |
| Q5 | App approvals and rulesets | **Proposed default accepted.** Verify whether the reviewer App's approval counts toward required reviews. If it doesn't, `merge` adds the merge label instead of merging directly. | [10](10-poc.md#taskspecs) |
| Q6 | LLM accounts and billing | **Out of scope for the PoC.** Agents use their owners' subscriptions. | — |
| Q7 | Ticket system | **GitHub Issues** for the PoC. Jira may become a provider later, only if the factory proves its value. | [05](05-agent-runtime.md#making-ccf-skills-headless-friendly) |
| Q8 | Changing `ccf-skills` | **Change existing skills, backwards-compatibly.** Headless mode is **opt-in** (`FACTORY_HEADLESS=1`), so interactive use is unchanged. | [05](05-agent-runtime.md#making-ccf-skills-headless-friendly) |
| Q9 | Reviewing human PRs | **Review all human PRs.** The factory reviews every human PR in scope. It doesn't push to or merge human PRs. | [10](10-poc.md#rules-human-prs-pack-review-human) |
| Q10 | Shadow mode | **Not needed.** Develop with the `merge` rule excluded, and add it only once everything else is trusted. Use **fake versions and fake trains** on docs to exercise the flow before real releases. | [08](08-roadmap.md) |
| Q11 | Roadmap scope | **The PoC only.** Phases B and C are removed. Everything else is unplanned future work. | [08](08-roadmap.md) |
| Q12 | Hosting the dispatcher | PoC: a laptop or small VM behind a tunnel. **Long term: Cloud Run** (min-instances=1). | [Future work](08-roadmap.md#future-work-not-planned) |
| Q13 | Transcript retention | **90 days, in the store only** (memory now, a DB later), with no external blob store. Revisit once the feature exists. | [10](10-poc.md#poc-scope) |
| Q14 | Licence | **AGPL-3.0.** The factory repo is open source, so catalog and prompts are public. Secrets are only ever referenced, never inlined. | [08](08-roadmap.md) |
| Q17 | Who `maintainers` is | **The CCF GitHub org admins** (`role: admin`, the same rule the release train uses for its comment commands). | [10](10-poc.md#provider-group-pool-queue) |
| Q18 | Approval channels | **Left out for now.** The PoC's use cases are fully automatic. The `Approval` kind stays in the model for later. | [03](03-domain-model.md#approval) |
| Q15 | Codex on ChatGPT plans | **Checked.** OpenAI's Codex docs recommend API keys for programmatic and CI workflows, and offer Enterprise access tokens for automation. The Terms forbid sharing credentials or letting others use your account. Nothing found forbids running the official CLI under your own login on your own machine. **The PoC proceeds** that way: device-code sign-in, credentials never shared or copied. **Beyond the PoC, Codex moves to API keys or access tokens.** | [10](10-poc.md#what-the-vendors-docs-say-and-what-it-means-for-the-design) |
| Q16 | PoC agents | **Gustavo's machine with no limits**, plus **one more maintainer**. Each owner sets how much they lend (queues, harnesses, concurrency, runs and tokens per day, hours, or `unlimited`) in their worker's local config. | [10](10-poc.md#provider-group-pool-queue) |
| Q19 | Docs navigation and plugins | Readers go from the general CCF docs to **API** (with architecture for now), **UI** (features and navigation, with GIFs), **Agent**, and a **Catalog** page. Each product and each plugin is its **own versioned instance**, so you can open a plugin at a given version and see everything about it. Instances are generated from a manifest. | [10](10-poc.md#versioned-docs-per-repository) |
| Q20 | First versioned instance | **`api`**, with the architecture docs moved into it for now. | [10](10-poc.md#versioned-docs-per-repository) |
| Q21 | Docs version granularity | **One docs version per minor line** (`api` `0.22`). Patch releases update their line in place and add to its release notes. | [10](10-poc.md#keeping-60-instances-manageable) |
| Q22 | Policies docs | **A policies catalog on each plugin's page**: all of that plugin's policy bundles (there are more bundles than plugins, all released independently), with version, controls covered and compatible plugin lines. The plugin ↔ bundle mapping is explicit in the docs manifest. | [10](10-poc.md#how-readers-navigate) |

## Still open

Nothing right now. New questions get added here as the PoC is built.
