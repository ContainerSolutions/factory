import type { Schemas } from '@/api/client';

type Agent = Schemas['Agent'];
type PoolView = Schemas['PoolView'];

/**
 * Why a pending task waits, per reason the scheduler records (the first check of the lease
 * matcher it failed: design/workstreams/dispatcher-runtime/LLD/scheduler-and-pools.md). The
 * contract types `PendingTask.reason` as a free string, so a reason not here is shown as it is.
 */
export const PENDING_REASONS: Readonly<Record<string, string>> = {
  'queue-full': 'The queue is running as many tasks as its concurrency allows.',
  budget: 'A daily budget (queue or RuleSet: runs, tokens or USD) is spent.',
  'agent-busy':
    'Every agent that could take it is at capacity, draining or offline.',
  'agent-throttled':
    'The agents that could take it are throttled by their harness (a usage window or rate limit).',
  lending:
    "No agent's lending covers it: the queue, RuleSet or harness, the hours, or the daily runs or tokens.",
  'no-capable-agent':
    'No agent in the pool offers the labels or a harness and model the task can run with.',
  'anti-affinity':
    'Only the agent that authored the PR could take this review; it waits for another agent.',
  'agent-lost':
    "The agent's lease expired without a heartbeat; the task waits to be retried.",
  'git-credential': "The task's git credentials couldn't be minted.",
};

/** What a pending reason means, or undefined for a reason the UI doesn't know. */
export function reasonText(reason: string | undefined): string | undefined {
  return reason && Object.prototype.hasOwnProperty.call(PENDING_REASONS, reason)
    ? PENDING_REASONS[reason]
    : undefined;
}

const UNITS: Record<string, number> = {
  ms: 1,
  s: 1000,
  m: 60_000,
  h: 3_600_000,
};

/** A Go duration (`30s`, `2m`, `1h30m`) in milliseconds; undefined when it doesn't parse. */
export function parseDuration(d: string | undefined): number | undefined {
  if (!d || !/^(\d+(\.\d+)?(ms|s|m|h))+$/.test(d)) return undefined;
  let total = 0;
  for (const [, n, , unit] of d.matchAll(/(\d+(\.\d+)?)(ms|s|m|h)/g)) {
    total += Number(n) * UNITS[unit!]!;
  }
  return total;
}

/** A span of time, coarse: `40 s`, `12 min`, `2 h 5 min`, `3 d`. */
export function formatSpan(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s} s`;
  const min = Math.floor(s / 60);
  if (min < 60) return `${min} min`;
  const h = Math.floor(min / 60);
  if (h < 48) return min % 60 ? `${h} h ${min % 60} min` : `${h} h`;
  return `${Math.floor(h / 24)} d`;
}

function time(at: string | null | undefined): number | undefined {
  if (!at) return undefined;
  const t = Date.parse(at);
  return Number.isNaN(t) ? undefined : t;
}

/** How long ago `at` was, as a span; undefined when it is unset or doesn't parse. */
export function ago(at: string | undefined, now: Date): string | undefined {
  const t = time(at);
  return t === undefined ? undefined : formatSpan(now.getTime() - t);
}

/** The lease policy's defaults (the scheduler's): 30 s heartbeats, a 2 min ttl. */
export const DEFAULT_HEARTBEAT_MS = 30_000;
export const DEFAULT_TTL_MS = 120_000;

/** The pool's heartbeat interval and lease ttl, with the scheduler's defaults. */
export function leasePolicy(pool: Schemas['Pool'] | undefined): {
  heartbeatMs: number;
  ttlMs: number;
} {
  const lease = pool?.spec.lease;
  return {
    heartbeatMs: parseDuration(lease?.heartbeat) ?? DEFAULT_HEARTBEAT_MS,
    ttlMs: parseDuration(lease?.ttl) ?? DEFAULT_TTL_MS,
  };
}

export type HeartbeatState = 'on time' | 'stale' | 'never';

/**
 * The agent's last heartbeat against its pool's lease ttl: `stale` once it is older than the ttl
 * (a lease it held would have expired), `never` without one.
 */
export function heartbeatOf(
  agent: Agent,
  pool: Schemas['Pool'] | undefined,
  now: Date,
): { state: HeartbeatState; ago?: string } {
  const t = time(agent.status?.lastHeartbeat);
  if (t === undefined) return { state: 'never' };
  const age = now.getTime() - t;
  return {
    state: age > leasePolicy(pool).ttlMs ? 'stale' : 'on time',
    ago: formatSpan(age),
  };
}

/**
 * The agent's throttle window: `throttled` until a time still ahead, `passed` when that time has
 * gone (the scheduler counts the agent as Ready again), `none` without one.
 */
export function throttleOf(
  agent: Agent,
  now: Date,
): { state: 'none' | 'throttled' | 'passed'; until?: string; in?: string } {
  const until = agent.status?.throttledUntil ?? undefined;
  const t = time(until);
  if (!until || t === undefined) return { state: 'none' };
  const left = t - now.getTime();
  return left > 0
    ? { state: 'throttled', until, in: formatSpan(left) }
    : { state: 'passed', until };
}

/**
 * How many tasks the agent may hold at once: the smaller of `lending.maxConcurrent` and the
 * pool's `lease.maxTasksPerAgent` that are set, else 1 (the scheduler's row 4).
 */
export function capacityOf(
  agent: Agent,
  pool: Schemas['Pool'] | undefined,
): number {
  const limits = [
    agent.spec.lending?.maxConcurrent,
    pool?.spec.lease?.maxTasksPerAgent,
  ].filter((n): n is number => typeof n === 'number' && n > 0);
  return limits.length ? Math.min(...limits) : 1;
}

const integer = new Intl.NumberFormat('en-US');

/** A count with thousands separators. */
export function formatCount(n: number): string {
  return integer.format(n);
}

/** Usage against a daily cap: `4 of 30`, or just `4` without a cap. */
export function ofCap(used: number | undefined, cap: number | undefined) {
  const u = formatCount(used ?? 0);
  return typeof cap === 'number' ? `${u} of ${formatCount(cap)}` : u;
}

/** A queue's daily budget, e.g. `80 runs, $20.00 a day`; empty when it has none. */
export function budgetText(budget: Schemas['Budget'] | undefined): string {
  const daily = budget?.daily;
  if (!daily) return '';
  const parts = [
    typeof daily.runs === 'number' ? `${formatCount(daily.runs)} runs` : '',
    typeof daily.tokens === 'number'
      ? `${formatCount(daily.tokens)} tokens`
      : '',
    typeof daily.usd === 'number' ? `$${daily.usd.toFixed(2)}` : '',
  ].filter(Boolean);
  return parts.length ? `${parts.join(', ')} a day` : '';
}

/** One pool's section: the pool (undefined for agents of a pool not listed) and its agents. */
export interface PoolGroup {
  name: string;
  pool?: Schemas['Pool'];
  agents: Agent[];
  /** Names the pool lists that `GET /agents` didn't return. */
  missing: string[];
}

/**
 * The agents by pool, in the pools' order. An agent belongs to the pool its spec names, and to a
 * pool that lists it (a pool's list can lag an agent that registered since). Agents of a pool not
 * listed get a group of their own, after the listed pools.
 */
export function poolGroups(
  pools: readonly PoolView[],
  agents: readonly Agent[],
): PoolGroup[] {
  const byName = new Map(agents.map((a) => [a.metadata.name, a]));
  const placed = new Set<string>();
  const groups: PoolGroup[] = pools.map((view) => {
    const name = view.pool.metadata.name;
    const members = agents.filter(
      (a) => a.spec.pool === name || view.agents.includes(a.metadata.name),
    );
    for (const a of members) placed.add(a.metadata.name);
    return {
      name,
      pool: view.pool,
      agents: members,
      missing: view.agents.filter((n) => !byName.has(n)),
    };
  });
  const rest = new Map<string, Agent[]>();
  for (const a of agents) {
    if (placed.has(a.metadata.name)) continue;
    rest.set(a.spec.pool, [...(rest.get(a.spec.pool) ?? []), a]);
  }
  for (const [name, members] of rest) {
    groups.push({ name, agents: members, missing: [] });
  }
  return groups;
}

/** The queues in the scheduler's order: priority, highest first, then name. */
export function sortQueues(
  queues: readonly Schemas['QueueView'][],
): Schemas['QueueView'][] {
  return [...queues].sort(
    (a, b) =>
      (b.queue.spec.priority ?? 0) - (a.queue.spec.priority ?? 0) ||
      a.queue.metadata.name.localeCompare(b.queue.metadata.name),
  );
}
