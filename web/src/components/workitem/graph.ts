import type { Schemas } from '@/api/client';
import { railVerdictLabel } from '@/components/shared/format';
import { asRecord } from './board';

type Detail = Schemas['WorkItemDetail'];
type Event = Schemas['Event'];
type Decision = Schemas['Decision'];
type Task = Schemas['Task'];
type Run = Schemas['Run'];
type Effect = Schemas['Effect'];

/** Actions no LLM takes part in: drawn as dotted rectangles. A dispatch (a task with a run) is solid. */
const DETERMINISTIC = new Set([
  'set',
  'effect',
  'notify',
  'cancel',
  'link',
  'debounce',
  'requestApproval',
]);

/** A `Decision.actions` entry, split: its kind and what it acted on. */
export interface ActionSummary {
  kind: string;
  /** The rest of the summary: `plan-epic → poc`, a state key, a template, an op, a ref. */
  subject: string;
}

/**
 * Splits a decision's action summary (`rules.ActionIntent.String()`: `dispatch plan-epic → poc`,
 * `set plan`, `notify <template>`, `cancel <spec>`, `link <ref>`, `effect <op>`,
 * `debounce <key>`) into its kind, the first word, and its subject, the rest. A bare kind has no
 * subject. `unsupported requestApproval`, how the engine records a requestApproval, is one.
 */
export function parseAction(summary: string): ActionSummary {
  const s = summary.trim();
  const i = s.search(/\s/);
  const kind = i < 0 ? s : s.slice(0, i);
  const subject = i < 0 ? '' : s.slice(i + 1).trim();
  if (kind === 'unsupported' && subject === 'requestApproval') {
    return { kind: 'requestApproval', subject: '' };
  }
  return { kind, subject };
}

/** The TaskSpec a dispatch summary names: `plan-epic` of `plan-epic → poc`. */
export function dispatchSpec(subject: string): string {
  return subject.split('→')[0]!.trim();
}

/** The label of an Effect a rule made: the rule's name (controllers.LabelRule). */
export const LABEL_RULE = 'factory.ccf.dev/rule';

/** Task phases that are not over yet. */
export const IN_FLIGHT = new Set([
  'Pending',
  'Leased',
  'Running',
  'Cancelling',
]);

/** One action a fired decision took, joined to what it created. */
export interface GraphAction {
  key: string;
  /** The action's kind, the first word of its `Decision.actions` summary: dispatch, effect, set, … */
  kind: string;
  deterministic: boolean;
  /** The decision's index in `workItem.status.decisions`. */
  decisionIndex: number;
  /** The action's index in its decision's actions: the `<i>` of the rule controller's key. */
  index: number;
  decision: Decision;
  /** `<RuleSet>/<rule>`, the arrow's label. */
  rule: string;
  /**
   * What it acted on: the TaskSpec of a dispatch, the op and target of an effect, else the rest
   * of its summary (a state key, a template, a ref).
   */
  subject: string;
  /** Its current state, as text: a task phase, applied/failed, an approval phase, a rail verdict. */
  state: string;
  task?: Task;
  /** The task's runs, oldest attempt first. */
  runs: Run[];
  /** The rule's own effect, for an effect action. */
  effect?: Effect;
  /** The effects the task's runs made, for a dispatch. */
  taskEffects: Effect[];
  approval?: Schemas['ApprovalRef'];
  /** The task a dispatch created, drawn as its own node after the action. */
  taskNode?: GraphTask;
}

/** The task a dispatch created: its TaskSpec, phase, attempts and the latest run's outcome. */
export interface GraphTask {
  key: string;
  task: Task;
  /** The TaskSpec's name. */
  spec: string;
  /** Its phase (Pending when not set yet), coloured as the LLM tasks tab colours it. */
  phase: string;
  attempts: number;
  /** The latest run's outcome, else its state; none before the first run. */
  outcome?: string;
  /** Why a task with no run yet is waiting. */
  reason?: string;
}

/** One event, in time order, with the actions its fired decisions took. */
export interface GraphEvent {
  key: string;
  id: string;
  /** The event's type, or `event` for one the work item no longer holds (only a decision names it). */
  type: string;
  at?: string;
  event?: Event;
  column: number;
  actions: GraphAction[];
}

/**
 * What a link says caused what:
 * - `task`: a dispatch created the task;
 * - `run`: the task's run completed (or failed) as the event;
 * - `effect`: the event names an effect of the action or task (`data.effect`);
 * - `notify`: the event names a notify action's key;
 * - `tracking`: the factory's tracking comment after a state change (inferred);
 * - `inferred`: the same object, later, by the factory (ref and time order).
 */
export type GraphCause =
  | 'task'
  | 'run'
  | 'effect'
  | 'notify'
  | 'tracking'
  | 'inferred';

/**
 * An action or task → what it caused: a task, or a later event. Reliable (provenance, a named
 * key) or inferred (`tracking`, `inferred`).
 */
export interface GraphLink {
  from: string;
  to: string;
  inferred: boolean;
  cause: GraphCause;
}

export interface WorkItemGraph {
  events: GraphEvent[];
  links: GraphLink[];
}

function time(at: string | undefined): number {
  const t = at ? Date.parse(at) : NaN;
  return Number.isNaN(t) ? Infinity : t;
}

/** The first rail that held the action back, as its verdict label (`Suppressed`, `Deferred`). */
function railState(decision: Decision): string | undefined {
  const r = (decision.rails ?? []).find((v) => v.verdict !== 'allowed');
  return r ? railVerdictLabel(r.verdict) : undefined;
}

/** The run a factory.run.* event reports (`facts.run.id`). */
function runIdOf(event: Event): string | undefined {
  const id = asRecord(asRecord(event.data.facts).run).id;
  return typeof id === 'string' ? id : undefined;
}

/** The tasks a decision dispatched: their provenance names its event, RuleSet and rule. */
function tasksOf(decision: Decision, tasks: readonly Task[]): Task[] {
  return tasks
    .filter(
      (t) =>
        t.metadata.provenance?.event === decision.event &&
        t.metadata.provenance?.ruleSet === decision.ruleSet &&
        t.metadata.provenance?.rule === decision.rule,
    )
    .sort((a, b) => time(a.metadata.createdAt) - time(b.metadata.createdAt));
}

/**
 * The effects a decision applied itself: no task, and an idempotency key of
 * `evt/<event>/<rule>/<i>` with the rule's label.
 */
function ruleEffectsOf(
  decision: Decision,
  effects: readonly Effect[],
): Effect[] {
  const prefix = `evt/${decision.event}/`;
  return effects
    .filter(
      (e) =>
        !e.spec.task &&
        !!decision.event &&
        e.spec.idempotencyKey.startsWith(prefix) &&
        (e.metadata.labels?.[LABEL_RULE] ?? decision.rule) === decision.rule,
    )
    .sort((a, b) => a.spec.idempotencyKey.localeCompare(b.spec.idempotencyKey));
}

/** A dispatch's task as a node: its TaskSpec, phase, attempts, latest outcome or why it waits. */
function taskNodeOf(task: Task, runs: readonly Run[]): GraphTask {
  const latest = runs[runs.length - 1];
  const outcome =
    latest?.status?.result?.outcome ??
    (latest ? (latest.status?.end ?? 'running') : undefined);
  const node: GraphTask = {
    key: `task/${task.metadata.id}`,
    task,
    spec: task.spec.taskSpec.name,
    phase: task.status?.phase ?? 'Pending',
    attempts: task.status?.attempts ?? runs.length,
  };
  if (outcome) node.outcome = outcome;
  else if (task.status?.reason) node.reason = task.status.reason;
  return node;
}

/** A factory comment on the work item that names no effect: the tracking comment's projection. */
function isTrackingComment(e: GraphEvent): boolean {
  return (
    e.type === 'issue.commented' &&
    !!e.event?.data.actor?.isFactory &&
    !e.event.data.effect
  );
}

/**
 * The state-changing actions (`set`, `link`) of the latest fired decision at or before the
 * event, leaving out the event's own decisions.
 */
function latestStateChange(
  actions: readonly GraphAction[],
  event: GraphEvent,
): GraphAction[] {
  const at = time(event.at);
  const changes = actions.filter(
    (a) =>
      (a.kind === 'set' || a.kind === 'link') &&
      a.decision.event !== event.id &&
      time(a.decision.at) <= at,
  );
  const last = changes.reduce<GraphAction | undefined>(
    (best, a) =>
      !best ||
      time(a.decision.at) > time(best.decision.at) ||
      (time(a.decision.at) === time(best.decision.at) &&
        a.decisionIndex > best.decisionIndex)
        ? a
        : best,
    undefined,
  );
  return last
    ? changes.filter((a) => a.decisionIndex === last.decisionIndex)
    : [];
}

function effectSubject(e: Effect): string {
  return `${e.spec.op} ${e.spec.target.ref}`;
}

/**
 * The work item as a graph: each event (a column, in time order), the actions of its fired
 * decisions, each dispatch's task, and links to what they caused.
 *
 * Reliable links:
 * - a dispatch to its task, and the task to its runs' `factory.run.completed` / `.failed` events
 *   (the task's provenance and `facts.run.id`);
 * - an effect (a rule's own from its action, or one its task made from the task) to the event
 *   whose `data.effect` is the effect's idempotency key (the provider reads it from the factory's
 *   marker on the object the effect created);
 * - a notify action to the comment whose `data.effect` is its key, `evt/<event>/<rule>/<i>`
 *   (a notify keeps no Effect record).
 *
 * Inferred links:
 * - the tracking comment (a factory `issue.commented` naming no effect) from the `set` and `link`
 *   actions of the latest fired decision before it;
 * - an effect, or a task whose run made effects or reported a PR, to the first later provider
 *   event the factory caused on the same object (by ref and time) that nothing else claims, e.g.
 *   a push's pr.updated.
 *
 * Decisions that didn't fire take no actions, so they aren't in the graph.
 */
export function graphOf(detail: Detail): WorkItemGraph {
  const decisions = detail.workItem.status?.decisions ?? [];
  const byId = new Map<string, GraphEvent>();
  for (const e of detail.events) {
    if (!byId.has(e.id)) {
      byId.set(e.id, {
        key: `event/${e.id}`,
        id: e.id,
        type: e.type,
        at: e.time,
        event: e,
        column: 0,
        actions: [],
      });
    }
  }
  for (const d of decisions) {
    if (d.event && !byId.has(d.event)) {
      byId.set(d.event, {
        key: `event/${d.event}`,
        id: d.event,
        type: 'event',
        at: d.at,
        column: 0,
        actions: [],
      });
    }
  }
  const events = [...byId.values()].sort(
    (a, b) => time(a.at) - time(b.at) || a.id.localeCompare(b.id),
  );
  events.forEach((e, i) => (e.column = i));

  const approvals = [...(detail.workItem.status?.approvals ?? [])];
  let approvalIndex = 0;
  decisions.forEach((decision, decisionIndex) => {
    if (!decision.fired || !decision.event) return;
    const node = byId.get(decision.event)!;
    const tasks = tasksOf(decision, detail.tasks);
    const ruleEffects = ruleEffectsOf(decision, detail.effects);
    const held = railState(decision);
    (decision.actions ?? []).forEach((summary, j) => {
      const { kind, subject } = parseAction(summary);
      const action: GraphAction = {
        key: `action/${decisionIndex}/${j}`,
        index: j,
        kind,
        deterministic: DETERMINISTIC.has(kind),
        decisionIndex,
        decision,
        rule: `${decision.ruleSet ?? '?'}/${decision.rule ?? '?'}`,
        subject,
        state: held ?? 'done',
        runs: [],
        taskEffects: [],
      };
      if (kind === 'dispatch') {
        // The first task left that the summary's TaskSpec names (any, for a bare kind).
        const spec = dispatchSpec(subject);
        action.subject = spec;
        const i = tasks.findIndex(
          (t) => !spec || t.spec.taskSpec.name === spec,
        );
        const task = i < 0 ? undefined : tasks.splice(i, 1)[0];
        if (task) {
          action.task = task;
          action.subject = task.spec.taskSpec.name;
          action.state = task.status?.phase ?? 'Pending';
          action.runs = detail.runs
            .filter((r) => r.spec.task === task.metadata.id)
            .sort((a, b) => a.spec.attempt - b.spec.attempt);
          action.taskEffects = detail.effects.filter(
            (e) => e.spec.task === task.metadata.id,
          );
          action.taskNode = taskNodeOf(task, action.runs);
        } else action.state = held ?? 'no task';
      } else if (kind === 'effect') {
        // The first rule effect left with the summary's op (any, for a bare kind).
        const i = ruleEffects.findIndex(
          (e) => !subject || e.spec.op === subject,
        );
        const effect = i < 0 ? undefined : ruleEffects.splice(i, 1)[0];
        if (effect) {
          action.effect = effect;
          action.subject = effectSubject(effect);
          action.state = effect.status?.result?.error ? 'failed' : 'applied';
        } else action.state = held ?? 'not recorded';
      } else if (kind === 'requestApproval') {
        const approval = approvals[approvalIndex++];
        if (approval) {
          action.approval = approval;
          action.subject = approval.name;
          action.state = approval.phase;
        }
      }
      node.actions.push(action);
    });
  });

  const links: GraphLink[] = [];
  const linked = new Set<string>();
  const link = (from: string, to: string, cause: GraphCause) => {
    if (linked.has(`${from}->${to}`)) return;
    linked.add(`${from}->${to}`);
    const inferred = cause === 'tracking' || cause === 'inferred';
    links.push({ from, to, inferred, cause });
  };
  const actions = events.flatMap((node) => node.actions);
  // Exact links first: a dispatch to its task, the task to its runs' completions, an action or
  // task to the events whose data.effect names one of its effects, and a notify action to the
  // comments whose data.effect is its key. What they claim isn't guessed at again below.
  const claimed = new Set<string>();
  for (const a of actions) {
    const source = a.taskNode?.key ?? a.key;
    if (a.taskNode) link(a.key, a.taskNode.key, 'task');
    for (const run of a.runs) {
      const done = events.find(
        (e) =>
          !!e.event &&
          (e.type === 'factory.run.completed' ||
            e.type === 'factory.run.failed') &&
          runIdOf(e.event) === run.metadata.name,
      );
      if (done) link(source, done.key, 'run');
    }
    const keys = new Set(
      [a.effect, ...a.taskEffects].flatMap((f) =>
        f ? [f.spec.idempotencyKey] : [],
      ),
    );
    const notifyKey =
      a.kind === 'notify' && a.decision.event && a.decision.rule
        ? `evt/${a.decision.event}/${a.decision.rule}/${a.index}`
        : undefined;
    for (const e of events) {
      const named = e.event?.data.effect;
      if (!named) continue;
      if (keys.has(named)) {
        link(
          a.effect && named === a.effect.spec.idempotencyKey ? a.key : source,
          e.key,
          'effect',
        );
        claimed.add(e.key);
      } else if (
        notifyKey &&
        (named === notifyKey || named.startsWith(`${notifyKey}/`))
      ) {
        link(a.key, e.key, 'notify');
        claimed.add(e.key);
      }
    }
  }
  // The work item's tracking comment (the controller's projection after the state changed):
  // a factory comment naming no effect, from the state-changing actions of the latest fired
  // decision before it.
  for (const e of events) {
    if (!isTrackingComment(e)) continue;
    const by = latestStateChange(actions, e);
    for (const a of by) link(a.key, e.key, 'tracking');
    if (by.length) claimed.add(e.key);
  }
  // Then the heuristic, for what nothing names: an event already linked isn't guessed at again
  // (e.g. a push's pr.updated still is; the pr.opened its pr.open made isn't), and an event gets
  // one guess at most: from the cause closest before it (the latest action or run).
  const guesses = new Map<string, { from: string; after: number }>();
  for (const node of events) {
    for (const a of node.actions) {
      const guess = inferredEvent(a, node, events, claimed);
      if (!guess) continue;
      const best = guesses.get(guess.event.key);
      if (!best || guess.after > best.after) {
        guesses.set(guess.event.key, {
          from: a.taskNode?.key ?? a.key,
          after: guess.after,
        });
      }
    }
  }
  for (const [to, { from }] of guesses) link(from, to, 'inferred');
  return { events, links };
}

/**
 * The first provider event after the action (`after`: the effect's or the latest run's start
 * time) that the factory caused on an object the action
 * touched (the effect's target, or a dispatch's run effects' targets and its Result's
 * `data.pr`), leaving out the events already linked by something that names them.
 */
function inferredEvent(
  a: GraphAction,
  from: GraphEvent,
  events: readonly GraphEvent[],
  claimed: ReadonlySet<string>,
): { event: GraphEvent; after: number } | undefined {
  const refs = new Set<string>();
  let after = time(a.decision.at);
  if (a.effect) {
    refs.add(a.effect.spec.target.ref);
    after = time(a.effect.status?.at ?? a.effect.metadata.createdAt);
  }
  if (a.task) {
    for (const e of a.taskEffects) refs.add(e.spec.target.ref);
    for (const r of a.runs) {
      const pr = asRecord(r.status?.result?.data).pr;
      if (typeof pr === 'string') refs.add(pr);
    }
    const last = a.runs[a.runs.length - 1];
    const started = last?.status?.startedAt ?? a.task.metadata.createdAt;
    if (started) after = time(started);
  }
  if (!refs.size || after === Infinity) return undefined;
  const event = events.find(
    (e) =>
      e !== from &&
      !claimed.has(e.key) &&
      !!e.event &&
      !e.type.startsWith('factory.') &&
      !!e.event.data.actor?.isFactory &&
      refs.has(e.event.data.object?.ref ?? '') &&
      time(e.at) >= after,
  );
  return event ? { event, after } : undefined;
}

/**
 * The graph without its idle events, for the Graph tab: an event no fired decision acted on (no
 * actions) and nothing links to. Columns are renumbered over the events that remain, so there
 * are no gaps; the Timeline still lists every event. Links run from actions and tasks, so an
 * event with no actions has none going out.
 */
export function withoutIdle(g: WorkItemGraph): {
  graph: WorkItemGraph;
  hidden: number;
} {
  const linked = new Set(g.links.map((l) => l.to));
  const kept = g.events.filter((e) => e.actions.length || linked.has(e.key));
  return {
    graph: {
      events: kept.map((e, column) => ({ ...e, column })),
      links: g.links,
    },
    hidden: g.events.length - kept.length,
  };
}
