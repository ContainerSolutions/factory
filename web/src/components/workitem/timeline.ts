import type { Schemas } from '@/api/client';

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
