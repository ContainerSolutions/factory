import type { GraphCause, WorkItemGraph } from './graph';

/** Which cause an event lines up with first: what is recorded, then the tracking guess, then the rest. */
const CAUSE_RANK: Record<GraphCause, number> = {
  task: 0,
  run: 0,
  effect: 0,
  notify: 0,
  tracking: 1,
  inferred: 2,
};

/** The graph's geometry, in SVG units. */
export const PAD = 16;
export const COL_W = 248;
export const R = 14;
export const ACT_X = 52;
export const ACT_W = 180;
export const ACT_H = 50;
export const ROW_H = 74;
export const TASK_GAP = 32;
export const TASK_W = 176;
export const TASK_H = 58;
/** Room after a task for its link's gutter, before the next column. */
export const TASK_TRAIL = 24;
/** The lanes routed links run in, above every row. */
export const LANE_Y = 12;
const LANES = 6;
const LANE_STEP = 3.5;
/** The first row's top: below the lanes. */
export const TOP = 36;
const GUTTER = 10;
const CORNER = 5;
/** How far down an event is pushed to find a free straight line before the link is routed. */
const MAX_ROW = 60;
/** How far right of its source an event is looked for before it is laid out as uncaused. */
const MAX_SLOTS = 200;
/** The width of a slot only a straight link runs through. */
const LINE_W = 24;

export interface Box {
  x: number;
  y: number;
  w: number;
  h: number;
}

export interface PlacedEvent {
  /** The slot it sits in (see layoutGraph). */
  slot: number;
  row: number;
  /** The circle's centre. */
  cx: number;
  cy: number;
}

export interface PlacedAction extends Box {
  slot: number;
  row: number;
  /** The x of the spine from the event's circle down to its actions. */
  spineX: number;
}

export interface PlacedTask extends Box {
  slot: number;
  row: number;
}

export interface PlacedLink {
  key: string;
  from: string;
  to: string;
  inferred: boolean;
  cause: GraphCause;
  d: string;
  /** One horizontal line from the source's right edge to its target. */
  straight: boolean;
}

export interface GraphLayout {
  width: number;
  height: number;
  events: Map<string, PlacedEvent>;
  actions: Map<string, PlacedAction>;
  tasks: Map<string, PlacedTask>;
  links: PlacedLink[];
}

/** A row's centre line. */
export function rowY(row: number): number {
  return TOP + row * ROW_H + ROW_H / 2;
}

/** What a slot holds on each row: an event (circle and label), actions, tasks, or a link. */
interface Cells {
  event: Set<number>;
  action: Set<number>;
  task: Set<number>;
  line: Set<number>;
}

/**
 * Lays the graph out on a grid of slots and rows. A slot holds events (a circle and its label),
 * the actions on the rows below an event, or the tasks beside them: an event's actions share its
 * slot, its tasks take the next one. Events are placed in time order.
 *
 * An event something caused goes in the leftmost slot strictly right of its source (the most
 * certain cause: recorded, then tracking, then inferred; then the highest row), on the source's
 * row, so that arrow is one straight horizontal line. When the line would cross anything, the
 * event moves down to the first row where it doesn't (the arrow drops down the source's gutter).
 * When the event, its actions or their tasks would land on something already placed, it moves
 * one slot right. An event with no cause takes the leftmost slot on the top row after the
 * previous one. Each slot is as wide as what it holds, so the graph is as narrow as it can be.
 *
 * Any other link into an event goes up or down its source's gutter and straight along the
 * event's row when that row is free between them; else it is routed up the gutter, along a lane
 * above every row, and down just before its target.
 */
export function layoutGraph(graph: WorkItemGraph): GraphLayout {
  const cells: Cells[] = [];
  const at = (k: number): Cells =>
    (cells[k] ??= {
      event: new Set(),
      action: new Set(),
      task: new Set(),
      line: new Set(),
    });
  const busy = (k: number, r: number) => {
    const c = cells[k];
    return (
      !!c &&
      (c.event.has(r) || c.action.has(r) || c.task.has(r) || c.line.has(r))
    );
  };
  /** Whether a straight line on `row` from a source in slot `from` reaches slot `to`. */
  const lineFree = (row: number, from: number, to: number) => {
    for (let k = from + 1; k < to; k++) if (busy(k, row)) return false;
    return true;
  };

  type Cell = { slot: number; row: number };
  const eventAt = new Map<string, Cell>();
  const actionAt = new Map<string, Cell>();
  const taskAt = new Map<string, Cell>();
  /** The link each event is aligned with. */
  const aligned = new Map<string, number>();
  const sourceAt = (key: string) => {
    const a = actionAt.get(key);
    if (a) return { ...a, task: false };
    const t = taskAt.get(key);
    return t ? { ...t, task: true } : undefined;
  };

  let uncaused = -1;
  for (const e of graph.events) {
    const fits = (t: number, r: number) =>
      !busy(t, r) &&
      e.actions.every(
        (a, j) =>
          !busy(t, r + 1 + j) && (!a.taskNode || !busy(t + 1, r + 1 + j)),
      );
    const first = graph.links
      .map((l, i) => ({ l, i, s: sourceAt(l.from) }))
      .filter((x) => x.l.to === e.key && x.s)
      .sort(
        (a, b) =>
          CAUSE_RANK[a.l.cause] - CAUSE_RANK[b.l.cause] ||
          a.s!.row - b.s!.row ||
          a.i - b.i,
      )[0];
    let place: Cell | undefined;
    if (first) {
      const s = first.s!;
      for (let t = s.slot + 1; !place && t <= s.slot + MAX_SLOTS; t++) {
        let r = s.row;
        while (r <= MAX_ROW && !lineFree(r, s.slot, t)) r++;
        if (r > MAX_ROW || !fits(t, r)) continue;
        place = { slot: t, row: r };
        aligned.set(e.key, first.i);
        for (let k = s.slot + 1; k < t; k++) at(k).line.add(r);
      }
    }
    if (!place) {
      let t = uncaused + 1;
      while (!fits(t, 0)) t++;
      place = { slot: t, row: 0 };
      uncaused = t;
    }
    eventAt.set(e.key, place);
    at(place.slot).event.add(place.row);
    e.actions.forEach((a, j) => {
      const r = place.row + 1 + j;
      actionAt.set(a.key, { slot: place.slot, row: r });
      at(place.slot).action.add(r);
      if (a.taskNode) {
        taskAt.set(a.taskNode.key, { slot: place.slot + 1, row: r });
        at(place.slot + 1).task.add(r);
      }
    });
  }

  // Each slot as wide as what it holds: an event or actions, else tasks, else a link.
  const xs: number[] = [];
  const ws: number[] = [];
  let x = PAD;
  for (let k = 0; k < cells.length; k++) {
    xs.push(x);
    const c = cells[k];
    const w =
      c && (c.event.size || c.action.size)
        ? COL_W
        : c?.task.size
          ? TASK_GAP + TASK_W + TASK_TRAIL
          : c?.line.size
            ? LINE_W
            : 0;
    ws.push(w);
    x += w;
  }
  const width = x + PAD;
  /**
   * A source's gutter: near its slot's right edge, right of every box and label in the slot, so
   * an arrow can drop down it past them.
   */
  const gutterX = (k: number) => (xs[k] ?? x) + (ws[k] ?? 0) - GUTTER + 2;
  const slotX = (k: number) => xs[k] ?? x;

  const events = new Map<string, PlacedEvent>();
  const actions = new Map<string, PlacedAction>();
  const tasks = new Map<string, PlacedTask>();
  for (const e of graph.events) {
    const p = eventAt.get(e.key)!;
    const cx = slotX(p.slot) + PAD + R;
    events.set(e.key, { slot: p.slot, row: p.row, cx, cy: rowY(p.row) });
    for (const a of e.actions) {
      const q = actionAt.get(a.key)!;
      actions.set(a.key, {
        slot: q.slot,
        row: q.row,
        x: slotX(q.slot) + ACT_X,
        y: rowY(q.row) - ACT_H / 2,
        w: ACT_W,
        h: ACT_H,
        spineX: cx,
      });
      if (a.taskNode) {
        const u = taskAt.get(a.taskNode.key)!;
        tasks.set(a.taskNode.key, {
          slot: u.slot,
          row: u.row,
          x: slotX(u.slot) + TASK_GAP,
          y: rowY(u.row) - TASK_H / 2,
          w: TASK_W,
          h: TASK_H,
        });
      }
    }
  }
  const source = (key: string): (Box & Cell) | undefined =>
    actions.get(key) ?? tasks.get(key);

  let lane = 0;
  const links = graph.links.flatMap((l, i): PlacedLink[] => {
    const s = source(l.from);
    if (!s) return [];
    const x1 = s.x + s.w;
    const y1 = s.y + s.h / 2;
    const base = {
      key: `${l.from}->${l.to}`,
      from: l.from,
      to: l.to,
      inferred: l.inferred,
      cause: l.cause,
    };
    const task = tasks.get(l.to);
    if (task) {
      return [{ ...base, d: `M ${x1} ${y1} H ${task.x - 2}`, straight: true }];
    }
    const t = events.get(l.to);
    if (!t) return [];
    const ex = t.cx - R - 2;
    const gx = Math.max(x1 + CORNER + 1, gutterX(s.slot));
    const c = CORNER;
    const isAligned = aligned.get(l.to) === i;
    const onRow =
      isAligned ||
      (s.slot < t.slot &&
        ex > gx + c &&
        (t.row === s.row || lineFree(t.row, s.slot, t.slot)));
    if (onRow && t.cy === y1) {
      return [{ ...base, d: `M ${x1} ${y1} H ${ex}`, straight: true }];
    }
    if (onRow) {
      // Down (or up) the source's gutter, then straight along the event's row.
      const dir = t.cy > y1 ? 1 : -1;
      return [
        {
          ...base,
          d: [
            `M ${x1} ${y1}`,
            `H ${gx - c}`,
            `Q ${gx} ${y1}, ${gx} ${y1 + dir * c}`,
            `V ${t.cy - dir * c}`,
            `Q ${gx} ${t.cy}, ${gx + c} ${t.cy}`,
            `H ${ex}`,
          ].join(' '),
          straight: false,
        },
      ];
    }
    // Routed: up the gutter, along a lane above every row, down just before the target.
    const ly = LANE_Y + (lane++ % LANES) * LANE_STEP;
    const dx = slotX(t.slot) - 6;
    if (dx < gx + 2 * c) {
      const bend = Math.max(40, Math.abs(ex - x1) / 3);
      return [
        {
          ...base,
          d: `M ${x1} ${y1} C ${x1 + bend} ${y1}, ${ex - bend} ${t.cy}, ${ex} ${t.cy}`,
          straight: false,
        },
      ];
    }
    return [
      {
        ...base,
        d: [
          `M ${x1} ${y1}`,
          `H ${gx - c}`,
          `Q ${gx} ${y1}, ${gx} ${y1 - c}`,
          `V ${ly + c}`,
          `Q ${gx} ${ly}, ${gx + c} ${ly}`,
          `H ${dx - c}`,
          `Q ${dx} ${ly}, ${dx} ${ly + c}`,
          `V ${t.cy - c}`,
          `Q ${dx} ${t.cy}, ${dx + c} ${t.cy}`,
          `H ${ex}`,
        ].join(' '),
        straight: false,
      },
    ];
  });

  const rows = Math.max(
    1,
    ...[...events.values()].map((e) => e.row + 1),
    ...[...actions.values()].map((a) => a.row + 1),
  );
  return {
    width,
    height: TOP + rows * ROW_H + PAD,
    events,
    actions,
    tasks,
    links,
  };
}
