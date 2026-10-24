import { describe, expect, it } from 'vitest';
import { workItemDetails } from '@/mocks/workitems';
import { graphOf } from './graph';
import type { GraphAction, WorkItemGraph } from './graph';
import { ACT_W, LANE_Y, R, layoutGraph, rowY } from './graphLayout';

const byName = (name: string) =>
  workItemDetails.find((d) => d.workItem.metadata.name === name)!;
const epic = byName('wi_d235e0c50568b22418e5');
const pipeline = byName('wi_01JAC0PIPELINE0000000000000');

/** Each event's type and row. */
function rows(name: Parameters<typeof graphOf>[0]) {
  const g = graphOf(name);
  const l = layoutGraph(g);
  return {
    g,
    l,
    events: g.events.map((e) => [e.type, l.events.get(e.key)!.row]),
  };
}

describe('layoutGraph', () => {
  it("puts each of Gustavo's events on its cause's row, so its arrow is straight", () => {
    const { g, l, events } = rows(epic);
    expect(events).toEqual([
      ['issue.opened', 0],
      // set plan, issue.opened's second action row.
      ['issue.commented', 2],
      // the plan-epic task, beside the first action row.
      ['factory.run.completed', 1],
    ]);
    // The run's actions stack below it; the implement task sits beside its dispatch.
    const run = g.events[2]!;
    expect(run.actions.map((a) => l.actions.get(a.key)!.row)).toEqual([
      2, 3, 4, 5,
    ]);
    expect(l.tasks.get(run.actions[3]!.taskNode!.key)!.row).toBe(5);
    // Every arrow is one horizontal line at its source's row.
    expect(
      l.links.map((k) => [
        k.cause,
        k.straight,
        /^M \S+ (\S+) H \S+$/.test(k.d),
      ]),
    ).toEqual([
      ['task', true, true],
      ['run', true, true],
      ['task', true, true],
      ['tracking', true, true],
    ]);
    const tracking = l.links.find((k) => k.cause === 'tracking')!;
    expect(tracking.d.split(' ')[2]).toBe(String(rowY(2)));
  });

  it('pushes an event down when the straight line would cross a box', () => {
    const { events, l } = rows(pipeline);
    expect(events).toEqual([
      ['issue.opened', 0],
      ['factory.run.completed', 1],
      ['issue.commented', 3],
      ['pr.opened', 2],
      // Beside the label effect that caused it (pr.opened's second action).
      ['pr.labeled', 4],
      // From the implement task (row 2): pr.opened, issue.commented, the label effect and
      // pr.labeled sit on rows 2–4 in between, so it goes down to row 5.
      ['factory.run.completed', 5],
    ]);
    const pushed = l.links.filter((k) => k.cause === 'run' && !k.straight);
    expect(pushed).toHaveLength(1);
    // Down the task's gutter, then straight along the event's row.
    expect(pushed[0]!.d).toMatch(/ H \S+$/);
    expect(pushed[0]!.d).toContain(`V ${rowY(5) - 5}`);
  });

  it('keeps an uncaused event on the top row, and routes a link only when its row is blocked', () => {
    const { l, g } = rows(pipeline);
    expect(l.events.get(g.events[0]!.key)!.row).toBe(0);
    const labeled = g.events[4]!.key;
    const into = l.links.filter((k) => k.to === labeled);
    // The label effect's arrow is straight; the implement task's would cross the label effect,
    // so it runs in the lanes above every row.
    expect(into.map((k) => [k.from.split('/')[0], k.straight])).toEqual([
      ['task', false],
      ['action', true],
    ]);
    expect(into[0]!.d).toMatch(/V 1\d(\.\d+)? /);
    expect(l.height).toBeGreaterThan(rowY(5));
  });

  it('aligns with the recorded cause over an inferred one, which takes the gutter', () => {
    const action = (key: string): GraphAction =>
      ({ key, kind: 'set' }) as GraphAction;
    const g: WorkItemGraph = {
      events: [
        {
          key: 'event/a',
          id: 'a',
          type: 'issue.opened',
          column: 0,
          actions: [action('action/0/0'), action('action/0/1')],
        },
        {
          key: 'event/b',
          id: 'b',
          type: 'issue.commented',
          column: 1,
          actions: [],
        },
      ],
      links: [
        {
          from: 'action/0/0',
          to: 'event/b',
          inferred: true,
          cause: 'inferred',
        },
        { from: 'action/0/1', to: 'event/b', inferred: false, cause: 'notify' },
      ],
    };
    const l = layoutGraph(g);
    // The notify (row 2) wins over the inferred link from the higher row 1.
    expect(l.events.get('event/b')!.row).toBe(2);
    const [inferred, notify] = l.links;
    expect(notify!.straight).toBe(true);
    expect(notify!.d).toBe(
      `M ${l.actions.get('action/0/1')!.x + ACT_W} ${rowY(2)} H ${l.events.get('event/b')!.cx - R - 2}`,
    );
    // Down its gutter from row 1 to row 2 and straight on: no lane above the rows.
    expect(inferred!.straight).toBe(false);
    expect(inferred!.d).toContain(`V ${rowY(2) - 5}`);
    expect(inferred!.d.split(' ').filter((t) => t === 'V')).toHaveLength(1);
    expect(inferred!.d).not.toMatch(new RegExp(`V ${LANE_Y}`));
  });
});
