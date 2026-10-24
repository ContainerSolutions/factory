import { describe, expect, it } from 'vitest';
import { workItemDetails } from '@/mocks/workitems';
import { graphOf, withoutIdle } from './graph';
import type { GraphAction, WorkItemGraph } from './graph';
import {
  ACT_W,
  COL_W,
  LANE_Y,
  PAD,
  R,
  TASK_GAP,
  TASK_TRAIL,
  TASK_W,
  layoutGraph,
  rowY,
} from './graphLayout';

const byName = (name: string) =>
  workItemDetails.find((d) => d.workItem.metadata.name === name)!;
const epic = byName('wi_d235e0c50568b22418e5');
const pipeline = byName('wi_01JAC0PIPELINE0000000000000');

/** Each event's type and row. */
function rows(name: Parameters<typeof graphOf>[0]) {
  const g = withoutIdle(graphOf(name)).graph;
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

  it('keeps an uncaused event on the top row, and no link loops through the lanes', () => {
    const { l, g } = rows(pipeline);
    expect(l.events.get(g.events[0]!.key)!.row).toBe(0);
    const labeled = g.events[4]!.key;
    // One guess into pr.labeled: the label effect's, straight along its row.
    expect(
      l.links
        .filter((k) => k.to === labeled)
        .map((k) => [k.from.split('/')[0], k.straight]),
    ).toEqual([['action', true]]);
    // No path climbs to the lanes above the first row.
    expect(
      l.links.filter((k) => /V 1\d(\.\d+)? /.test(k.d)).map((k) => k.key),
    ).toEqual([]);
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

  describe('packing into slots', () => {
    const build = byName('wi_01JAC0BUILDSINGLE0000000000');

    it("packs Gustavo's item into fewer slots, each moved event beside its cause", () => {
      const { g, l } = rows(build);
      const at = (i: number) => {
        const p = l.events.get(g.events[i]!.key)!;
        return [g.events[i]!.type, p.slot, p.row];
      };
      const task = (spec: string) =>
        [...l.tasks.entries()].find(([k]) =>
          g.events.some((e) =>
            e.actions.some(
              (a) => a.taskNode?.key === k && a.taskNode.spec === spec,
            ),
          ),
        )![1];
      expect(at(0)).toEqual(['issue.opened', 0, 0]);
      // Beside set plan, under the plan-epic task (which sits in slot 1).
      expect(task('plan-epic')).toMatchObject({ slot: 1, row: 1 });
      expect(at(1)).toEqual(['issue.commented', 1, 2]);
      // Right after the plan-epic task.
      expect(at(2)).toEqual(['factory.run.completed', 2, 1]);
      // Right after the build-single sets (slot 2).
      expect(at(3)).toEqual(['issue.commented', 3, 2]);
      expect(task('implement')).toMatchObject({ slot: 3, row: 5 });
      expect(at(4)).toEqual(['pr.opened', 4, 5]);
      // pr.opened holds slot 4 on row 5: one slot right, and down a row so its line is free.
      expect(at(5)).toEqual(['factory.run.completed', 5, 6]);
      // One column per event (and a task column beside each dispatch) took far more room.
      const before =
        2 * PAD +
        g.events.reduce(
          (w, e) =>
            w +
            COL_W +
            (e.actions.some((a) => a.taskNode)
              ? TASK_GAP + TASK_W + TASK_TRAIL
              : 0),
          0,
        );
      expect(l.width).toBeLessThan(before - COL_W);
      // Seven slots (the review task in the last), where today drew six columns and four task
      // columns.
      expect(Math.max(...[...l.tasks.values()].map((t) => t.slot)) + 1).toBe(7);
      // And its arrows are still straight where they were.
      expect(
        l.links
          .filter((k) => k.cause !== 'tracking')
          .map((k) => [k.cause, k.straight]),
      ).toEqual([
        ['task', true],
        ['run', true],
        ['task', true],
        ['run', false],
        ['effect', true],
        ['task', true],
      ]);
    });

    it('bumps an event one slot right when its slot is taken on its row', () => {
      const { g, l } = rows(build);
      const opened = l.events.get(g.events[4]!.key)!;
      const done = l.events.get(g.events[5]!.key)!;
      // Both are caused by the implement task (slot 3, row 5).
      expect(opened).toMatchObject({ slot: 4, row: 5 });
      expect(done.slot).toBe(opened.slot + 1);
    });

    it.each(workItemDetails.map((d) => [d.workItem.metadata.name, d]))(
      'keeps every event right of its cause in %s',
      (_, detail) => {
        const { g, l } = rows(detail);
        for (const k of g.links) {
          const from = l.actions.get(k.from) ?? l.tasks.get(k.from);
          const to = l.events.get(k.to) ?? l.tasks.get(k.to);
          if (!from || !to) continue;
          expect(to.slot, `${k.from} -> ${k.to}`).toBeGreaterThan(from.slot);
        }
      },
    );
  });
});
