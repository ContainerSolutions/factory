import type { Schemas } from '@/api/client';
import { asRecord } from '@/components/workitem/board';

type Run = Schemas['Run'];
type ProgressEvent = Schemas['ProgressEvent'];

/** Whether the run is still going: no `end` and no `endedAt`. */
export function isRunning(run: Run): boolean {
  return !run.status?.end && !run.status?.endedAt;
}

/** How the run ended, `ended` when it ended without saying how, `running` while it runs. */
export function runStateOf(run: Run): string {
  return run.status?.end ?? (isRunning(run) ? 'running' : 'ended');
}

/** The turn a progress event belongs to, from its free-form `data.turn`. */
export function turnOf(event: ProgressEvent): number | undefined {
  const turn = asRecord(event.data).turn;
  return typeof turn === 'number' && Number.isInteger(turn) ? turn : undefined;
}

/** Progress events carry no id, so one is the same as another when every field is. */
function keyOf(event: ProgressEvent): string {
  return JSON.stringify([
    event.at,
    event.kind,
    event.message ?? '',
    event.data ?? null,
  ]);
}

function time(at: string): number {
  const t = Date.parse(at);
  return Number.isNaN(t) ? Infinity : t;
}

/**
 * The progress events of every list (the run's ring buffer, the events streamed since, a
 * backfill after a reconnect) once each, oldest first; events at the same time keep their order.
 */
export function mergeProgress(
  ...lists: readonly (readonly ProgressEvent[])[]
): ProgressEvent[] {
  const seen = new Set<string>();
  const merged: ProgressEvent[] = [];
  for (const event of lists.flat()) {
    const key = keyOf(event);
    if (seen.has(key)) continue;
    seen.add(key);
    merged.push(event);
  }
  return merged
    .map((event, i) => ({ event, i }))
    .sort((a, b) => time(a.event.at) - time(b.event.at) || a.i - b.i)
    .map(({ event }) => event);
}

const integer = new Intl.NumberFormat('en-US');

/** A token count with thousands separators. */
export function formatCount(n: number | undefined): string {
  return n === undefined ? '' : integer.format(n);
}

/** A cost in USD; a subscription harness reports none. */
export function formatCost(cost: number | null | undefined): string {
  return typeof cost === 'number' ? `$${cost.toFixed(2)}` : 'not reported';
}
