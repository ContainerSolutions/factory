import { describe, expect, it } from 'vitest';
import type { Schemas } from '@/api/client';
import { examples } from '@/mocks/examples';
import { workItemDetails } from '@/mocks/workitems';
import { graphOf } from './graph';

const example = examples.get('getWorkItem') as Schemas['WorkItemDetail'];
const byName = (name: string) =>
  workItemDetails.find((d) => d.workItem.metadata.name === name)!;
const pipeline = byName('wi_01JAC0PIPELINE0000000000000');

describe('graphOf', () => {
  it('lays out one column per event, in time order, with its fired actions', () => {
    const g = graphOf(pipeline);
    expect(g.events.map((e) => [e.column, e.type])).toEqual([
      [0, 'issue.opened'],
      [1, 'factory.run.completed'],
      [2, 'pr.opened'],
      [3, 'factory.run.completed'],
    ]);
    expect(
      g.events.map((e) =>
        e.actions.map((a) => [
          a.kind,
          a.rule,
          a.subject,
          a.state,
          a.deterministic,
        ]),
      ),
    ).toEqual([
      [
        [
          'dispatch',
          'docs/triage-outside-issues',
          'triage',
          'Succeeded',
          false,
        ],
        ['set', 'docs/triage-outside-issues', '', 'done', true],
        [
          'effect',
          'docs/docs/label-docs',
          'label.add compliance-framework/docs#77',
          'applied',
          true,
        ],
      ],
      [
        [
          'dispatch',
          'docs/implement-promoted',
          'implement',
          'Succeeded',
          false,
        ],
        ['notify', 'docs/implement-promoted', '', 'done', true],
      ],
      [
        [
          'dispatch',
          'docs/review-factory-prs',
          'docs-review',
          'Pending',
          false,
        ],
      ],
      [],
    ]);
    // Both implement attempts are joined to its dispatch.
    expect(g.events[1]!.actions[0]!.runs.map((r) => r.spec.attempt)).toEqual([
      1, 2,
    ]);
  });

  it("links dispatches to their runs' completions, and infers the PR event", () => {
    const g = graphOf(pipeline);
    const at = (key: string) => {
      const e = g.events.find((e) => e.actions.some((a) => a.key === key))!;
      const a = e.actions.find((a) => a.key === key)!;
      return `${a.kind}@${e.column}`;
    };
    const col = (key: string) => g.events.find((e) => e.key === key)!.column;
    expect(g.links.map((l) => [at(l.from), col(l.to), l.inferred])).toEqual([
      ['dispatch@0', 1, false],
      ['dispatch@1', 3, false],
      ['dispatch@1', 2, true],
    ]);
  });

  it('adds an event only a decision names, and shows a held action by its rail', () => {
    const g = graphOf(byName('wi_01JAC0ESCALATED0000000000000'));
    expect(
      g.events.map((e) => [
        e.type,
        e.actions.map((a) => `${a.kind} ${a.state}`),
      ]),
    ).toEqual([
      [
        'pr.review.submitted',
        ['dispatch Suppressed', 'effect not recorded', 'notify done'],
      ],
    ]);
    expect(g.links).toEqual([]);
  });

  it('names an approval by its phase, and an event the item no longer holds', () => {
    const g = graphOf({
      ...example,
      events: [],
      workItem: {
        ...example.workItem,
        status: {
          approvals: [{ name: 'implement', phase: 'Granted' }],
          decisions: [
            {
              at: '2026-10-16T10:00:00Z',
              event: 'gone',
              ruleSet: 'r',
              rule: 'x',
              fired: true,
              actions: ['requestApproval'],
            },
            {
              at: '2026-10-16T10:00:00Z',
              event: 'gone',
              ruleSet: 'r',
              rule: 'y',
              fired: false,
            },
          ],
        },
      },
    });
    expect(g.events).toHaveLength(1);
    expect(g.events[0]).toMatchObject({
      type: 'event',
      id: 'gone',
      at: '2026-10-16T10:00:00Z',
    });
    expect(g.events[0]!.actions.map((a) => [a.subject, a.state])).toEqual([
      ['implement', 'Granted'],
    ]);
  });

  it('joins the example: its dispatch to the running triage task', () => {
    const g = graphOf(example);
    expect(g.events[0]!.actions.map((a) => `${a.kind} ${a.state}`)).toEqual([
      'dispatch Running',
      'set done',
    ]);
  });
});
