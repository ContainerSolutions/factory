import { describe, expect, it } from 'vitest';
import type { Schemas } from '@/api/client';
import { freeform, workItemDetails } from '@/mocks/workitems';
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

const state = freeform;
const items = workItemDetails.map((d) => d.workItem);
const NOW = new Date('2026-10-19T12:00:00Z');

describe('laneOf', () => {
  it.each<[string, WorkItem['status'], LaneId]>([
    ['no status', undefined, 'waiting'],
    ['empty state', { state: state({}) }, 'waiting'],
    ['a running key', { state: state({ triage: 'running' }) }, 'running'],
    ['a drafting key', { state: state({ release: 'drafting' }) }, 'running'],
    [
      'needs-human beats running',
      { state: state({ triage: 'running', review: 'needs-human' }) },
      'needs-human',
    ],
    ['blocked', { state: state({ merge: 'blocked' }) }, 'needs-human'],
    [
      'a pending approval',
      {
        state: state({ plan: 'planned' }),
        approvals: [{ name: 'implement', phase: 'Pending' }],
      },
      'needs-human',
    ],
    [
      'a granted approval',
      {
        state: state({ plan: 'planned' }),
        approvals: [{ name: 'implement', phase: 'Granted' }],
      },
      'waiting',
    ],
    [
      'every key terminal',
      { state: state({ implement: 'done', merge: 'merged' }) },
      'done',
    ],
    [
      'one key not terminal',
      { state: state({ implement: 'done', review: 'changes-requested' }) },
      'waiting',
    ],
    [
      'non-string values are ignored',
      { state: state({ merge: 'merged', stackNext: 2, plan: ['a'] }) },
      'done',
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
