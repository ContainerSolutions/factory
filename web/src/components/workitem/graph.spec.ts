import { describe, expect, it } from 'vitest';
import { example, workItemDetails } from '@/mocks/workitems';
import { graphOf, parseAction } from './graph';

const byName = (name: string) =>
  workItemDetails.find((d) => d.workItem.metadata.name === name)!;
const pipeline = byName('wi_01JAC0PIPELINE0000000000000');

/** A link's source as `kind@column` (an action) or `task:<TaskSpec>` (a task). */
function from(g: ReturnType<typeof graphOf>, key: string): string {
  for (const e of g.events) {
    for (const a of e.actions) {
      if (a.key === key) return `${a.kind}@${e.column}`;
      if (a.taskNode?.key === key) return `task:${a.taskNode.spec}`;
    }
  }
  return key;
}

/** A link's target as its column (an event) or `task:<TaskSpec>` (a task). */
function col(g: ReturnType<typeof graphOf>, key: string): number | string {
  const e = g.events.find((e) => e.key === key);
  return e ? e.column : from(g, key);
}

describe('parseAction', () => {
  it.each([
    ['dispatch plan-epic → poc', 'dispatch', 'plan-epic → poc'],
    ['set plan', 'set', 'plan'],
    ['notify needs-human', 'notify', 'needs-human'],
    ['cancel implement', 'cancel', 'implement'],
    [
      'link compliance-framework/docs#78',
      'link',
      'compliance-framework/docs#78',
    ],
    ['effect label.add', 'effect', 'label.add'],
    ['debounce review', 'debounce', 'review'],
    ['unsupported requestApproval', 'requestApproval', ''],
    // An older record's bare kind.
    ['dispatch', 'dispatch', ''],
  ])('splits %j', (summary, kind, subject) => {
    expect(parseAction(summary)).toEqual({ kind, subject });
  });
});

describe('graphOf', () => {
  it('joins a real-format decision to its task, runs and run-completed event', () => {
    const g = graphOf(pipeline);
    const triage = g.events[0]!.actions[0]!;
    expect(pipeline.workItem.status!.decisions![0]!.actions![0]).toBe(
      'dispatch triage → default',
    );
    expect([triage.kind, triage.subject, triage.task?.metadata.id]).toEqual([
      'dispatch',
      'triage',
      'tsk_01JAC0PIPELINETRIAGE0000000',
    ]);
    expect(triage.runs.map((r) => r.metadata.name)).toEqual([
      'run_01JAC0PIPELINETRIAGE000000',
    ]);
    // The dispatch → its task → the run's completion.
    expect(
      g.links.filter(
        (l) => l.from === triage.key || l.from === triage.taskNode?.key,
      ),
    ).toEqual([
      {
        from: triage.key,
        to: 'task/tsk_01JAC0PIPELINETRIAGE0000000',
        inferred: false,
        cause: 'task',
      },
      {
        from: 'task/tsk_01JAC0PIPELINETRIAGE0000000',
        to: 'event/factory/run-completed-triage-77',
        inferred: false,
        cause: 'run',
      },
    ]);
  });

  it("takes a dispatch's TaskSpec from its summary when no task matched", () => {
    const g = graphOf(byName('wi_01JAC0ESCALATED0000000000000'));
    expect(g.events[0]!.actions[0]).toMatchObject({
      kind: 'dispatch',
      subject: 'implement',
      deterministic: false,
    });
  });

  it('lays out one column per event, in time order, with its fired actions', () => {
    const g = graphOf(pipeline);
    expect(g.events.map((e) => [e.column, e.type])).toEqual([
      [0, 'issue.opened'],
      [1, 'factory.run.completed'],
      [2, 'issue.commented'],
      [3, 'pr.opened'],
      [4, 'pr.labeled'],
      [5, 'factory.run.completed'],
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
        ['set', 'docs/triage-outside-issues', 'triage', 'done', true],
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
        [
          'notify',
          'docs/implement-promoted',
          'implement-started',
          'done',
          true,
        ],
      ],
      [],
      [
        [
          'dispatch',
          'docs/review-factory-prs',
          'docs-review',
          'Pending',
          false,
        ],
        [
          'effect',
          'docs/label-factory-prs',
          'label.add compliance-framework/docs#78',
          'applied',
          true,
        ],
      ],
      [],
      [],
    ]);
    // Both implement attempts are joined to its dispatch.
    expect(g.events[1]!.actions[0]!.runs.map((r) => r.spec.attempt)).toEqual([
      1, 2,
    ]);
  });

  it('links dispatches to tasks, tasks to completions and named effects, notify to its comment, and infers the rest', () => {
    const g = graphOf(pipeline);
    expect(
      g.links.map((l) => [from(g, l.from), col(g, l.to), l.cause]),
    ).toEqual([
      ['dispatch@0', 'task:triage', 'task'],
      ['task:triage', 1, 'run'],
      ['dispatch@1', 'task:implement', 'task'],
      // implement: its last run's completion, and pr.opened, whose data.effect is its pr.open.
      ['task:implement', 5, 'run'],
      ['task:implement', 3, 'effect'],
      // The comment whose data.effect is the notify's key, evt/<event>/<rule>/<i>/<request>.
      ['notify@1', 2, 'notify'],
      ['dispatch@3', 'task:docs-review', 'task'],
      // pr.labeled names no effect: guessed by ref and time, for both causes on docs#78.
      ['task:implement', 4, 'inferred'],
      ['effect@3', 4, 'inferred'],
    ]);
    expect(g.links.map((l) => l.inferred)).toEqual([
      false,
      false,
      false,
      false,
      false,
      false,
      false,
      true,
      true,
    ]);
  });

  it('draws each task with its phase, attempts, and outcome or pending reason', () => {
    const tasks = graphOf(pipeline)
      .events.flatMap((e) => e.actions)
      .flatMap((a) => (a.taskNode ? [a.taskNode] : []));
    expect(
      tasks.map((t) => [t.spec, t.phase, t.attempts, t.outcome, t.reason]),
    ).toEqual([
      ['triage', 'Succeeded', 1, 'promote', undefined],
      ['implement', 'Succeeded', 2, 'pr-opened', undefined],
      [
        'docs-review',
        'Pending',
        0,
        undefined,
        'anti-affinity: waiting for an agent other than the author',
      ],
    ]);
  });

  it("links the tracking comment from the latest decision's state changes, dashed", () => {
    const epic = byName('wi_d235e0c50568b22418e5');
    const g = graphOf(epic);
    expect(g.events.map((e) => e.type)).toEqual([
      'issue.opened',
      'issue.commented',
      'factory.run.completed',
    ]);
    expect(
      g.events.map((e) => e.actions.map((a) => `${a.kind} ${a.subject}`)),
    ).toEqual([
      ['dispatch plan-epic', 'set plan'],
      [],
      ['set plan', 'set stackNext', 'set stackTotal', 'dispatch implement'],
    ]);
    expect(
      g.links.map((l) => [from(g, l.from), col(g, l.to), l.cause, l.inferred]),
    ).toEqual([
      ['dispatch@0', 'task:plan-epic', 'task', false],
      ['task:plan-epic', 2, 'run', false],
      ['dispatch@2', 'task:implement', 'task', false],
      // set plan → the factory's issue.commented 4 s later.
      ['set@0', 1, 'tracking', true],
    ]);
    // The implement task has no run yet: it says why it waits.
    expect(g.events[2]!.actions[3]!.taskNode).toMatchObject({
      phase: 'Pending',
      attempts: 0,
      reason: expect.stringContaining('no-capable-agent'),
    });
  });

  it('links no tracking comment without a state change before it', () => {
    const epic = byName('wi_d235e0c50568b22418e5');
    const decisions = epic.workItem.status!.decisions!;
    const g = graphOf({
      ...epic,
      workItem: {
        ...epic.workItem,
        status: {
          ...epic.workItem.status,
          decisions: [
            { ...decisions[0]!, actions: ['dispatch plan-epic → poc'] },
            decisions[1]!,
          ],
        },
      },
    });
    expect(g.links.filter((l) => l.cause === 'tracking')).toEqual([]);
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
              actions: ['unsupported requestApproval'],
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

  it('links an exact match only once, and not by the heuristic too', () => {
    // Without data.effect, pr.opened was the heuristic's pick for implement.
    const g = graphOf({
      ...pipeline,
      events: pipeline.events.map((e) =>
        e.type === 'pr.opened'
          ? { ...e, data: { ...e.data, effect: undefined } }
          : e,
      ),
    });
    const toOpened = g.links.filter(
      (l) => l.to === 'event/github-public/gh-docs-78-opened',
    );
    expect(toOpened).toEqual([
      {
        from: 'task/tsk_01JAC0PIPELINEIMPLEMENT0000',
        to: 'event/github-public/gh-docs-78-opened',
        inferred: true,
        cause: 'inferred',
      },
    ]);
    // With it, the link is reliable and the heuristic moves on to pr.labeled.
    expect(
      graphOf(pipeline).links.filter(
        (l) => l.to === 'event/github-public/gh-docs-78-opened',
      ),
    ).toEqual([{ ...toOpened[0]!, inferred: false, cause: 'effect' }]);
  });

  it("links a rule's own effect to the event that names it", () => {
    const detail = {
      ...pipeline,
      events: pipeline.events.map((e) =>
        e.type === 'pr.labeled'
          ? {
              ...e,
              data: {
                ...e.data,
                effect: `evt/github-public/gh-docs-78-opened/label-factory-prs/0`,
              },
            }
          : e,
      ),
    };
    const g = graphOf(detail);
    const effect = g.events[3]!.actions[1]!;
    expect(effect.kind).toBe('effect');
    expect(g.links.filter((l) => l.from === effect.key)).toEqual([
      {
        from: effect.key,
        to: 'event/github-public/gh-docs-78-labeled',
        inferred: false,
        cause: 'effect',
      },
    ]);
  });
});
