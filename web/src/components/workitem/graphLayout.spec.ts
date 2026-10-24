import { describe, expect, it } from 'vitest';
import { workItemDetails } from '@/mocks/workitems';
import { graphOf } from './graph';
import { layoutGraph, rowY } from './graphLayout';

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
      // From the implement task (row 2): pr.opened, issue.commented and the label effect sit
      // on rows 2–4 in between, so it goes down to row 5.
      ['pr.labeled', 5],
      ['factory.run.completed', 6],
    ]);
    const pushed = l.links.filter((k) => k.cause === 'run' && !k.straight);
    expect(pushed).toHaveLength(1);
    // Down the task's gutter, then straight along the event's row.
    expect(pushed[0]!.d).toMatch(new RegExp(` H \\S+$`));
    expect(pushed[0]!.d).toContain(`V ${rowY(6) - 5}`);
  });

  it('routes the second link into an event, and keeps an uncaused event on top', () => {
    const { l, g } = rows(pipeline);
    const labeled = g.events[4]!.key;
    const into = l.links.filter((k) => k.to === labeled);
    expect(into.map((k) => k.straight)).toEqual([false, false]);
    // The second, from the label effect, runs in the lanes above every row.
    expect(into[1]!.d).toMatch(/V 1\d(\.\d+)? /);
    expect(l.events.get(g.events[0]!.key)!.row).toBe(0);
    expect(l.height).toBeGreaterThan(rowY(6));
  });
});
