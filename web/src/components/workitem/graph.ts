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
  'requestApproval',
]);

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
  /** The action's kind, as `Decision.actions` names it: dispatch, effect, set, … */
  kind: string;
  deterministic: boolean;
  /** The decision's index in `workItem.status.decisions`. */
  decisionIndex: number;
  decision: Decision;
  /** `<RuleSet>/<rule>`, the arrow's label. */
  rule: string;
  /** What it acted on: the TaskSpec of a dispatch, the op and target of an effect. */
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

/** An action → a later event it caused: reliable (provenance) or inferred (ref and time order). */
export interface GraphLink {
  from: string;
  to: string;
  inferred: boolean;
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

function effectSubject(e: Effect): string {
  return `${e.spec.op} ${e.spec.target.ref}`;
}

/**
 * The work item as a graph: each event (a column, in time order), the actions of its fired
 * decisions, and links from an action to the events it caused. Reliable links: a dispatch to
 * its runs' `factory.run.completed` / `.failed` events (the task's provenance and
 * `facts.run.id`), and an effect (a rule's own, or one its task made) to the event whose
 * `data.effect` is the effect's idempotency key (the provider reads it from the factory's
 * marker on the object the effect created). Inferred links: an effect, or a dispatch whose run
 * made effects or reported a PR, to the first later provider event the factory caused on the
 * same object (by ref and time) that no effect is exactly linked to, e.g. a push's pr.updated.
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
    let dispatches = 0;
    let effects = 0;
    const held = railState(decision);
    (decision.actions ?? []).forEach((kind, j) => {
      const action: GraphAction = {
        key: `action/${decisionIndex}/${j}`,
        kind,
        deterministic: DETERMINISTIC.has(kind),
        decisionIndex,
        decision,
        rule: `${decision.ruleSet ?? '?'}/${decision.rule ?? '?'}`,
        subject: '',
        state: held ?? 'done',
        runs: [],
        taskEffects: [],
      };
      if (kind === 'dispatch') {
        const task = tasks[dispatches++];
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
        } else action.state = held ?? 'no task';
      } else if (kind === 'effect') {
        const effect = ruleEffects[effects++];
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
  const linked = new Map<string, Set<string>>();
  const link = (from: string, to: string, inferred: boolean) => {
    const seen = linked.get(from) ?? new Set<string>();
    if (seen.has(to)) return;
    seen.add(to);
    linked.set(from, seen);
    links.push({ from, to, inferred });
  };
  // Exact links first: the run completions, and the events whose data.effect names one of the
  // action's effects by its idempotency key.
  const exact = new Set<string>();
  for (const node of events) {
    for (const a of node.actions) {
      for (const run of a.runs) {
        const done = events.find(
          (e) =>
            !!e.event &&
            (e.type === 'factory.run.completed' ||
              e.type === 'factory.run.failed') &&
            runIdOf(e.event) === run.metadata.name,
        );
        if (done) link(a.key, done.key, false);
      }
      const keys = new Set(
        [a.effect, ...a.taskEffects].flatMap((f) =>
          f ? [f.spec.idempotencyKey] : [],
        ),
      );
      for (const e of events) {
        if (e.event?.data.effect && keys.has(e.event.data.effect)) {
          link(a.key, e.key, false);
          exact.add(e.key);
        }
      }
    }
  }
  // Then the heuristic, for what no effect names: an event already linked exactly isn't
  // guessed at again (e.g. a push's pr.updated still is; the pr.opened its pr.open made isn't).
  for (const node of events) {
    for (const a of node.actions) {
      const inferred = inferredEvent(a, node, events, exact);
      if (inferred) link(a.key, inferred.key, true);
    }
  }
  return { events, links };
}

/**
 * The first provider event after the action that the factory caused on an object the action
 * touched (the effect's target, or a dispatch's run effects' targets and its Result's
 * `data.pr`), leaving out the events an effect is exactly linked to.
 */
function inferredEvent(
  a: GraphAction,
  from: GraphEvent,
  events: readonly GraphEvent[],
  exact: ReadonlySet<string>,
): GraphEvent | undefined {
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
  return events.find(
    (e) =>
      e !== from &&
      !exact.has(e.key) &&
      !!e.event &&
      !e.type.startsWith('factory.') &&
      !!e.event.data.actor?.isFactory &&
      refs.has(e.event.data.object?.ref ?? '') &&
      time(e.at) >= after,
  );
}
