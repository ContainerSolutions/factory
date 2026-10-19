import { http, HttpResponse, type HttpHandler } from 'msw';
import { API_BASE, type Schemas } from '@/api/client';
import { examples } from './examples';
import { liveRun } from './workitems';

type Agent = Schemas['Agent'];
type QueueView = Schemas['QueueView'];
type PoolView = Schemas['PoolView'];

// The contract's own examples: the triage queue, the maintainer-agents pool and one agent.
const exampleQueue = (examples.get('listQueues') as Schemas['QueueList'])
  .items[0]!;
const examplePool = (examples.get('listPools') as Schemas['PoolList'])
  .items[0]!;
const exampleAgent = (examples.get('listAgents') as Schemas['AgentList'])
  .items[0]!;
const POOL = examplePool.pool.metadata.name;

function queue(
  name: string,
  spec: Schemas['QueueSpec'],
  running: number,
  pending: Schemas['PendingTask'][],
): QueueView {
  return {
    queue: { kind: 'Queue', metadata: { name }, spec },
    depth: pending.length,
    running,
    pending,
  };
}

/**
 * One queue per pending reason the UI explains, dated around the fixtures' "now"
 * (2026-10-19T12:00:00Z): the default queue runs the mock work items' live run.
 */
export const queueViews: readonly QueueView[] = [
  queue(
    'review',
    { pool: POOL, concurrency: 1, priority: 70, retry: { max: 1 } },
    0,
    [
      {
        task: 'tsk_01JAC0REVIEWAPI31100000000',
        reason: 'anti-affinity',
        since: '2026-10-19T11:52:00Z',
      },
    ],
  ),
  {
    ...exampleQueue,
    depth: 1,
    running: 0,
    pending: [
      {
        task: 'tsk_01JAC0TRIAGEDOCS4300000000',
        reason: 'budget',
        since: '2026-10-19T09:15:00Z',
      },
    ],
  },
  queue(
    'default',
    {
      pool: POOL,
      concurrency: 2,
      priority: 50,
      budget: { daily: { tokens: 5_000_000, usd: 20 } },
    },
    1,
    [
      {
        task: 'tsk_01JAB3Q8R9S0T1V2W3X4Y5Z6AB',
        reason: 'agent-throttled',
        since: '2026-10-19T11:40:00Z',
      },
      {
        task: 'tsk_01JAC0IMPLGPU0000000000000',
        reason: 'no-capable-agent',
        since: '2026-10-19T10:05:00Z',
      },
      {
        task: 'tsk_01JAC0IMPLLENDING00000000',
        reason: 'lending',
        since: '2026-10-19T11:58:00Z',
      },
    ],
  ),
  queue(
    'nightly',
    { pool: 'ci-ephemeral', concurrency: 4, priority: 10 },
    0,
    [],
  ),
];

/** The example's agent, busy with the mock live run and heartbeating. */
const laptop: Agent = {
  ...exampleAgent,
  metadata: { ...exampleAgent.metadata, resourceVersion: '530' },
  spec: {
    ...exampleAgent.spec,
    capabilities: {
      ...exampleAgent.spec.capabilities,
      harnesses: exampleAgent.spec.capabilities.harnesses!.map((h) => ({
        ...h,
        defaultModel: h.models![h.models!.length - 1],
      })),
    },
  },
  status: {
    phase: 'Busy',
    throttledUntil: null,
    lastHeartbeat: '2026-10-19T11:59:40Z',
    running: [liveRun.spec.task],
    today: { runs: 4, tokens: 430_200 },
  },
};

/** A contributor's agent lending only the default queue, throttled by its Codex plan. */
const throttled: Agent = {
  kind: 'Agent',
  metadata: {
    name: 'agt_octocat-linux-02',
    labels: { owner: 'octocat' },
    resourceVersion: '531',
    createdAt: '2026-10-12T10:00:00Z',
  },
  spec: {
    pool: POOL,
    owner: 'github-public:octocat',
    capabilities: {
      harnesses: [
        {
          name: 'codex',
          version: '0.46.0',
          auth: 'subscription',
          models: ['gpt-5-codex'],
          reasoningEffort: ['low', 'medium'],
        },
      ],
      labels: ['linux'],
    },
    lending: {
      queues: ['default'],
      harnesses: ['codex'],
      maxConcurrent: 1,
      maxRunsPerDay: 10,
      maxTokensPerDay: 1_000_000,
    },
  },
  status: {
    phase: 'Throttled',
    throttledUntil: '2026-10-19T13:30:00Z',
    lastHeartbeat: '2026-10-19T11:59:50Z',
    today: { runs: 9, tokens: 980_000 },
  },
};

/** A shared build box lending everything, offline since last night. */
export const offline: Agent = {
  kind: 'Agent',
  metadata: {
    name: 'agt_ccf-bot-builder-03',
    labels: { owner: 'ccf-bot', site: 'lisbon' },
    resourceVersion: '400',
    createdAt: '2026-10-01T08:00:00Z',
  },
  spec: {
    pool: POOL,
    owner: 'github-public:ccf-bot',
    capabilities: {
      harnesses: [
        {
          name: 'claude-code',
          version: '2.1.3',
          auth: 'apiKey',
          models: ['claude-sonnet-5-5'],
        },
      ],
      labels: ['has-local-stack', 'linux'],
    },
    lending: { unlimited: true },
  },
  status: {
    phase: 'Offline',
    throttledUntil: null,
    lastHeartbeat: '2026-10-18T22:10:00Z',
    today: { runs: 0, tokens: 0 },
  },
};

export const agents: readonly Agent[] = [laptop, throttled, offline];

/** The builder back online: the agent update the mock stream sends on the `agents` topic. */
export const backOnline: Agent = {
  ...offline,
  metadata: { ...offline.metadata, resourceVersion: '533' },
  status: {
    ...offline.status!,
    phase: 'Ready',
    lastHeartbeat: '2026-10-19T12:00:00Z',
  },
};

/** The example's registered pool with every mock agent, and an ephemeral one with none. */
export const poolViews: readonly PoolView[] = [
  { pool: examplePool.pool, agents: agents.map((a) => a.metadata.name) },
  {
    pool: {
      kind: 'Pool',
      metadata: { name: 'ci-ephemeral' },
      spec: {
        mode: 'ephemeral',
        backend: {
          type: 'github-actions',
          githubActions: {
            providerRef: 'github-public',
            repo: 'compliance-framework/factory-runners',
            workflow: 'agent.yml',
          },
        },
        image: 'ghcr.io/compliance-framework/factory-agent:v0.1.0',
        scaling: { min: 0, max: 4 },
        lease: { ttl: '5m', heartbeat: '1m', maxTasksPerAgent: 1 },
        llmAuth: { apiKeyRef: { name: 'ci-llm', key: 'api-key' } },
      },
    },
    agents: [],
  },
];

/**
 * `GET /queues`, `GET /pools` and `GET /agents` over the fixtures above, one page each.
 * Registered before the example handlers, which they override.
 */
export const poolHandlers: HttpHandler[] = [
  http.get(`${API_BASE}/queues`, () =>
    HttpResponse.json<Schemas['QueueList']>({ items: [...queueViews] }),
  ),
  http.get(`${API_BASE}/pools`, () =>
    HttpResponse.json<Schemas['PoolList']>({ items: [...poolViews] }),
  ),
  http.get(`${API_BASE}/agents`, () =>
    HttpResponse.json<Schemas['AgentList']>({ items: [...agents] }),
  ),
];
