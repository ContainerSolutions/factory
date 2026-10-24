import type { Schemas } from '@/api/client';
import type { GraphAction, GraphEvent, WorkItemGraph } from './graph';

export type TimelineEntry =
  | { kind: 'event'; at?: string; key: string; event: Schemas['Event'] }
  | {
      kind: 'decision';
      at?: string;
      key: string;
      /** The decision's index in `workItem.status.decisions`. */
      index: number;
      decision: Schemas['Decision'];
    }
  | { kind: 'task'; at?: string; key: string; task: Schemas['Task'] }
  | { kind: 'run'; at?: string; key: string; run: Schemas['Run'] }
  | { kind: 'effect'; at?: string; key: string; effect: Schemas['Effect'] };

// At the same instant, an event causes a decision, which creates a task, which gets a run, which
// performs an effect.
const RANK: Record<TimelineEntry['kind'], number> = {
  event: 0,
  decision: 1,
  task: 2,
  run: 3,
  effect: 4,
};

function time(at: string | undefined): number {
  const t = at ? Date.parse(at) : NaN;
  return Number.isNaN(t) ? Infinity : t;
}

/**
 * The work item's events, decisions, tasks, runs and effects merged into one timeline, oldest
 * first. Entries without a time go last.
 */
export function timelineOf(detail: Schemas['WorkItemDetail']): TimelineEntry[] {
  const entries: TimelineEntry[] = [
    ...detail.events.map(
      (event): TimelineEntry => ({
        kind: 'event',
        at: event.time,
        key: `event/${event.id}`,
        event,
      }),
    ),
    ...(detail.workItem.status?.decisions ?? []).map(
      (decision, index): TimelineEntry => ({
        kind: 'decision',
        at: decision.at,
        key: `decision/${index}`,
        index,
        decision,
      }),
    ),
    ...detail.tasks.map(
      (task): TimelineEntry => ({
        kind: 'task',
        at: task.metadata.createdAt,
        key: `task/${task.metadata.id}`,
        task,
      }),
    ),
    ...detail.runs.map(
      (run): TimelineEntry => ({
        kind: 'run',
        at: run.status?.startedAt ?? run.metadata.createdAt,
        key: `run/${run.metadata.name}`,
        run,
      }),
    ),
    ...detail.effects.map(
      (effect): TimelineEntry => ({
        kind: 'effect',
        at: effect.status?.at ?? effect.metadata.createdAt,
        key: `effect/${effect.metadata.name}`,
        effect,
      }),
    ),
  ];
  return entries
    .map((entry, i) => ({ entry, i }))
    .sort(
      (a, b) =>
        time(a.entry.at) - time(b.entry.at) ||
        RANK[a.entry.kind] - RANK[b.entry.kind] ||
        a.i - b.i,
    )
    .map(({ entry }) => entry);
}

/**
 * The catalog commit a decision was made at. Decisions don't carry it, so it is read from the
 * provenance of a task the decision created (same event, RuleSet and rule); undefined otherwise.
 */
export function catalogSHAFor(
  decision: Schemas['Decision'],
  tasks: Schemas['Task'][],
): string | undefined {
  return tasks.find(
    (t) =>
      t.metadata.provenance?.event === decision.event &&
      t.metadata.provenance?.ruleSet === decision.ruleSet &&
      t.metadata.provenance?.rule === decision.rule,
  )?.metadata.provenance?.catalogSHA;
}

/** One decision of an event row, by its index in `workItem.status.decisions`. */
export interface RowDecision {
  index: number;
  decision: Schemas['Decision'];
  /** The graph's actions for a fired decision; none for one that didn't fire. */
  actions: GraphAction[];
}

/** One event of the collapsible timeline: its fired decisions first, the others apart. */
export interface EventRow {
  key: string;
  node: GraphEvent;
  fired: RowDecision[];
  notFired: RowDecision[];
}

/** The work item's events in time order, each with its decisions split into fired and not. */
export function eventRows(
  detail: Schemas['WorkItemDetail'],
  graph: WorkItemGraph,
): EventRow[] {
  const decisions = detail.workItem.status?.decisions ?? [];
  return graph.events.map((node) => {
    const row: EventRow = { key: node.key, node, fired: [], notFired: [] };
    decisions.forEach((decision, index) => {
      if (decision.event !== node.id) return;
      const actions = node.actions.filter((a) => a.decisionIndex === index);
      (decision.fired ? row.fired : row.notFired).push({
        index,
        decision,
        actions,
      });
    });
    return row;
  });
}

/** The tasks, runs and effects no fired decision's action accounts for, in time order. */
export function unattached(
  detail: Schemas['WorkItemDetail'],
  graph: WorkItemGraph,
): TimelineEntry[] {
  const seen = new Set<string>();
  for (const e of graph.events) {
    for (const a of e.actions) {
      if (a.task) seen.add(`task/${a.task.metadata.id}`);
      for (const r of a.runs) seen.add(`run/${r.metadata.name}`);
      for (const f of [a.effect, ...a.taskEffects]) {
        if (f) seen.add(`effect/${f.metadata.name}`);
      }
    }
  }
  return timelineOf(detail).filter(
    (e) => e.kind !== 'event' && e.kind !== 'decision' && !seen.has(e.key),
  );
}
