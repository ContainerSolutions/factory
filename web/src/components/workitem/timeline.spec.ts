import { describe, expect, it } from 'vitest';
import type { Schemas } from '@/api/client';
import { examples } from '@/mocks/examples';
import { catalogSHAFor, timelineOf } from './timeline';

const example = examples.get('getWorkItem') as Schemas['WorkItemDetail'];

describe('timelineOf', () => {
  it('merges the example by time, oldest first', () => {
    expect(timelineOf(example).map((e) => [e.kind, e.at])).toEqual([
      ['event', '2026-10-16T09:29:58Z'],
      ['decision', '2026-10-16T09:30:00Z'],
      ['task', '2026-10-16T09:30:00Z'],
      ['run', '2026-10-16T09:30:31Z'],
      ['effect', '2026-10-16T09:33:21Z'],
    ]);
  });

  it('orders by cause at the same instant and puts untimed entries last', () => {
    const at = '2026-10-16T10:00:00Z';
    const detail: Schemas['WorkItemDetail'] = {
      ...example,
      workItem: {
        ...example.workItem,
        status: { decisions: [{ rule: 'b' }, { at, rule: 'a' }] },
      },
      events: [{ ...example.events[0]!, time: at }],
      tasks: [],
      runs: [
        {
          ...example.runs[0]!,
          metadata: { name: 'run_x', createdAt: at },
          status: {},
        },
      ],
      effects: [],
    };
    expect(timelineOf(detail).map((e) => e.key)).toEqual([
      `event/${example.events[0]!.id}`,
      'decision/1',
      'run/run_x',
      'decision/0',
    ]);
  });

  it('keeps each decision index', () => {
    const decisions = timelineOf(example).filter((e) => e.kind === 'decision');
    expect(decisions.map((d) => d.kind === 'decision' && d.index)).toEqual([0]);
  });
});

describe('catalogSHAFor', () => {
  const decision = example.workItem.status!.decisions![0]!;

  it('is the provenance SHA of the task the decision created', () => {
    expect(catalogSHAFor(decision, example.tasks)).toBe(
      '9f1e2d3c4b5a69788796a5b4c3d2e1f0a9b8c7d6',
    );
  });

  it('is undefined when no task matches', () => {
    expect(
      catalogSHAFor({ ...decision, rule: 'other' }, example.tasks),
    ).toBeUndefined();
    expect(catalogSHAFor(decision, [])).toBeUndefined();
  });
});
