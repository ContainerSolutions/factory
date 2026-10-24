import { http, HttpResponse, type HttpHandler } from 'msw';
import { API_BASE, type Schemas } from '@/api/client';
import { examples } from './examples';

type WorkItem = Schemas['WorkItem'];
type Detail = Schemas['WorkItemDetail'];

// The contract's examples record the triage decision's actions as bare kinds; the engine records
// each as its summary (rules.ActionIntent.String()), so the mocks do too.
const EXAMPLE_ACTIONS: Record<string, string> = {
  dispatch: 'dispatch triage → triage',
  set: 'set triage',
};

/** The contract example's work item, its decisions' actions as the engine records them. */
export function summarised(workItem: WorkItem): WorkItem {
  const status = workItem.status;
  if (!status?.decisions) return workItem;
  return {
    ...workItem,
    status: {
      ...status,
      decisions: status.decisions.map((d) => ({
        ...d,
        actions: d.actions?.map((a) => EXAMPLE_ACTIONS[a] ?? a),
      })),
    },
  };
}

// The contract's own example (a running triage on compliance-framework/docs#42), summarised.
const contractExample = examples.get('getWorkItem') as Detail;
export const example: Detail = {
  ...contractExample,
  workItem: summarised(contractExample.workItem),
};
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
      facts: facts,
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
    // Long, unbroken values (a ref, a JSON state value) must stay inside their boxes.
    {
      provider: 'github-public',
      ref: 'compliance-framework/api@factory/wi_01JAC0ESCALATED0000000000000/implement-review-feedback-round-3-oscal-profile-flag-visibility',
      kind: 'branch',
    },
  ],
  {
    state: {
      stackNext: 1,
      stackTotal: 2,
      review: 'needs-human',
      lastReview: {
        round: 3,
        reviewer: 'ccf-factory-reviewer[bot]',
        verdict: 'changes_requested',
        blocking: [
          'cmd/oscal/profile.go:118:the --profile flag is exported but undocumented',
          'internal/oscal/resolve.go:42:the resolver swallows ErrProfileNotFound',
        ],
        url: 'https://github.com/compliance-framework/api/pull/310#pullrequestreview-2468013579',
      },
    },
    counters: { implement: 3, 'docs-review': 3 },
    decisions: [
      {
        at: '2026-10-17T14:02:01Z',
        event: escalatedEvent.id,
        ruleSet: 'api',
        rule: 'implement-review-feedback',
        fired: true,
        inputs: {
          'event.review.state': 'changes_requested',
          "item.count('implement')": 3,
          'params.maxImplementRuns': 3,
        },
        rails: [
          {
            rail: 'run-cap',
            verdict: 'suppressed',
            reason: 'implement ran 3 of 3 times',
          },
        ],
        actions: ['dispatch implement → default'],
      },
      {
        at: '2026-10-17T14:02:01Z',
        event: escalatedEvent.id,
        ruleSet: 'api',
        rule: 'escalate-needs-human',
        fired: true,
        inputs: {
          'run.spec': 'docs-review',
          'run.outcome': 'needs-human',
        },
        rails: [{ rail: 'effects-allowed', verdict: 'allowed' }],
        actions: ['effect label.add', 'notify needs-human'],
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
          data: {
            pr: 'compliance-framework/api#310',
            branch:
              'factory/wi_01JAC0ESCALATED0000000000000/implement-review-feedback-round-3-oscal-profile-flag-visibility',
          },
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
    state: { stackNext: 0, stackTotal: 3 },
    counters: { 'plan-epic': 1 },
    approvals: [{ name: 'implement', phase: 'Pending' }],
    decisions: [
      {
        at: '2026-10-16T12:00:01Z',
        event: 'github-public/gh-epic-opened-57',
        ruleSet: 'docs-epic',
        rule: 'plan-epic',
        fired: true,
        inputs: { 'event.labels': ['epic'] },
        rails: [{ rail: 'run-cap', verdict: 'allowed' }],
        actions: ['dispatch plan-epic → default', 'set stackTotal'],
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
    state: { triage: 'needs-info' },
    counters: { triage: 1 },
    decisions: [
      {
        at: '2026-10-14T10:00:02Z',
        event: 'github-public/gh-ui-issue-88',
        ruleSet: 'ui',
        rule: 'triage-outside-issues',
        fired: true,
        inputs: { 'actor.tier': 'contributor' },
        rails: [{ rail: 'run-cap', verdict: 'allowed' }],
        actions: ['dispatch triage → default', 'set triage'],
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
    state: {
      release: 'drafting',
      train: 'platform/release-notes@v1.9.0',
      manifest: 'repos.yaml',
    },
    counters: { 'release-docs': 1 },
    decisions: [
      {
        at: '2026-10-18T09:00:01Z',
        event: 'github-enterprise/ghe-release-1.9.0',
        ruleSet: 'release-docs',
        rule: 'draft-release-docs',
        fired: true,
        inputs: { 'event.release.tag': 'v1.9.0' },
        rails: [
          {
            rail: 'budget',
            verdict: 'deferred',
            reason: 'queue default is at its daily budget',
          },
        ],
        actions: ['dispatch release-docs → default', 'set release'],
      },
    ],
  },
);

// The deferred dispatch went out once the budget allowed: a run still going at the fixtures'
// "now", whose progress the mock stream (src/mocks/stream.ts) keeps sending.
const releaseTask: Schemas['Task'] = {
  ...exampleTask,
  metadata: {
    ...exampleTask.metadata,
    id: 'tsk_01JAC0RELEASEDRAFT000000000',
    workItem: release.metadata.name,
    provenance: {
      event: 'github-enterprise/ghe-release-1.9.0',
      ruleSet: 'release-docs',
      rule: 'draft-release-docs',
      catalogSHA: CATALOG_SHA,
    },
    createdAt: '2026-10-19T11:55:00Z',
  },
  spec: {
    ...exampleTask.spec,
    taskSpec: { name: 'release-docs' },
    queue: 'default',
    provider: 'github-enterprise',
    target: { provider: 'github-enterprise', repo: 'platform/release-notes' },
  },
  status: { phase: 'Running', agent: 'agt_gusfcarvalho-mbp-01', attempts: 1 },
};

/** A run in progress: no end, Result, effects or transcript yet. */
export const liveRun: Schemas['Run'] = {
  kind: 'Run',
  metadata: {
    name: 'run_01JAC0RELEASEDRAFT00000000',
    createdAt: '2026-10-19T11:55:30Z',
  },
  spec: {
    task: releaseTask.metadata.id,
    attempt: 1,
    agent: 'agt_gusfcarvalho-mbp-01',
    owner: 'github-public:gusfcarvalho',
  },
  status: {
    startedAt: '2026-10-19T11:55:30Z',
    progress: [
      {
        at: '2026-10-19T11:55:40Z',
        kind: 'phase',
        message: 'cloning platform/release-notes',
      },
      {
        at: '2026-10-19T11:56:10Z',
        kind: 'tool',
        message: 'read repos.yaml',
        data: { turn: 2 },
      },
    ],
    usage: { inputTokens: 18200, outputTokens: 900, turns: 4, costUSD: null },
  },
};

const releaseDetail: Detail = {
  workItem: release,
  events: [],
  tasks: [releaseTask],
  runs: [liveRun],
  effects: [],
};

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
    state: { review: 'approved', merge: 'merged' },
    counters: { triage: 1, implement: 1, 'docs-review': 1, merge: 1 },
    decisions: [
      {
        at: '2026-10-17T16:00:00Z',
        event: 'github-public/gh-docs-41-approved',
        ruleSet: 'docs',
        rule: 'merge-approved',
        fired: true,
        inputs: { 'item.pr.checks.state': 'success' },
        rails: [{ rail: 'run-cap', verdict: 'allowed' }],
        actions: ['dispatch merge → default'],
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
    state: { merge: 'merged' },
    counters: { merge: 1 },
    decisions: [
      {
        at: '2026-09-02T08:00:00Z',
        event: 'github-public/gh-docs-12',
        ruleSet: 'docs',
        rule: 'merge-approved',
        fired: true,
        rails: [{ rail: 'run-cap', verdict: 'allowed' }],
        actions: ['dispatch merge → default'],
      },
    ],
  },
);

// A docs issue the factory triaged, implemented as a PR and is now reviewing: every link the
// work-item graph draws (dispatch → run → factory.run.completed, and an inferred PR event).
const PIPELINE = 'wi_01JAC0PIPELINE0000000000000';
const pipelineOpened = event(
  'gh-docs-77-opened',
  'issue.opened',
  '2026-10-18T08:00:00Z',
  'compliance-framework/docs#77',
  'octocat',
  { title: 'Document the OSCAL --profile flag' },
);
pipelineOpened.data.actor = {
  id: 'github-public:octocat',
  tier: 'contributor',
};
pipelineOpened.data.object = {
  provider: 'github-public',
  kind: 'issue',
  ref: 'compliance-framework/docs#77',
};
const pipelinePROpened = event(
  'gh-docs-78-opened',
  'pr.opened',
  '2026-10-18T08:31:00Z',
  'compliance-framework/docs#78',
  'ccf-factory',
  { title: 'docs: the OSCAL --profile flag' },
);
pipelinePROpened.data.actor = {
  id: 'github-public:ccf-factory[bot]',
  tier: 'bot',
  isBot: true,
  isFactory: true,
};
// The factory's marker on docs#78 names the pr.open effect of the implement task (below).
pipelinePROpened.data.effect = 'tsk_01JAC0PIPELINEIMPLEMENT0000/pr.open/1';
// The rule that labels factory PRs did it on docs#78: GitHub's pr.labeled names no effect.
const pipelinePRLabeled = event(
  'gh-docs-78-labeled',
  'pr.labeled',
  '2026-10-18T08:31:03Z',
  'compliance-framework/docs#78',
  'ccf-factory',
  { label: 'factory' },
);
pipelinePRLabeled.data.actor = pipelinePROpened.data.actor;
// The implement-promoted rule's notify (its second action) commented on docs#77: the marker
// names the rule controller's key, evt/<event>/<rule>/<i>, plus the request's index.
const pipelineNotified = event(
  'gh-docs-77-notified',
  'issue.commented',
  '2026-10-18T08:06:03Z',
  'compliance-framework/docs#77',
  'ccf-factory',
);
pipelineNotified.data.actor = pipelinePROpened.data.actor;
pipelineNotified.data.object = pipelineOpened.data.object;
pipelineNotified.data.effect =
  'evt/factory/run-completed-triage-77/implement-promoted/1/0';

function runCompleted(
  id: string,
  time: string,
  run: string,
  spec: string,
  outcome: string,
): Schemas['Event'] {
  return {
    kind: 'Event',
    id: `factory/${id}`,
    source: 'factory',
    type: 'factory.run.completed',
    subject: run,
    time,
    data: {
      provider: 'factory',
      object: {
        provider: 'github-public',
        kind: 'issue',
        ref: 'compliance-framework/docs#77',
      },
      facts: { run: { id: run, spec, outcome, status: 'done' } },
    },
  };
}

function pipelineTask(
  id: string,
  createdAt: string,
  provenance: { event: string; rule: string },
  taskSpec: string,
  inputs: Record<string, unknown>,
  status: Schemas['Task']['status'],
): Schemas['Task'] {
  return {
    ...exampleTask,
    metadata: {
      ...exampleTask.metadata,
      id,
      workItem: PIPELINE,
      provenance: { ...provenance, ruleSet: 'docs', catalogSHA: CATALOG_SHA },
      createdAt,
    },
    spec: {
      ...exampleTask.spec,
      taskSpec: { name: taskSpec },
      queue: 'default',
      inputs,
    },
    status,
  };
}

function pipelineRun(
  name: string,
  task: string,
  attempt: number,
  startedAt: string,
  endedAt: string | undefined,
  end: NonNullable<Schemas['Run']['status']>['end'],
  result?: Partial<Schemas['Result']>,
): Schemas['Run'] {
  return {
    kind: 'Run',
    metadata: { name, createdAt: startedAt },
    spec: {
      task,
      attempt,
      agent: 'agt_gusfcarvalho-mbp-01',
      owner: 'github-public:gusfcarvalho',
    },
    status: {
      startedAt,
      ...(endedAt ? { endedAt } : {}),
      ...(end ? { end } : {}),
      ...(result
        ? {
            result: {
              ...exampleRun.status!.result!,
              artifacts: [],
              task,
              ...result,
            } as Schemas['Result'],
          }
        : {}),
      transcriptRef: endedAt ? `transcripts/${name}.jsonl.gz` : undefined,
    },
  };
}

const triageTask = pipelineTask(
  'tsk_01JAC0PIPELINETRIAGE0000000',
  '2026-10-18T08:00:01Z',
  { event: pipelineOpened.id, rule: 'triage-outside-issues' },
  'triage',
  { issue: 'compliance-framework/docs#77' },
  { phase: 'Succeeded', agent: 'agt_gusfcarvalho-mbp-01', attempts: 1 },
);
const triageRun = pipelineRun(
  'run_01JAC0PIPELINETRIAGE000000',
  triageTask.metadata.id,
  1,
  '2026-10-18T08:00:30Z',
  '2026-10-18T08:05:58Z',
  'completed',
  {
    outcome: 'promote',
    summary: 'A small, well-scoped docs change: promote it to implement.',
  },
);
const triageDone = runCompleted(
  'run-completed-triage-77',
  '2026-10-18T08:06:00Z',
  triageRun.metadata.name,
  'triage',
  'promote',
);
const implementTask = pipelineTask(
  'tsk_01JAC0PIPELINEIMPLEMENT0000',
  '2026-10-18T08:06:01Z',
  { event: triageDone.id, rule: 'implement-promoted' },
  'implement',
  { mode: 'new', target: 'pr', issue: 'compliance-framework/docs#77' },
  { phase: 'Succeeded', agent: 'agt_gusfcarvalho-mbp-01', attempts: 2 },
);
const implementFailed = pipelineRun(
  'run_01JAC0PIPELINEIMPLEMENT01',
  implementTask.metadata.id,
  1,
  '2026-10-18T08:06:30Z',
  '2026-10-18T08:10:00Z',
  'failed',
);
const implementRun = pipelineRun(
  'run_01JAC0PIPELINEIMPLEMENT02',
  implementTask.metadata.id,
  2,
  '2026-10-18T08:11:00Z',
  '2026-10-18T08:31:20Z',
  'completed',
  {
    outcome: 'pr-opened',
    summary: 'Opened compliance-framework/docs#78 documenting --profile.',
    data: {
      pr: 'compliance-framework/docs#78',
      branch: `factory/${PIPELINE}/0`,
    },
    artifacts: [
      {
        kind: 'pr',
        url: 'https://github.com/compliance-framework/docs/pull/78',
      },
    ],
  },
);
const implementDone = runCompleted(
  'run-completed-implement-77',
  '2026-10-18T08:31:30Z',
  implementRun.metadata.name,
  'implement',
  'pr-opened',
);
const reviewTask = pipelineTask(
  'tsk_01JAC0PIPELINEREVIEW0000000',
  '2026-10-18T08:31:01Z',
  { event: pipelinePROpened.id, rule: 'review-factory-prs' },
  'docs-review',
  { mode: 'review', target: 'pr' },
  {
    phase: 'Pending',
    agent: null,
    attempts: 0,
    reason: 'anti-affinity: waiting for an agent other than the author',
  },
);

const pipeline = item(
  PIPELINE,
  '2026-10-18T08:00:00Z',
  [
    {
      provider: 'github-public',
      ref: 'compliance-framework/docs#77',
      kind: 'issue',
      primary: true,
    },
    {
      provider: 'github-public',
      ref: 'compliance-framework/docs#78',
      kind: 'pr',
    },
  ],
  {
    state: { triage: 'promoted', review: 'running' },
    counters: { triage: 1, implement: 2 },
    decisions: [
      {
        at: '2026-10-18T08:00:01Z',
        event: pipelineOpened.id,
        ruleSet: 'docs',
        rule: 'triage-outside-issues',
        fired: true,
        inputs: { 'actor.tier': 'contributor', 'actor.isBot': false },
        rails: [{ rail: 'run-cap', verdict: 'allowed' }],
        actions: ['dispatch triage → default', 'set triage'],
      },
      {
        at: '2026-10-18T08:00:01Z',
        event: pipelineOpened.id,
        ruleSet: 'docs',
        rule: 'docs/label-docs',
        fired: true,
        rails: [{ rail: 'effects-allowed', verdict: 'allowed' }],
        actions: ['effect label.add'],
      },
      {
        at: '2026-10-18T08:00:01Z',
        event: pipelineOpened.id,
        ruleSet: 'docs',
        rule: 'triage-maintainer-issues',
        fired: false,
        inputs: { 'actor.tier': 'contributor' },
        reason: 'where is false',
      },
      {
        at: '2026-10-18T08:06:01Z',
        event: triageDone.id,
        ruleSet: 'docs',
        rule: 'implement-promoted',
        fired: true,
        inputs: { 'run.spec': 'triage', 'run.outcome': 'promote' },
        rails: [{ rail: 'run-cap', verdict: 'allowed' }],
        actions: ['dispatch implement → default', 'notify implement-started'],
      },
      {
        at: '2026-10-18T08:06:01Z',
        event: triageDone.id,
        ruleSet: 'docs',
        rule: 'escalate-needs-human',
        fired: false,
        inputs: { 'run.outcome': 'promote' },
        reason: 'where is false',
      },
      {
        at: '2026-10-18T08:31:01Z',
        event: pipelinePROpened.id,
        ruleSet: 'docs',
        rule: 'review-factory-prs',
        fired: true,
        inputs: { 'actor.isFactory': true },
        rails: [{ rail: 'run-cap', verdict: 'allowed' }],
        actions: ['dispatch docs-review → default'],
      },
      {
        at: '2026-10-18T08:31:01Z',
        event: pipelinePROpened.id,
        ruleSet: 'api',
        rule: 'review-factory-prs',
        fired: false,
        reason: 'already fired through RuleSet docs',
      },
      {
        at: '2026-10-18T08:31:01Z',
        event: pipelinePROpened.id,
        ruleSet: 'docs',
        rule: 'label-factory-prs',
        fired: true,
        inputs: { 'actor.isFactory': true },
        rails: [{ rail: 'effects-allowed', verdict: 'allowed' }],
        actions: ['effect label.add'],
      },
    ],
    reporter: { id: 'github-public:octocat', tier: 'contributor' },
  },
);

const pipelineDetail: Detail = {
  workItem: pipeline,
  events: [
    pipelineOpened,
    triageDone,
    pipelineNotified,
    pipelinePROpened,
    pipelinePRLabeled,
    implementDone,
  ],
  tasks: [triageTask, implementTask, reviewTask],
  runs: [triageRun, implementFailed, implementRun],
  effects: [
    {
      kind: 'Effect',
      metadata: {
        name: 'eff_01JAC0PIPELINELABEL0000000',
        createdAt: '2026-10-18T08:00:02Z',
        labels: { 'factory.ccf.dev/rule': 'docs/label-docs' },
      },
      spec: {
        // A rule's own effect has no task (the field is empty).
        task: '',
        op: 'label.add',
        target: {
          provider: 'github-public',
          kind: 'issue',
          ref: 'compliance-framework/docs#77',
        },
        idempotencyKey: `evt/${pipelineOpened.id}/docs/label-docs/0`,
        identity: 'triager',
        request: { labels: ['area/docs'] },
      },
      status: { at: '2026-10-18T08:00:02Z', result: {} },
    },
    {
      kind: 'Effect',
      metadata: {
        name: 'eff_01JAC0PIPELINELABELPR00000',
        createdAt: '2026-10-18T08:31:02Z',
        labels: { 'factory.ccf.dev/rule': 'label-factory-prs' },
      },
      spec: {
        task: '',
        op: 'label.add',
        target: {
          provider: 'github-public',
          kind: 'pr',
          ref: 'compliance-framework/docs#78',
        },
        idempotencyKey: `evt/${pipelinePROpened.id}/label-factory-prs/0`,
        identity: 'author',
        request: { labels: ['factory'] },
      },
      status: { at: '2026-10-18T08:31:02Z', result: {} },
    },
    {
      kind: 'Effect',
      metadata: {
        name: 'eff_01JAC0PIPELINEPROPEN000000',
        createdAt: '2026-10-18T08:30:50Z',
      },
      spec: {
        task: implementTask.metadata.id,
        op: 'pr.open',
        target: {
          provider: 'github-public',
          kind: 'pr',
          ref: 'compliance-framework/docs#78',
        },
        idempotencyKey: `${implementTask.metadata.id}/pr.open/1`,
        identity: 'author',
      },
      status: {
        at: '2026-10-18T08:30:55Z',
        result: { url: 'https://github.com/compliance-framework/docs/pull/78' },
      },
    },
  ],
};

// An epic, as wi_d235e0c50568b22418e5 ran: issue.opened planned it and set the plan; the
// factory's tracking comment followed 4 s later; the plan-epic run's completion set the plan and
// the stack and dispatched implement, which waits for an agent.
const EPIC = 'wi_d235e0c50568b22418e5';
const epicOpened = event(
  'gh-docs-91-opened',
  'issue.opened',
  '2026-10-19T09:00:00Z',
  'compliance-framework/docs#91',
  'gusfcarvalho',
  { title: 'Epic: document the OSCAL profile resolver', labels: ['epic'] },
);
epicOpened.data.object = {
  provider: 'github-public',
  kind: 'issue',
  ref: 'compliance-framework/docs#91',
};
// The work-item controller's projection (<!-- factory:wi … -->): a factory comment, no effect.
const epicTracking = event(
  'gh-docs-91-tracking',
  'issue.commented',
  '2026-10-19T09:00:04Z',
  'compliance-framework/docs#91',
  'ccf-factory',
);
epicTracking.data.actor = pipelinePROpened.data.actor;
epicTracking.data.object = epicOpened.data.object;
// A maintainer labelled the issue: no rule fired on it, so the graph leaves it out.
const epicLabeled = event(
  'gh-docs-91-labeled',
  'issue.labeled',
  '2026-10-19T09:02:00Z',
  'compliance-framework/docs#91',
  'gusfcarvalho',
  { label: 'area/docs' },
);
epicLabeled.data.object = epicOpened.data.object;
const epicPlanTask: Schemas['Task'] = {
  ...exampleTask,
  metadata: {
    ...exampleTask.metadata,
    id: 'tsk_d235e0c50568PLANEPIC0000',
    workItem: EPIC,
    provenance: {
      event: epicOpened.id,
      ruleSet: 'docs-epic',
      rule: 'plan-epic',
      catalogSHA: CATALOG_SHA,
    },
    createdAt: '2026-10-19T09:00:01Z',
  },
  spec: {
    ...exampleTask.spec,
    taskSpec: { name: 'plan-epic' },
    queue: 'poc',
    inputs: { issue: 'compliance-framework/docs#91' },
  },
  status: { phase: 'Succeeded', agent: 'agt_gusfcarvalho-mbp-01', attempts: 1 },
};
const epicPlanRun: Schemas['Run'] = {
  kind: 'Run',
  metadata: {
    name: 'run_d235e0c50568PLANEPIC000',
    createdAt: '2026-10-19T09:00:30Z',
  },
  spec: {
    task: epicPlanTask.metadata.id,
    attempt: 1,
    agent: 'agt_gusfcarvalho-mbp-01',
    owner: 'github-public:gusfcarvalho',
  },
  status: {
    startedAt: '2026-10-19T09:00:30Z',
    endedAt: '2026-10-19T09:09:50Z',
    end: 'completed',
    result: {
      ...exampleRun.status!.result!,
      artifacts: [],
      task: epicPlanTask.metadata.id,
      outcome: 'planned',
      summary: 'Split the epic into a stack of three PRs.',
    } as Schemas['Result'],
    transcriptRef: 'transcripts/run_d235e0c50568PLANEPIC000.jsonl.gz',
  },
};
const epicPlanned: Schemas['Event'] = {
  kind: 'Event',
  id: 'factory/run-completed-plan-epic-91',
  source: 'factory',
  type: 'factory.run.completed',
  subject: epicPlanRun.metadata.name,
  time: '2026-10-19T09:10:00Z',
  data: {
    provider: 'factory',
    object: epicOpened.data.object,
    facts: {
      run: {
        id: epicPlanRun.metadata.name,
        spec: 'plan-epic',
        outcome: 'planned',
        status: 'done',
      },
    },
  },
};
const epicImplementTask: Schemas['Task'] = {
  ...epicPlanTask,
  metadata: {
    ...epicPlanTask.metadata,
    id: 'tsk_d235e0c50568IMPLEMENT000',
    provenance: {
      event: epicPlanned.id,
      ruleSet: 'docs-epic',
      rule: 'implement-plan',
      catalogSHA: CATALOG_SHA,
    },
    createdAt: '2026-10-19T09:10:01Z',
  },
  spec: {
    ...epicPlanTask.spec,
    taskSpec: { name: 'implement' },
    inputs: {
      mode: 'new',
      target: 'pr',
      issue: 'compliance-framework/docs#91',
    },
  },
  status: {
    phase: 'Pending',
    agent: null,
    attempts: 0,
    reason:
      'no-capable-agent: no agent in pool maintainer-agents lends queue poc',
  },
};
const epicDetail: Detail = {
  workItem: item(
    EPIC,
    '2026-10-19T09:00:00Z',
    [
      {
        provider: 'github-public',
        ref: 'compliance-framework/docs#91',
        kind: 'issue',
        primary: true,
      },
    ],
    {
      state: { plan: 'planned', stackNext: 0, stackTotal: 3 },
      counters: { 'plan-epic': 1 },
      decisions: [
        {
          at: '2026-10-19T09:00:01Z',
          event: epicOpened.id,
          ruleSet: 'docs-epic',
          rule: 'plan-epic',
          fired: true,
          inputs: { 'event.labels': ['epic'] },
          rails: [{ rail: 'run-cap', verdict: 'allowed' }],
          actions: ['dispatch plan-epic → poc', 'set plan'],
        },
        {
          at: '2026-10-19T09:02:01Z',
          event: epicLabeled.id,
          ruleSet: 'docs-epic',
          rule: 'plan-epic',
          fired: false,
          inputs: { 'event.labels': ['area/docs'] },
          reason: 'where is false',
        },
        {
          at: '2026-10-19T09:10:01Z',
          event: epicPlanned.id,
          ruleSet: 'docs-epic',
          rule: 'implement-plan',
          fired: true,
          inputs: { 'run.spec': 'plan-epic', 'run.outcome': 'planned' },
          rails: [{ rail: 'run-cap', verdict: 'allowed' }],
          actions: [
            'set plan',
            'set stackNext',
            'set stackTotal',
            'dispatch implement → poc',
          ],
        },
      ],
      reporter: { id: 'github-public:gusfcarvalho', tier: 'maintainer' },
    },
  ),
  events: [epicOpened, epicTracking, epicLabeled, epicPlanned],
  tasks: [epicPlanTask, epicImplementTask],
  runs: [epicPlanRun],
  effects: [],
};

function bare(workItem: WorkItem): Detail {
  return { workItem, events: [], tasks: [], runs: [], effects: [] };
}

/** The mock work items and their timelines, most recently updated first. */
export const workItemDetails: readonly Detail[] = [
  epicDetail,
  releaseDetail,
  escalatedDetail,
  pipelineDetail,
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
