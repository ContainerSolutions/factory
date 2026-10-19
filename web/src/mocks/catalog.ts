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
    { pack: 'review' },
    {
      providerSelector: github,
      on: ['pr.review.submitted'],
      where: "item.count('implement') < params.maxRounds",
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
    harness: {
      default: { name: 'claude-code', config: { model: 'claude-sonnet-5-*' } },
    },
    effects: { allow: ['push-branch', 'pr.comment'] },
    limits: { maxTurns: 120, maxWallClock: '1h' },
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

/** Every object of the mock catalog, by kind and name. */
export const catalogObjects: readonly CatalogObject[] = [
  ...providerViews.map((v) => v.provider),
  ...groups,
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
 * `GET /catalog`, `GET /providers` and `GET /catalog/{kind}/{name}` over the fixtures above (404
 * for an unknown object), and `POST /catalog/dry-run` answering the example Evaluation for the
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
