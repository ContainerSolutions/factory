import { http, HttpResponse, type HttpHandler } from 'msw';
import { API_BASE, type Schemas } from '@/api/client';
import { examples } from './examples';

type WorkItem = Schemas['WorkItem'];
type Detail = Schemas['WorkItemDetail'];

/**
 * A free-form object for a field the contract types as `type: object` with no
 * `additionalProperties` (WorkItemStatus.state, Decision.inputs, EventData.facts): openapi-typescript
 * makes those `Record<string, never>`, so no value type-checks without this one cast.
 */
export function freeform(
  value: Record<string, unknown>,
): Record<string, never> {
  return value as Record<string, never>;
}

// The contract's own example: a running triage on compliance-framework/docs#42.
const example = examples.get('getWorkItem') as Detail;
const exampleTask = example.tasks[0]!;
const exampleRun = example.runs[0]!;
const exampleEffect = example.effects[0]!;
const CATALOG_SHA = exampleTask.metadata.provenance!.catalogSHA;

function item(
  name: string,
  createdAt: string,
  links: Schemas['Link'][],
  status: NonNullable<WorkItem['status']>,
): WorkItem {
  return {
    kind: 'WorkItem',
    metadata: { name, resourceVersion: '1', createdAt },
    spec: { links },
    status,
  };
}

function event(
  id: string,
  type: Schemas['EventType'],
  time: string,
  subject: string,
  actor: string,
  facts: Record<string, unknown> = {},
): Schemas['Event'] {
  const [owner, repo] = subject.split(/[#@]/)[0]!.split('/');
  return {
    kind: 'Event',
    id: `github-public/${id}`,
    source: 'github-public',
    type,
    subject,
    time,
    data: {
      provider: 'github-public',
      actor: { id: `github-public:${actor}`, tier: 'maintainer' },
      object: { provider: 'github-public', kind: 'pr', ref: subject },
      repo: `${owner}/${repo}`,
      facts: freeform(facts),
    },
  };
}

// An api PR escalated to a human after three review rounds, with a Slack thread linked.
const escalatedEvent = event(
  'gh-escalated-review-3',
  'pr.review.submitted',
  '2026-10-17T14:02:00Z',
  'compliance-framework/api#310',
  'ccf-factory-reviewer',
  { 'review.state': 'changes_requested' },
);
const escalated = item(
  'wi_01JAC0ESCALATED0000000000000',
  '2026-10-15T08:00:00Z',
  [
    {
      provider: 'github-public',
      ref: 'compliance-framework/api#310',
      kind: 'pr',
      primary: true,
    },
    {
      provider: 'slack-ccf',
      ref: '#ccf-api/1729070000.000100',
      kind: 'thread',
    },
  ],
  {
    state: freeform({ stackNext: 1, stackTotal: 2, review: 'needs-human' }),
    counters: { implement: 3, 'docs-review': 3 },
    decisions: [
      {
        at: '2026-10-17T14:02:01Z',
        event: escalatedEvent.id,
        ruleSet: 'api',
        rule: 'implement-review-feedback',
        fired: true,
        inputs: freeform({
          'event.review.state': 'changes_requested',
          "item.count('implement')": 3,
          'params.maxImplementRuns': 3,
        }),
        rails: [
          {
            rail: 'run-cap',
            verdict: 'Suppressed',
            reason: 'implement ran 3 of 3 times',
          },
        ],
        actions: ['dispatch'],
      },
      {
        at: '2026-10-17T14:02:01Z',
        event: escalatedEvent.id,
        ruleSet: 'api',
        rule: 'escalate-needs-human',
        fired: true,
        inputs: freeform({
          'run.spec': 'docs-review',
          'run.outcome': 'needs-human',
        }),
        rails: [{ rail: 'effects-allowed', verdict: 'Allowed' }],
        actions: ['effect', 'notify'],
      },
    ],
    reporter: { id: 'github-public:gusfcarvalho', tier: 'maintainer' },
  },
);
const escalatedTask: Schemas['Task'] = {
  ...exampleTask,
  metadata: {
    ...exampleTask.metadata,
    id: 'tsk_01JAC0ESCALATEDREVIEW000000',
    workItem: escalated.metadata.name,
    provenance: {
      event: 'github-public/gh-escalated-review-2',
      ruleSet: 'api',
      rule: 'review-factory-prs',
      catalogSHA: CATALOG_SHA,
    },
    createdAt: '2026-10-17T11:00:00Z',
  },
  spec: {
    ...exampleTask.spec,
    taskSpec: { name: 'docs-review' },
    queue: 'default',
  },
  status: { phase: 'Succeeded', agent: 'agt_gusfcarvalho-mbp-01', attempts: 1 },
};
const escalatedDetail: Detail = {
  workItem: escalated,
  events: [escalatedEvent],
  tasks: [escalatedTask],
  runs: [
    {
      ...exampleRun,
      metadata: {
        name: 'run_01JAC0ESCALATEDREVIEW00000',
        createdAt: '2026-10-17T11:00:30Z',
      },
      spec: { ...exampleRun.spec, task: escalatedTask.metadata.id },
      status: {
        startedAt: '2026-10-17T11:00:30Z',
        endedAt: '2026-10-17T11:20:00Z',
        end: 'completed',
        result: {
          ...exampleRun.status!.result!,
          task: escalatedTask.metadata.id,
          outcome: 'needs-human',
          summary:
            'Third review round: whether the OSCAL profile flag is public needs a maintainer.',
        },
      },
    },
  ],
  effects: [
    {
      ...exampleEffect,
      metadata: {
        name: 'eff_01JAC0ESCALATEDLABEL0000000',
        createdAt: '2026-10-17T14:02:02Z',
      },
      spec: {
        task: escalatedTask.metadata.id,
        op: 'label.add',
        target: {
          provider: 'github-public',
          kind: 'pr',
          ref: 'compliance-framework/api#310',
        },
        idempotencyKey: `${escalatedTask.metadata.id}/label.add/1`,
        identity: 'reviewer',
      },
      status: { at: '2026-10-17T14:02:03Z', result: {} },
    },
  ],
};

// An epic plan waiting on a maintainer's approval.
const approval = item(
  'wi_01JAC0APPROVAL00000000000000',
  '2026-10-16T12:00:00Z',
  [
    {
      provider: 'github-public',
      ref: 'compliance-framework/docs#57',
      kind: 'issue',
      primary: true,
    },
  ],
  {
    state: freeform({ stackNext: 0, stackTotal: 3 }),
    counters: { 'plan-epic': 1 },
    approvals: [{ name: 'implement', phase: 'Pending' }],
    decisions: [
      {
        at: '2026-10-16T12:00:01Z',
        event: 'github-public/gh-epic-opened-57',
        ruleSet: 'docs-epic',
        rule: 'plan-epic',
        fired: true,
        inputs: freeform({ 'event.labels': ['epic'] }),
        rails: [{ rail: 'run-cap', verdict: 'Allowed' }],
        actions: ['dispatch', 'set'],
      },
    ],
  },
);

// A ui issue waiting on its reporter.
const waiting = item(
  'wi_01JAC0WAITING000000000000000',
  '2026-10-14T10:00:00Z',
  [
    {
      provider: 'github-public',
      ref: 'compliance-framework/ui#88',
      kind: 'issue',
      primary: true,
    },
  ],
  {
    state: freeform({ triage: 'needs-info' }),
    counters: { triage: 1 },
    decisions: [
      {
        at: '2026-10-14T10:00:02Z',
        event: 'github-public/gh-ui-issue-88',
        ruleSet: 'ui',
        rule: 'triage-outside-issues',
        fired: true,
        inputs: freeform({ 'actor.tier': 'contributor' }),
        rails: [{ rail: 'run-cap', verdict: 'Allowed' }],
        actions: ['dispatch', 'set'],
      },
    ],
    reporter: { id: 'github-public:octocat', tier: 'contributor' },
  },
);

// A release-docs train drafting on a second GitHub provider.
const release = item(
  'wi_01JAC0RELEASE000000000000000',
  '2026-10-18T09:00:00Z',
  [
    {
      provider: 'github-enterprise',
      ref: 'platform/release-notes#7',
      kind: 'issue',
      primary: true,
    },
  ],
  {
    state: freeform({
      release: 'drafting',
      train: 'platform/release-notes@v1.9.0',
      manifest: 'repos.yaml',
    }),
    counters: { 'release-docs': 1 },
    decisions: [
      {
        at: '2026-10-18T09:00:01Z',
        event: 'github-enterprise/ghe-release-1.9.0',
        ruleSet: 'release-docs',
        rule: 'draft-release-docs',
        fired: true,
        inputs: freeform({ 'event.release.tag': 'v1.9.0' }),
        rails: [
          {
            rail: 'budget',
            verdict: 'Deferred',
            reason: 'queue default is at its daily budget',
          },
        ],
        actions: ['dispatch', 'set'],
      },
    ],
  },
);

// Done two days before the fixtures' "now" (2026-10-19), so it shows in the Done lane.
const doneRecent = item(
  'wi_01JAC0DONERECENT000000000000',
  '2026-10-10T08:00:00Z',
  [
    {
      provider: 'github-public',
      ref: 'compliance-framework/docs#40',
      kind: 'issue',
      primary: true,
    },
    {
      provider: 'github-public',
      ref: 'compliance-framework/docs#41',
      kind: 'pr',
    },
  ],
  {
    state: freeform({ review: 'approved', merge: 'merged' }),
    counters: { triage: 1, implement: 1, 'docs-review': 1, merge: 1 },
    decisions: [
      {
        at: '2026-10-17T16:00:00Z',
        event: 'github-public/gh-docs-41-approved',
        ruleSet: 'docs',
        rule: 'merge-approved',
        fired: true,
        inputs: freeform({ 'item.pr.checks.state': 'success' }),
        rails: [{ rail: 'run-cap', verdict: 'Allowed' }],
        actions: ['dispatch'],
      },
    ],
  },
);

// Done long ago: off the board's Done lane.
const doneOld = item(
  'wi_01JAC0DONEOLD000000000000000',
  '2026-09-01T08:00:00Z',
  [
    {
      provider: 'github-public',
      ref: 'compliance-framework/docs#12',
      kind: 'issue',
      primary: true,
    },
  ],
  {
    state: freeform({ merge: 'merged' }),
    counters: { merge: 1 },
    decisions: [
      {
        at: '2026-09-02T08:00:00Z',
        event: 'github-public/gh-docs-12',
        ruleSet: 'docs',
        rule: 'merge-approved',
        fired: true,
        rails: [{ rail: 'run-cap', verdict: 'Allowed' }],
        actions: ['dispatch'],
      },
    ],
  },
);

function bare(workItem: WorkItem): Detail {
  return { workItem, events: [], tasks: [], runs: [], effects: [] };
}

/** The mock work items and their timelines, most recently updated first. */
export const workItemDetails: readonly Detail[] = [
  bare(release),
  escalatedDetail,
  bare(doneRecent),
  bare(approval),
  example,
  bare(waiting),
  bare(doneOld),
];

/**
 * `GET /workitems` and `GET /workitems/{id}` over the mock work items, so the board has every
 * lane, several providers, repos and RuleSets, and the work-item page answers per id (404 for an
 * unknown one). Registered before the example handlers, which they override.
 */
export const workItemHandlers: HttpHandler[] = [
  http.get(`${API_BASE}/workitems`, () =>
    HttpResponse.json<Schemas['WorkItemList']>({
      items: workItemDetails.map((d) => d.workItem),
    }),
  ),
  http.get<{ id: string }, never, Detail | Schemas['Problem']>(
    `${API_BASE}/workitems/:id`,
    ({ params }) => {
      const detail = workItemDetails.find(
        (d) => d.workItem.metadata.name === params.id,
      );
      if (detail) return HttpResponse.json<Detail>(detail);
      return HttpResponse.json<Schemas['Problem']>(
        {
          type: 'about:blank',
          title: 'Not Found',
          status: 404,
          code: 'not-found',
          detail: `work item ${params.id} not found`,
        },
        {
          status: 404,
          headers: { 'Content-Type': 'application/problem+json' },
        },
      );
    },
  ),
];
