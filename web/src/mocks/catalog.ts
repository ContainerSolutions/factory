import { http, HttpResponse, type HttpHandler } from 'msw';
import { API_BASE, type Schemas } from '@/api/client';
import { examples } from './examples';
import { poolViews, queueViews } from './pools';
import { workItemDetails } from './workitems';

type CatalogObject = Schemas['CatalogObject'];
type Rule = Schemas['Rule'];
type ProviderView = Schemas['ProviderView'];

// The contract's own examples: the summary, the github-public provider and the triage TaskSpec.
const exampleSummary = examples.get('getCatalog') as Schemas['CatalogSummary'];
const exampleProvider = (
  examples.get('listProviders') as Schemas['ProviderList']
).items[0]!;
const triage = examples.get('getCatalogObject') as Schemas['TaskSpec'];

function rule(
  name: string,
  labels: Record<string, string>,
  spec: Schemas['RuleSpec'],
): Rule {
  return { kind: 'Rule', metadata: { name, labels }, spec };
}

const github = { matchLabels: { family: 'github' } };

/** The rules: two intake rules, a merge rule, and the api RuleSet's two. */
export const rules: readonly Rule[] = [
  rule(
    'triage-outside-issues',
    { pack: 'intake' },
    {
      providerSelector: github,
      on: ['issue.opened'],
      where: "actor.tier != 'maintainer' && !actor.isBot",
      params: { queue: 'triage', harness: 'codex' },
      actions: [
        {
          dispatch: {
            taskSpec: 'triage',
            queue: '{{ params.queue }}',
            context: ['event.object'],
          },
        },
        { set: { triage: 'running' } },
      ],
      priority: 50,
    },
  ),
  rule(
    'triage-maintainer-issues',
    { pack: 'intake' },
    {
      providerSelector: github,
      on: ['issue.opened'],
      where: "actor.tier == 'maintainer'",
      actions: [{ set: { triage: 'skipped' } }],
    },
  ),
  rule(
    'merge-approved',
    { pack: 'merge' },
    {
      providerSelector: github,
      on: ['pr.review.submitted'],
      where: "event.data.facts.state == 'approved'",
      params: { mergeMethod: 'squash', minApprovals: 1 },
      actions: [{ effect: { op: 'pr.merge' } }],
      final: true,
    },
  ),
  rule(
    'implement-review-feedback',
    {
      pack: 'review',
      // A long label and a long where must stay inside their boxes.
      owner:
        'compliance-framework.platform-maintainers.release-engineering.oscal-tooling-working-group',
    },
    {
      providerSelector: github,
      on: ['pr.review.submitted'],
      where:
        "item.count('implement') < params.maxRounds && event.data.facts['review.state'] == 'changes_requested' && !event.data.actor.isBot && event.data.object.ref.startsWith('compliance-framework/')",
      params: { maxRounds: 3 },
      actions: [{ dispatch: { taskSpec: 'implement', queue: 'default' } }],
    },
  ),
  rule(
    'review-factory-prs',
    { pack: 'review' },
    {
      providerSelector: github,
      on: ['pr.opened', 'pr.updated'],
      actions: [{ dispatch: { taskSpec: 'review', queue: 'review' } }],
    },
  ),
];

/**
 * The docs RuleSet selects the intake pack minus the maintainer rule, plus merge-approved by ref,
 * with a default and an override, and one inline rule. The api RuleSet picks its two by ref.
 */
export const ruleSets: readonly Schemas['RuleSet'][] = [
  {
    kind: 'RuleSet',
    metadata: { name: 'docs' },
    spec: {
      providerSelector: { matchLabels: { family: 'github' } },
      scope: { repos: ['compliance-framework/docs'] },
      rules: {
        refs: ['merge-approved'],
        selector: { matchLabels: { pack: 'intake' } },
        exclude: ['triage-maintainer-issues'],
      },
      defaults: { harness: 'claude-code', minApprovals: 2 },
      overrides: { 'merge-approved': { mergeMethod: 'rebase' } },
      inlineRules: [
        {
          metadata: { name: 'label-docs' },
          spec: {
            on: ['issue.opened'],
            providerSelector: github,
            actions: [
              {
                effect: { op: 'label.add', labels: ['area/docs'] },
              },
            ],
          },
        },
      ],
    },
  },
  {
    kind: 'RuleSet',
    metadata: { name: 'api' },
    spec: {
      providerSelector: {
        matchExpressions: [
          { key: 'network', operator: 'In', values: ['public'] },
        ],
      },
      scope: {
        repos: ['compliance-framework/api', 'compliance-framework/factory'],
      },
      rules: { refs: ['implement-review-feedback', 'review-factory-prs'] },
    },
  },
];

function taskSpec(
  name: string,
  spec: Schemas['TaskSpecSpec'],
): Schemas['TaskSpec'] {
  return { kind: 'TaskSpec', metadata: { name }, spec };
}

export const taskSpecs: readonly Schemas['TaskSpec'][] = [
  triage,
  taskSpec('implement', {
    description: 'Address review feedback on a pull request.',
    inputs: {
      mode: { type: 'string', enum: ['new', 'address-review'], default: 'new' },
      issue: { type: 'string' },
      base: { type: 'string', default: 'main' },
    },
    instructions: { template: 'prompts/implement.md.tmpl', headless: true },
    skills: [
      { source: 'ccf-skills', ref: 'v0.9.0', use: ['lisa', 'docs-style'] },
    ],
    harness: {
      default: { name: 'claude-code', config: { model: 'claude-sonnet-5-*' } },
    },
    workspace: {
      repos: [{ ref: "=task.target.repo + '@' + inputs.base", mode: 'rw' }],
      hosts: ['registry.npmjs.org'],
      environment: {
        setup: 'corepack enable\nyarn install --immutable\n',
        exposes: { FACTORY_STACK_URL: 'http://localhost:8000' },
      },
    },
    identity: 'author',
    permissions: { contents: 'write', pull_requests: 'write' },
    effects: { allow: ['push-branch', 'pr.comment'] },
    limits: { maxTurns: 120, maxWallClock: '1h' },
    output: {
      schema: 'schemas/results/implement.json',
      outcomes: ['pr-opened', 'pr-updated', 'blocked'],
    },
  }),
  taskSpec('review', {
    description: 'Review a pull request against the CCF rule catalog.',
    effects: { allow: ['review.comment'] },
  }),
];

/** github-public (the example), a degraded enterprise instance and Slack. */
export const providerViews: readonly ProviderView[] = [
  exampleProvider,
  {
    provider: {
      kind: 'Provider',
      metadata: {
        name: 'github-enterprise',
        labels: { family: 'github', network: 'enterprise' },
      },
      spec: {
        type: 'github',
        github: {
          ...exampleProvider.provider.spec.github!,
          apiURL: 'https://ghe.example.com/api/v3',
        },
        scope: { orgs: ['platform'] },
      },
    },
    health: {
      status: 'degraded',
      message: 'rate limited until 12:30 UTC',
      checkedAt: '2026-10-19T11:58:00Z',
    },
  },
  {
    provider: {
      kind: 'Provider',
      metadata: { name: 'slack-ccf', labels: { family: 'slack' } },
      spec: {
        type: 'slack',
        slack: {
          mode: 'socket',
          appTokenRef: { name: 'slack-ccf', key: 'app-token' },
          botTokenRef: { name: 'slack-ccf', key: 'bot-token' },
        },
      },
    },
    health: { status: 'ok', checkedAt: '2026-10-19T11:59:30Z' },
  },
];

const groups: readonly Schemas['Group'][] = [
  {
    kind: 'Group',
    metadata: { name: 'maintainers' },
    spec: {
      members: [
        {
          provider: 'github-public',
          orgRole: { org: 'compliance-framework', role: 'admin' },
        },
      ],
    },
  },
  {
    kind: 'Group',
    metadata: { name: 'readers' },
    spec: {
      members: [
        {
          provider: 'github-public',
          orgRole: { org: 'compliance-framework', role: 'member' },
        },
      ],
    },
  },
];

const templates: readonly Schemas['Template'][] = [
  {
    kind: 'Template',
    metadata: { name: 'needs-human' },
    spec: {
      bodies: {
        github:
          '🙋 The factory needs a person here.\n\n- **What blocked:** the `{{ .run.spec }}` run ended `{{ .run.outcome }}`{{ with .run.result.summary }}: {{ . }}{{ end }}\n- **Last run:** [{{ .run.id }}]({{ .links.ui }})\n',
        slack: { blocks: 'templates/needs-human.blocks.json' },
      },
    },
  },
];

/** Every object of the mock catalog, by kind and name. */
export const catalogObjects: readonly CatalogObject[] = [
  ...providerViews.map((v) => v.provider),
  ...groups,
  ...templates,
  ...ruleSets,
  ...rules,
  ...taskSpecs,
  ...queueViews.map((v) => v.queue),
  ...poolViews.map((v) => v.pool),
];

/** The example's summary, listing every mock object. */
export const catalogSummary: Schemas['CatalogSummary'] = {
  ...exampleSummary,
  objects: catalogObjects.map((o) =>
    o.metadata.labels
      ? { kind: o.kind, name: o.metadata.name, labels: o.metadata.labels }
      : { kind: o.kind, name: o.metadata.name },
  ),
};

const exampleFile = examples.get('getCatalogFile') as Schemas['CatalogFile'];

async function digest(text: string): Promise<string> {
  const sum = await crypto.subtle.digest(
    'SHA-256',
    new TextEncoder().encode(text),
  );
  return `sha256:${[...new Uint8Array(sum)].map((b) => b.toString(16).padStart(2, '0')).join('')}`;
}

/** The mock catalog's files: the example's triage prompt, an implement prompt and two schemas. */
export const catalogFiles: readonly Omit<Schemas['CatalogFile'], 'digest'>[] = [
  exampleFile,
  {
    path: 'prompts/implement.md.tmpl',
    kind: 'prompt',
    content: [
      '{{- /* implement: one issue as a PR, or the review fixes on one. */ -}}',
      '# Task: {{ .task.spec }} for {{ .task.target.repo }}',
      '',
      '## Goal',
      '',
      '{{ if eq .inputs.mode "new" -}}',
      'Implement {{ with .inputs.issue }}{{ printf "%q" . }}{{ else }}(none given){{ end }} on a new branch.',
      '{{- else -}}',
      'Address every unresolved review thread on the pull request, and push the fixes.',
      '{{- end }}',
      '',
      '## Inputs',
      '',
      '- mode: {{ printf "%q" .inputs.mode }}',
      '- base: {{ printf "%q" .inputs.base }}',
      '',
    ].join('\n'),
  },
  {
    path: 'schemas/results/implement.json',
    kind: 'schema',
    content: `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "allOf": [{ "$ref": "https://factory.ccf.dev/schemas/v1alpha1/result.schema.json" }],
  "properties": {
    "outcome": { "enum": ["pr-opened", "pr-updated", "blocked"] },
    "data": {
      "type": "object",
      "properties": { "pr": { "type": "string" }, "branch": { "type": "string", "minLength": 1 } }
    }
  }
}
`,
  },
  {
    path: 'schemas/results/triage.json',
    kind: 'schema',
    content: `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "allOf": [{ "$ref": "https://factory.ccf.dev/schemas/v1alpha1/result.schema.json" }],
  "properties": {
    "outcome": { "enum": ["triaged", "needs-info", "duplicate", "not-actionable", "promote"] },
    "data": { "type": "object", "properties": { "labels": { "type": "array", "items": { "type": "string" } } } }
  }
}
`,
  },
];

/** The mock work item whose events the dry-run panel offers first. */
export const recordedWorkItem = workItemDetails[0]!;

function notFound(detail: string): Response {
  return HttpResponse.json<Schemas['Problem']>(
    {
      type: 'about:blank',
      title: 'Not Found',
      status: 404,
      code: 'not-found',
      detail,
    },
    {
      status: 404,
      headers: { 'Content-Type': 'application/problem+json' },
    },
  );
}

function badRequest(detail: string, field: string): Response {
  return HttpResponse.json<Schemas['Problem']>(
    {
      type: 'about:blank',
      title: 'Bad Request',
      status: 400,
      code: 'invalid',
      detail,
      errors: [{ field, message: detail }],
    },
    {
      status: 400,
      headers: { 'Content-Type': 'application/problem+json' },
    },
  );
}

const exampleEvaluation = examples.get(
  'dryRunCatalog',
) as Schemas['Evaluation'];

/**
 * `GET /catalog`, `GET /providers`, `GET /catalog/{kind}/{name}` and the catalog files over the
 * fixtures above (404 for an unknown object or file, 400 for a path outside prompts/ and
 * schemas/), and `POST /catalog/dry-run` answering the example Evaluation for the
 * posted event (400 without one). Registered before the example handlers, which they override.
 */
export const catalogHandlers: HttpHandler[] = [
  http.get(`${API_BASE}/catalog`, () =>
    HttpResponse.json<Schemas['CatalogSummary']>(catalogSummary),
  ),
  http.get(`${API_BASE}/providers`, () =>
    HttpResponse.json<Schemas['ProviderList']>({ items: [...providerViews] }),
  ),
  http.post(`${API_BASE}/catalog/dry-run`, async ({ request }) => {
    const body = (await request.json().catch(() => null)) as {
      event?: { id?: unknown };
      workItem?: unknown;
    } | null;
    const id = body?.event?.id;
    if (typeof id !== 'string' || !id) {
      return badRequest('event.id: required', 'event.id');
    }
    const evaluation: Schemas['Evaluation'] = {
      ...exampleEvaluation,
      event: id,
    };
    if (typeof body?.workItem === 'string' && body.workItem) {
      evaluation.workItem = body.workItem;
    } else {
      delete evaluation.workItem;
    }
    return HttpResponse.json<Schemas['Evaluation']>(evaluation);
  }),
  http.get(`${API_BASE}/catalog/files`, () =>
    HttpResponse.json<Schemas['CatalogFileList']>({
      items: catalogFiles
        .map(({ path, kind }) => ({ path, kind }))
        .sort((a, b) => a.path.localeCompare(b.path)),
    }),
  ),
  http.get(`${API_BASE}/catalog/file`, async ({ request }) => {
    const path = new URL(request.url).searchParams.get('path') ?? '';
    if (!/^(prompts|schemas)\/./.test(path) || path.includes('..')) {
      return badRequest(
        'path must be a file under prompts/ or schemas/',
        'path',
      );
    }
    const file = catalogFiles.find((f) => f.path === path);
    if (!file) return notFound(`no file "${path}" in the catalog`);
    return HttpResponse.json<Schemas['CatalogFile']>({
      ...file,
      digest:
        file === exampleFile ? exampleFile.digest : await digest(file.content),
    });
  }),
  http.get<{ kind: string; name: string }>(
    `${API_BASE}/catalog/:kind/:name`,
    ({ params }) => {
      const object = catalogObjects.find(
        (o) => o.kind === params.kind && o.metadata.name === params.name,
      );
      return object
        ? HttpResponse.json<CatalogObject>(object)
        : notFound(`no ${params.kind} ${params.name} in the catalog`);
    },
  ),
];
