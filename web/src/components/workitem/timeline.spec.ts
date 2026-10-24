import { describe, expect, it } from 'vitest';
import type { Schemas } from '@/api/client';
import { examples } from '@/mocks/examples';
import { workItemDetails } from '@/mocks/workitems';
import { graphOf } from './graph';
import { catalogSHAFor, eventRows, timelineOf, unattached } from './timeline';

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

describe('eventRows', () => {
  const pipeline = workItemDetails.find(
    (d) => d.workItem.metadata.name === 'wi_01JAC0PIPELINE0000000000000',
  )!;

  it("splits each event's decisions into fired (with their actions) and not fired", () => {
    const rows = eventRows(pipeline, graphOf(pipeline));
    expect(
      rows.map((r) => [
        r.node.type,
        r.fired.map(
          (d) =>
            `${d.decision.rule}: ${d.actions.map((a) => a.kind).join(', ')}`,
        ),
        r.notFired.map((d) => `${d.decision.rule}: ${d.decision.reason}`),
      ]),
    ).toEqual([
      [
        'issue.opened',
        ['triage-outside-issues: dispatch, set', 'docs/label-docs: effect'],
        ['triage-maintainer-issues: where is false'],
      ],
      [
        'factory.run.completed',
        ['implement-promoted: dispatch, notify'],
        ['escalate-needs-human: where is false'],
      ],
      [
        'pr.opened',
        ['review-factory-prs: dispatch', 'label-factory-prs: effect'],
        ['review-factory-prs: already fired through RuleSet docs'],
      ],
      ['pr.labeled', [], []],
      ['factory.run.completed', [], []],
    ]);
    expect(rows[0]!.notFired[0]!.index).toBe(2);
  });

  it('keeps what no action accounts for', () => {
    expect(unattached(pipeline, graphOf(pipeline))).toEqual([]);
    expect(unattached(example, graphOf(example))).toEqual([]);
    const orphan = {
      ...example,
      workItem: { ...example.workItem, status: {} },
    };
    expect(unattached(orphan, graphOf(orphan)).map((e) => e.kind)).toEqual([
      'task',
      'run',
      'effect',
    ]);
  });
});
