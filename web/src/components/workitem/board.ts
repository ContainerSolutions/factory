import type { Schemas } from '@/api/client';

type WorkItem = Schemas['WorkItem'];

/** A board lane, in display order: the `needs-human` lane comes first. */
export type LaneId = 'needs-human' | 'running' | 'waiting' | 'done';

export const LANES: readonly { id: LaneId; title: string }[] = [
  { id: 'needs-human', title: 'Needs human' },
  { id: 'running', title: 'Running' },
  { id: 'waiting', title: 'Waiting' },
  { id: 'done', title: 'Done (last 7 days)' },
];

/*
 * The contract has no derived state for a work item, so the lane is derived from its state keys,
 * which rules set by catalog convention: lifecycle words (`set: { review: running }`) and run
 * outcomes (`set: { review: "=run.outcome" }`). Each string value is classified against the
 * vocabularies below; a value in none of them (a train ref, a manifest name, an id) is ignored.
 * The PoC catalog is internal/catalog/testdata/poc/ (TaskSpecs' `output.outcomes`, rules' `set:`);
 * the triage TaskSpec is design/03's (internal/apis/v1alpha1/testdata/examples/03-09-taskspec.yaml).
 */

/**
 * A human has to act. `needs-human`: the docs-review and merge outcomes; `blocked`: the
 * release-docs, implement and merge outcomes. Both are what the shared `escalate-needs-human`
 * rule escalates on.
 */
export const NEEDS_HUMAN_VALUES: readonly string[] = ['needs-human', 'blocked'];

/**
 * A task is under way: the values rules set when they dispatch. `running`: review-whole-stack and
 * review-release-docs (`review`); `planning`: plan-maintainer-epics (`plan`); `drafting`:
 * docs-on-release-train (`release`).
 */
export const RUNNING_VALUES: readonly string[] = [
  'running',
  'planning',
  'drafting',
];

/**
 * A terminal result. `merged`: the merge outcome; `published`: announce-release-docs
 * (`release`); `approved`: the docs-review outcome; `no-docs-change`: the release-docs outcome
 * for a release that needs no docs; `triaged`, `duplicate`, `not-actionable`: the triage outcomes.
 */
export const DONE_VALUES: readonly string[] = [
  'merged',
  'published',
  'approved',
  'no-docs-change',
  'triaged',
  'duplicate',
  'not-actionable',
];

/**
 * A non-terminal outcome: more work follows, from a rule or a person. `pr-opened`, `pr-updated`:
 * the implement and release-docs outcomes; `changes-requested`, `commented`: the docs-review
 * outcomes; `labelled-for-merge`, `rebased-with-conflicts`: the merge outcomes; `decomposed`,
 * `single`: the plan-epic outcomes; `needs-info`: the implement, plan-epic and triage outcome;
 * `promote`: the triage outcome.
 */
export const OPEN_VALUES: readonly string[] = [
  'pr-opened',
  'pr-updated',
  'changes-requested',
  'commented',
  'labelled-for-merge',
  'rebased-with-conflicts',
  'decomposed',
  'single',
  'needs-info',
  'promote',
];

/** How long a done work item stays on the board after its last activity. */
export const DONE_WINDOW_MS = 7 * 24 * 60 * 60 * 1000;

/** An object as a string-keyed record; anything else as an empty one. */
export function asRecord(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

/**
 * The work item's state keys. The contract types `status.state` as an empty object
 * (`type: object` with no `additionalProperties`), so it is read as a plain record here.
 */
export function stateOf(item: WorkItem): Record<string, unknown> {
  return asRecord(item.status?.state);
}

/**
 * The lane a work item belongs in: needs-human for any needs-human value or a pending approval;
 * running for any running value; done for at least one done value and no needs-human, running or
 * open value; waiting otherwise. Values in no vocabulary don't count.
 */
export function laneOf(item: WorkItem): LaneId {
  const values = Object.values(stateOf(item)).filter(
    (v): v is string => typeof v === 'string',
  );
  const any = (vocabulary: readonly string[]) =>
    values.some((v) => vocabulary.includes(v));
  const pending = (item.status?.approvals ?? []).some(
    (a) => a.phase === 'Pending',
  );
  if (pending || any(NEEDS_HUMAN_VALUES)) return 'needs-human';
  if (any(RUNNING_VALUES)) return 'running';
  if (any(DONE_VALUES) && !any(OPEN_VALUES)) return 'done';
  return 'waiting';
}

/**
 * The work item's last known activity: its newest decision, else its creation. The contract has
 * no `updatedAt`, so this is the closest it gets.
 */
export function lastActivity(item: WorkItem): Date | undefined {
  const times = [
    item.metadata.createdAt,
    ...(item.status?.decisions ?? []).map((d) => d.at),
  ]
    .filter((t): t is string => !!t)
    .map((t) => Date.parse(t))
    .filter((t) => !Number.isNaN(t));
  return times.length ? new Date(Math.max(...times)) : undefined;
}

/** The repository of a provider ref: `owner/repo#42` and `owner/repo@main` → `owner/repo`. */
export function repoOf(ref: string): string | undefined {
  return /^([^/#@\s]+\/[^/#@\s]+)(?:[#@].*)?$/.exec(ref)?.[1];
}

/** The primary link, else the first. */
export function primaryLink(item: WorkItem): Schemas['Link'] | undefined {
  const links = item.spec?.links ?? [];
  return links.find((l) => l.primary) ?? links[0];
}

/** The work item's display title: its primary link's ref, else its name. */
export function titleOf(item: WorkItem): string {
  return primaryLink(item)?.ref ?? item.metadata.name;
}

/** The board's filters; an empty value doesn't filter. */
export interface BoardFilters {
  ruleSet: string;
  provider: string;
  repo: string;
  taskSpec: string;
}

export const NO_FILTERS: BoardFilters = {
  ruleSet: '',
  provider: '',
  repo: '',
  taskSpec: '',
};

/** The values each filter can take for a work item. */
function facetsOf(item: WorkItem): Record<keyof BoardFilters, string[]> {
  const links = item.spec?.links ?? [];
  return {
    ruleSet: (item.status?.decisions ?? [])
      .map((d) => d.ruleSet)
      .filter((v): v is string => !!v),
    provider: links.map((l) => l.provider),
    repo: links.map((l) => repoOf(l.ref)).filter((v): v is string => !!v),
    taskSpec: Object.keys(item.status?.counters ?? {}),
  };
}

/** Every value each filter can take across the work items, sorted. */
export function facets(
  items: WorkItem[],
): Record<keyof BoardFilters, string[]> {
  const sets = {
    ruleSet: new Set<string>(),
    provider: new Set<string>(),
    repo: new Set<string>(),
    taskSpec: new Set<string>(),
  };
  for (const item of items) {
    const f = facetsOf(item);
    for (const key of Object.keys(sets) as (keyof BoardFilters)[]) {
      f[key].forEach((v) => sets[key].add(v));
    }
  }
  return {
    ruleSet: [...sets.ruleSet].sort(),
    provider: [...sets.provider].sort(),
    repo: [...sets.repo].sort(),
    taskSpec: [...sets.taskSpec].sort(),
  };
}

/** Whether the work item passes every set filter. */
export function matches(item: WorkItem, filters: BoardFilters): boolean {
  const f = facetsOf(item);
  return (Object.keys(f) as (keyof BoardFilters)[]).every(
    (key) => !filters[key] || f[key].includes(filters[key]),
  );
}

export interface Lane {
  id: LaneId;
  title: string;
  items: WorkItem[];
  /** Done work items left off the lane because their last activity is outside the window. */
  hidden: number;
}

/** The filtered work items, by lane, in lane order; the input order is kept within a lane. */
export function boardLanes(
  items: WorkItem[],
  filters: BoardFilters,
  now: Date,
): Lane[] {
  const lanes = LANES.map((l) => ({
    ...l,
    items: [] as WorkItem[],
    hidden: 0,
  }));
  const byId = new Map(lanes.map((l) => [l.id, l]));
  for (const item of items) {
    if (!matches(item, filters)) continue;
    const lane = byId.get(laneOf(item))!;
    if (lane.id === 'done') {
      const at = lastActivity(item);
      if (!at || now.getTime() - at.getTime() > DONE_WINDOW_MS) {
        lane.hidden++;
        continue;
      }
    }
    lane.items.push(item);
  }
  return lanes;
}
