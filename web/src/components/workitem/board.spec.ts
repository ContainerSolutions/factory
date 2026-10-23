import { describe, expect, it } from 'vitest';
import type { Schemas } from '@/api/client';
import { workItemDetails } from '@/mocks/workitems';
import {
  boardLanes,
  facets,
  lastActivity,
  laneOf,
  matches,
  NO_FILTERS,
  primaryLink,
  repoOf,
  titleOf,
  type LaneId,
} from './board';

type WorkItem = Schemas['WorkItem'];

function wi(
  status: WorkItem['status'],
  extra: Partial<WorkItem> = {},
): WorkItem {
  return {
    kind: 'WorkItem',
    metadata: { name: 'wi_1', createdAt: '2026-10-10T00:00:00Z' },
    spec: {},
    status,
    ...extra,
  };
}

const items = workItemDetails.map((d) => d.workItem);
const NOW = new Date('2026-10-19T12:00:00Z');

// State shapes the PoC catalog's rules write (internal/catalog/testdata/poc/rules): the
// release-docs pack's `release`, `train`, `manifest` and `review`, the docs-epic pack's `plan`,
// `stackNext`, `stackTotal`, `review`, `merge` and `announced`.
const train = {
  train: 'compliance-framework/workflows#123',
  manifest: 'repos.yaml',
};
const plan = [
  { issue: 'compliance-framework/docs#58' },
  { issue: 'compliance-framework/docs#59' },
];
const stack = { plan, stackNext: 1, stackTotal: 2 };

describe('laneOf', () => {
  it.each<[string, Record<string, unknown>, LaneId]>([
    ['empty state', {}, 'waiting'],
    // release-docs: docs-on-release-train, review-release-docs, announce-release-docs.
    ['release drafting', { release: 'drafting', ...train }, 'running'],
    [
      'release under review',
      { release: 'drafting', ...train, review: 'running' },
      'running',
    ],
    ['release published', { release: 'published', ...train }, 'done'],
    // The release-docs pack sets `review: running` but records no review outcome, so a
    // published release whose review key is still `running` stays in Running.
    [
      'release published, review key left running',
      { release: 'published', ...train, review: 'running' },
      'running',
    ],
    // docs-epic: plan-maintainer-epics, then build-stack-start sets the plan array.
    ['plan planning', { plan: 'planning' }, 'running'],
    ['plan decomposed into a stack', { ...stack }, 'waiting'],
    // docs-epic: review-whole-stack, then record-review-outcome.
    ['review running', { ...stack, review: 'running' }, 'running'],
    ['review approved', { ...stack, review: 'approved' }, 'done'],
    [
      'review changes requested',
      { ...stack, review: 'changes-requested' },
      'waiting',
    ],
    ['review commented', { ...stack, review: 'commented' }, 'waiting'],
    [
      'review needs a human',
      { ...stack, review: 'needs-human' },
      'needs-human',
    ],
    // docs-epic: record-merge-outcome.
    [
      'merge merged',
      { ...stack, review: 'approved', merge: 'merged', announced: true },
      'done',
    ],
    [
      'merge labelled for merge',
      { ...stack, review: 'approved', merge: 'labelled-for-merge' },
      'waiting',
    ],
    [
      'merge rebased with conflicts',
      { ...stack, review: 'approved', merge: 'rebased-with-conflicts' },
      'waiting',
    ],
    [
      'merge blocked',
      { ...stack, review: 'approved', merge: 'blocked' },
      'needs-human',
    ],
    // The triage TaskSpec's outcomes.
    ['triage running', { triage: 'running' }, 'running'],
    ['triaged', { triage: 'triaged' }, 'done'],
    ['triage needs info', { triage: 'needs-info' }, 'waiting'],
    ['triage promoted', { triage: 'promote' }, 'waiting'],
    // Vocabulary-free values don't count either way.
    ['only refs and ids', { ...train }, 'waiting'],
    ['an unknown word', { review: 'pondering', merge: 'merged' }, 'done'],
    // The close rule's `lifecycle: closed`: done whatever else is set.
    ['closed', { lifecycle: 'closed' }, 'done'],
    [
      'closed with an open value',
      { ...stack, review: 'changes-requested', lifecycle: 'closed' },
      'done',
    ],
    [
      'closed with a needs-human value',
      { ...stack, merge: 'blocked', lifecycle: 'closed' },
      'done',
    ],
    [
      'closed with a running value',
      { ...stack, review: 'running', lifecycle: 'closed' },
      'done',
    ],
  ])('%s', (_, values, want) => {
    expect(laneOf(wi({ state: values }))).toBe(want);
  });

  it.each<[string, WorkItem['status'], LaneId]>([
    ['no status', undefined, 'waiting'],
    [
      'a pending approval',
      {
        state: { ...stack },
        approvals: [{ name: 'implement', phase: 'Pending' }],
      },
      'needs-human',
    ],
    [
      'a pending approval beats done',
      {
        state: { review: 'approved' },
        approvals: [{ name: 'merge', phase: 'Pending' }],
      },
      'needs-human',
    ],
    [
      'closed beats a pending approval',
      {
        state: { ...stack, lifecycle: 'closed' },
        approvals: [{ name: 'merge', phase: 'Pending' }],
      },
      'done',
    ],
    [
      'a granted approval',
      {
        state: { ...stack },
        approvals: [{ name: 'implement', phase: 'Granted' }],
      },
      'waiting',
    ],
  ])('%s', (_, status, want) => {
    expect(laneOf(wi(status))).toBe(want);
  });
});

describe('lastActivity', () => {
  it('is the newest decision', () => {
    const item = wi({
      decisions: [
        { at: '2026-10-12T00:00:00Z' },
        { at: '2026-10-14T00:00:00Z' },
        { at: 'not a time' },
      ],
    });
    expect(lastActivity(item)?.toISOString()).toBe('2026-10-14T00:00:00.000Z');
  });

  it('falls back to the creation time', () => {
    expect(lastActivity(wi({}))?.toISOString()).toBe(
      '2026-10-10T00:00:00.000Z',
    );
  });

  it('is undefined without any time', () => {
    expect(
      lastActivity(wi({}, { metadata: { name: 'wi_1' } })),
    ).toBeUndefined();
  });
});

describe('repoOf', () => {
  it.each([
    ['compliance-framework/docs#42', 'compliance-framework/docs'],
    ['compliance-framework/docs@main', 'compliance-framework/docs'],
    ['compliance-framework/docs', 'compliance-framework/docs'],
    ['#ccf-api/1729070000.000100', undefined],
    ['just-a-name', undefined],
  ])('%s → %s', (ref, want) => {
    expect(repoOf(ref)).toBe(want);
  });
});

describe('primaryLink and titleOf', () => {
  it('prefers the primary link', () => {
    const item = wi(
      {},
      {
        spec: {
          links: [
            { provider: 'slack-ccf', ref: '#a/1' },
            { provider: 'github-public', ref: 'o/r#1', primary: true },
          ],
        },
      },
    );
    expect(primaryLink(item)?.ref).toBe('o/r#1');
    expect(titleOf(item)).toBe('o/r#1');
  });

  it('falls back to the first link, then the name', () => {
    expect(
      titleOf(wi({}, { spec: { links: [{ provider: 'p', ref: 'x' }] } })),
    ).toBe('x');
    expect(titleOf(wi({}))).toBe('wi_1');
  });
});

describe('facets and matches', () => {
  it('collects every value, sorted and unique', () => {
    expect(facets(items)).toEqual({
      ruleSet: ['api', 'docs', 'docs-epic', 'release-docs', 'ui'],
      provider: ['github-enterprise', 'github-public', 'slack-ccf'],
      repo: [
        'compliance-framework/api',
        'compliance-framework/docs',
        'compliance-framework/ui',
        'platform/release-notes',
      ],
      taskSpec: [
        'docs-review',
        'implement',
        'merge',
        'plan-epic',
        'release-docs',
        'triage',
      ],
    });
  });

  it.each([
    [{ ...NO_FILTERS }, 7],
    [{ ...NO_FILTERS, ruleSet: 'docs' }, 3],
    [{ ...NO_FILTERS, provider: 'slack-ccf' }, 1],
    [{ ...NO_FILTERS, repo: 'compliance-framework/docs' }, 4],
    [{ ...NO_FILTERS, ruleSet: 'docs', taskSpec: 'triage' }, 2],
    [{ ...NO_FILTERS, ruleSet: 'nope' }, 0],
  ])('%o matches %i work items', (filters, want) => {
    expect(items.filter((i) => matches(i, filters))).toHaveLength(want);
  });
});

describe('boardLanes', () => {
  it('puts each work item in its lane, needs-human first', () => {
    const lanes = boardLanes(items, NO_FILTERS, NOW);
    expect(
      lanes.map((l) => [l.id, l.items.map((i) => titleOf(i)), l.hidden]),
    ).toEqual([
      [
        'needs-human',
        ['compliance-framework/api#310', 'compliance-framework/docs#57'],
        0,
      ],
      [
        'running',
        ['platform/release-notes#7', 'compliance-framework/docs#42'],
        0,
      ],
      ['waiting', ['compliance-framework/ui#88'], 0],
      ['done', ['compliance-framework/docs#40'], 1],
    ]);
  });

  it('drops done work items once their last activity is over 7 days old', () => {
    const lanes = boardLanes(
      items,
      NO_FILTERS,
      new Date('2026-10-25T00:00:00Z'),
    );
    expect(lanes[3]!.items).toHaveLength(0);
    expect(lanes[3]!.hidden).toBe(2);
  });

  it('applies the filters', () => {
    const lanes = boardLanes(
      items,
      { ...NO_FILTERS, provider: 'github-enterprise' },
      NOW,
    );
    expect(lanes.map((l) => l.items.length)).toEqual([0, 1, 0, 0]);
  });
});
