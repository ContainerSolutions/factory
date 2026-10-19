import { describe, expect, it } from 'vitest';
import type { Schemas } from '@/api/client';
import { NOW } from '@/__tests__/mount';
import { agents, offline, poolViews, queueViews } from '@/mocks/pools';
import {
  ago,
  budgetText,
  capacityOf,
  formatSpan,
  heartbeatOf,
  leasePolicy,
  ofCap,
  parseDuration,
  poolGroups,
  reasonText,
  sortQueues,
  throttleOf,
} from './pools';

const [laptop, throttled] = agents as [
  Schemas['Agent'],
  Schemas['Agent'],
  Schemas['Agent'],
];
const registered = poolViews[0]!.pool;
const ephemeral = poolViews[1]!.pool;

function withStatus(
  agent: Schemas['Agent'],
  status: Partial<Schemas['AgentStatus']>,
): Schemas['Agent'] {
  return {
    ...agent,
    status: { phase: 'Ready', throttledUntil: null, ...status },
  };
}

describe('reasonText', () => {
  it.each([
    'queue-full',
    'budget',
    'agent-busy',
    'agent-throttled',
    'lending',
    'no-capable-agent',
    'anti-affinity',
    'agent-lost',
    'git-credential',
  ])('explains %s', (reason) => {
    expect(reasonText(reason)).toMatch(/\.$/);
  });

  it('knows nothing of other reasons', () => {
    expect(reasonText('something-new')).toBeUndefined();
    expect(reasonText(undefined)).toBeUndefined();
    expect(reasonText('toString')).toBeUndefined();
  });
});

describe('parseDuration', () => {
  it.each<[string | undefined, number | undefined]>([
    ['30s', 30_000],
    ['2m', 120_000],
    ['1h30m', 5_400_000],
    ['1.5s', 1500],
    ['250ms', 250],
    ['2 m', undefined],
    ['soon', undefined],
    ['', undefined],
    [undefined, undefined],
  ])('%s → %s', (d, want) => {
    expect(parseDuration(d)).toBe(want);
  });
});

describe('formatSpan', () => {
  it.each([
    [0, '0 s'],
    [-5000, '0 s'],
    [40_000, '40 s'],
    [12 * 60_000, '12 min'],
    [2 * 3_600_000, '2 h'],
    [2 * 3_600_000 + 5 * 60_000, '2 h 5 min'],
    [3 * 86_400_000, '3 d'],
  ])('%d ms → %s', (ms, want) => {
    expect(formatSpan(ms)).toBe(want);
  });

  it('says how long ago a time was', () => {
    expect(ago('2026-10-19T11:48:00Z', NOW)).toBe('12 min');
    expect(ago('not a time', NOW)).toBeUndefined();
    expect(ago(undefined, NOW)).toBeUndefined();
  });
});

describe('leasePolicy and heartbeatOf', () => {
  it("uses the pool's lease policy, else the scheduler's defaults", () => {
    expect(leasePolicy(registered)).toEqual({
      heartbeatMs: 30_000,
      ttlMs: 120_000,
    });
    expect(leasePolicy(ephemeral)).toEqual({
      heartbeatMs: 60_000,
      ttlMs: 300_000,
    });
    expect(leasePolicy(undefined)).toEqual({
      heartbeatMs: 30_000,
      ttlMs: 120_000,
    });
  });

  it('is on time within the ttl, stale after it, never without one', () => {
    expect(heartbeatOf(laptop, registered, NOW)).toEqual({
      state: 'on time',
      ago: '20 s',
    });
    expect(heartbeatOf(offline, registered, NOW)).toEqual({
      state: 'stale',
      ago: '13 h 50 min',
    });
    const late = withStatus(laptop, { lastHeartbeat: '2026-10-19T11:57:00Z' });
    expect(heartbeatOf(late, registered, NOW).state).toBe('stale');
    // The ephemeral pool's ttl is 5 min.
    expect(heartbeatOf(late, ephemeral, NOW).state).toBe('on time');
    expect(heartbeatOf(withStatus(laptop, {}), registered, NOW)).toEqual({
      state: 'never',
    });
  });
});

describe('throttleOf', () => {
  it('is throttled until a time ahead, passed after it, none without one', () => {
    expect(throttleOf(throttled, NOW)).toEqual({
      state: 'throttled',
      until: '2026-10-19T13:30:00Z',
      in: '1 h 30 min',
    });
    const passed = withStatus(throttled, {
      phase: 'Throttled',
      throttledUntil: '2026-10-19T11:00:00Z',
    });
    expect(throttleOf(passed, NOW)).toEqual({
      state: 'passed',
      until: '2026-10-19T11:00:00Z',
    });
    expect(throttleOf(laptop, NOW)).toEqual({ state: 'none' });
    expect(throttleOf({ ...laptop, status: undefined }, NOW)).toEqual({
      state: 'none',
    });
  });
});

describe('capacityOf', () => {
  it('is the smaller of the lending and pool limits, else 1', () => {
    const lending = (maxConcurrent?: number): Schemas['Agent'] => ({
      ...laptop,
      spec: { ...laptop.spec, lending: { maxConcurrent } },
    });
    const pool = (maxTasksPerAgent?: number): Schemas['Pool'] => ({
      ...registered,
      spec: { ...registered.spec, lease: { maxTasksPerAgent } },
    });
    expect(capacityOf(lending(3), pool(2))).toBe(2);
    expect(capacityOf(lending(1), pool(4))).toBe(1);
    expect(capacityOf(lending(3), pool(undefined))).toBe(3);
    expect(capacityOf(lending(undefined), pool(undefined))).toBe(1);
    expect(capacityOf(lending(3), undefined)).toBe(3);
  });
});

describe('ofCap and budgetText', () => {
  it('shows usage against a cap', () => {
    expect(ofCap(4, 30)).toBe('4 of 30');
    expect(ofCap(430_200, 3_000_000)).toBe('430,200 of 3,000,000');
    expect(ofCap(4, undefined)).toBe('4');
    expect(ofCap(undefined, undefined)).toBe('0');
  });

  it("shows a queue's daily budget", () => {
    expect(budgetText({ daily: { runs: 80 } })).toBe('80 runs a day');
    expect(budgetText({ daily: { tokens: 5_000_000, usd: 20 } })).toBe(
      '5,000,000 tokens, $20.00 a day',
    );
    expect(budgetText({ daily: {} })).toBe('');
    expect(budgetText(undefined)).toBe('');
  });
});

describe('poolGroups', () => {
  it("groups the agents by the pools' order", () => {
    const groups = poolGroups(poolViews, agents);
    expect(
      groups.map((g) => [g.name, g.agents.map((a) => a.metadata.name)]),
    ).toEqual([
      [
        'maintainer-agents',
        [
          'agt_gusfcarvalho-mbp-01',
          'agt_octocat-linux-02',
          'agt_ccf-bot-builder-03',
        ],
      ],
      ['ci-ephemeral', []],
    ]);
    expect(groups.every((g) => g.missing.length === 0)).toBe(true);
  });

  it("places an agent the pool doesn't list yet by its spec, and reports listed names not returned", () => {
    const newcomer: Schemas['Agent'] = {
      ...laptop,
      metadata: { name: 'agt_new-host' },
      spec: { ...laptop.spec, pool: 'ci-ephemeral' },
    };
    const groups = poolGroups(poolViews, [laptop, newcomer]);
    expect(groups[0]!.missing).toEqual([
      'agt_octocat-linux-02',
      'agt_ccf-bot-builder-03',
    ]);
    expect(groups[1]!.agents.map((a) => a.metadata.name)).toEqual([
      'agt_new-host',
    ]);
  });

  it('gives agents of a pool not listed a group of their own, last', () => {
    const stray: Schemas['Agent'] = {
      ...laptop,
      metadata: { name: 'agt_stray' },
      spec: { ...laptop.spec, pool: 'gone' },
    };
    const groups = poolGroups(poolViews.slice(0, 1), [stray]);
    expect(groups.map((g) => [g.name, !!g.pool, g.agents.length])).toEqual([
      ['maintainer-agents', true, 0],
      ['gone', false, 1],
    ]);
  });
});

describe('sortQueues', () => {
  it('orders by priority, highest first, then name', () => {
    expect(sortQueues(queueViews).map((q) => q.queue.metadata.name)).toEqual([
      'review',
      'triage',
      'default',
      'nightly',
    ]);
    const unprioritised = (name: string): Schemas['QueueView'] => ({
      queue: { kind: 'Queue', metadata: { name }, spec: { pool: 'p' } },
      depth: 0,
      running: 0,
      pending: [],
    });
    expect(
      sortQueues([unprioritised('b'), unprioritised('a')]).map(
        (q) => q.queue.metadata.name,
      ),
    ).toEqual(['a', 'b']);
  });
});
