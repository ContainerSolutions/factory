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

export interface Box {
  x: number;
  y: number;
  w: number;
  h: number;
}

export interface PlacedEvent {
  column: number;
  row: number;
  /** The circle's centre. */
  cx: number;
  cy: number;
}

export interface PlacedAction extends Box {
  column: number;
  row: number;
  /** The x of the spine from the event's circle down to its actions. */
  spineX: number;
}

export interface PlacedTask extends Box {
  column: number;
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

/** What a column holds on each row: its event, its actions, and the tasks beside them. */
interface Cells {
  event: Set<number>;
  action: Set<number>;
  task: Set<number>;
}

/**
 * Lays the graph out on a grid: a column per event, in time order (wider when it holds a task),
 * and rows. An event with no cause sits on the top row. An event something caused sits on the
 * row of one incoming link's source, so that arrow is one straight horizontal line: the most
 * certain cause (recorded, then tracking, then inferred), then one whose line is free on its own
 * row, then the highest row. When the line would cross a box or a circle, the event moves down
 * to the first row where it doesn't, and the arrow drops down the source's gutter to it. Its
 * actions stack on the rows below it, each task beside its dispatch. Any other link into the
 * event goes up or down its source's gutter and straight along the event's row when that row is
 * free between them; else it is routed up the gutter, along a lane above every row, and down
 * just before its target.
 */
export function layoutGraph(graph: WorkItemGraph): GraphLayout {
  const xs: number[] = [];
  let x = PAD;
  for (const e of graph.events) {
    xs.push(x);
    x +=
      COL_W +
      (e.actions.some((a) => a.taskNode) ? TASK_GAP + TASK_W + TASK_TRAIL : 0);
  }
  const width = x + PAD;
  const colX = (c: number) => xs[c] ?? PAD;

  const cells: Cells[] = graph.events.map(() => ({
    event: new Set(),
    action: new Set(),
    task: new Set(),
  }));
  const events = new Map<string, PlacedEvent>();
  const actions = new Map<string, PlacedAction>();
  const tasks = new Map<string, PlacedTask>();
  /** The link each event is aligned with, and the row its line runs on. */
  const aligned = new Map<string, number>();

  const source = (key: string) => {
    const a = actions.get(key);
    if (a) return { ...a, task: false };
    const t = tasks.get(key);
    return t ? { ...t, task: true } : undefined;
  };
  /** Whether a straight line on `row` from a source in `from` reaches column `to` unblocked. */
  const free = (row: number, from: number, isTask: boolean, to: number) => {
    if (!isTask && cells[from]!.task.has(row)) return false;
    for (let c = from + 1; c < to; c++) {
      const k = cells[c]!;
      if (k.event.has(row) || k.action.has(row) || k.task.has(row)) {
        return false;
      }
    }
    return true;
  };

  for (const e of graph.events) {
    let row = 0;
    const incoming = graph.links
      .map((l, i) => ({ l, i, s: source(l.from) }))
      .filter((x) => x.l.to === e.key && x.s && x.s.column < e.column)
      .map((x) => ({
        ...x,
        rank: CAUSE_RANK[x.l.cause],
        blocked: free(x.s!.row, x.s!.column, x.s!.task, e.column) ? 0 : 1,
      }))
      .sort(
        (a, b) =>
          a.rank - b.rank ||
          a.blocked - b.blocked ||
          a.s!.row - b.s!.row ||
          a.i - b.i,
      );
    const first = incoming[0];
    if (first) {
      const s = first.s!;
      for (let r = s.row; r <= MAX_ROW; r++) {
        if (free(r, s.column, s.task, e.column)) {
          row = r;
          aligned.set(e.key, first.i);
          break;
        }
      }
    }
    const x0 = colX(e.column);
    const cx = x0 + PAD + R;
    events.set(e.key, { column: e.column, row, cx, cy: rowY(row) });
    cells[e.column]!.event.add(row);
    e.actions.forEach((a, j) => {
      const r = row + 1 + j;
      const y = rowY(r) - ACT_H / 2;
      const placed: PlacedAction = {
        column: e.column,
        row: r,
        x: x0 + ACT_X,
        y,
        w: ACT_W,
        h: ACT_H,
        spineX: cx,
      };
      actions.set(a.key, placed);
      cells[e.column]!.action.add(r);
      if (a.taskNode) {
        tasks.set(a.taskNode.key, {
          column: e.column,
          row: r,
          x: placed.x + ACT_W + TASK_GAP,
          y: rowY(r) - TASK_H / 2,
          w: TASK_W,
          h: TASK_H,
        });
        cells[e.column]!.task.add(r);
      }
    });
  }

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
    const gx = x1 + GUTTER;
    const c = CORNER;
    const onRow =
      aligned.get(l.to) === i ||
      (s.column < t.column && free(t.row, s.column, s.task, t.column));
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
    const dx = colX(t.column) - 6;
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
