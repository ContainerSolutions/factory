import type { Schemas } from '@/api/client';
import type { ListItem } from '@/components/shared/list';

type LabelSelector = Schemas['LabelSelector'];
type RuleSet = Schemas['RuleSet'];
type Rule = Schemas['Rule'];
type RuleSpec = Schemas['RuleSpec'];

type Params = Record<string, unknown>;

/** Whether `labels` match the selector, as the dispatcher's `LabelSelector.Matches`: none matches all. */
export function matchesSelector(
  selector: LabelSelector | undefined,
  labels: Record<string, string> | undefined = {},
): boolean {
  if (!selector) return true;
  for (const [k, want] of Object.entries(selector.matchLabels ?? {})) {
    if (labels[k] !== want) return false;
  }
  for (const r of selector.matchExpressions ?? []) {
    const has = Object.prototype.hasOwnProperty.call(labels, r.key);
    const got = labels[r.key];
    const values = r.values ?? [];
    switch (r.operator) {
      case 'In':
        if (!has || !values.includes(got!)) return false;
        break;
      case 'NotIn':
        if (has && values.includes(got!)) return false;
        break;
      case 'Exists':
        if (!has) return false;
        break;
      case 'DoesNotExist':
        if (has) return false;
        break;
      default:
        return false;
    }
  }
  return true;
}

/** A provider's labels for selectors: its own, plus `type: <spec.type>` unless it sets one. */
export function providerLabels(
  provider: Schemas['Provider'],
): Record<string, string> {
  return { type: provider.spec.type, ...(provider.metadata.labels ?? {}) };
}

/** A selector as text: `family=github, network in (public, enterprise)`; `everything` when empty. */
export function formatSelector(selector: LabelSelector | undefined): string {
  const parts = [
    ...Object.entries(selector?.matchLabels ?? {}).map(([k, v]) => `${k}=${v}`),
    ...(selector?.matchExpressions ?? []).map((r) => {
      switch (r.operator) {
        case 'In':
          return `${r.key} in (${(r.values ?? []).join(', ')})`;
        case 'NotIn':
          return `${r.key} notin (${(r.values ?? []).join(', ')})`;
        case 'Exists':
          return r.key;
        case 'DoesNotExist':
          return `!${r.key}`;
        default:
          return `${r.key} ${r.operator}`;
      }
    }),
  ];
  return parts.length ? parts.join(', ') : 'everything';
}

function asParams(value: unknown): Params {
  return value && typeof value === 'object' && !Array.isArray(value)
    ? (value as Params)
    : {};
}

/**
 * A rule's effective params, as the catalog loader merges them: the rule's declared params ← the
 * RuleSet's defaults (declared ones only) ← the RuleSet's overrides for the rule. An override of an
 * undeclared param is a load error; it is left out and reported in `unknown`.
 */
export function mergeParams(
  declared: unknown,
  defaults: unknown,
  overrides: unknown,
): { params: Params; unknown: string[] } {
  const d = asParams(declared);
  const params: Params = { ...d };
  for (const [k, v] of Object.entries(asParams(defaults))) {
    if (k in d) params[k] = v;
  }
  const unknown: string[] = [];
  for (const [k, v] of Object.entries(asParams(overrides)).sort(([a], [b]) =>
    a.localeCompare(b),
  )) {
    if (k in d) params[k] = v;
    else unknown.push(k);
  }
  return { params, unknown };
}

/** How a rule came into a RuleSet's selection. */
export type SelectedVia = 'ref' | 'selector' | 'inline';

/** One rule a RuleSet selected, with its params after the RuleSet's defaults and overrides. */
export interface SelectedRule {
  /** The rule's name: `<RuleSet>/<name>` for an inline rule, as the loader names it. */
  name: string;
  /** The name overrides and exclude use. */
  bare: string;
  via: SelectedVia[];
  labels: Record<string, string>;
  spec: RuleSpec;
  params: Params;
  /** Overrides of params the rule doesn't declare (the loader rejects them). */
  unknownOverrides: string[];
}

export interface RuleSelection {
  rules: SelectedRule[];
  /** Refs and excludes that name no loaded rule. */
  missing: string[];
}

/**
 * The rules a RuleSet selects, as the catalog loader resolves them: refs ∪ selector ∪ inline
 * rules, minus exclude, sorted by name. The contract has no read model of the resolved selection,
 * so the UI repeats the loader's logic over the Rule objects it fetched.
 */
export function selectRules(
  ruleSet: RuleSet,
  rules: readonly Rule[],
): RuleSelection {
  const spec = ruleSet.spec;
  const sel = spec.rules ?? {};
  const set = ruleSet.metadata.name;
  const byName = new Map(rules.map((r) => [r.metadata.name, r]));
  const picked = new Map<
    string,
    { rule: Pick<Rule, 'metadata' | 'spec'>; bare: string; via: SelectedVia[] }
  >();
  const missing: string[] = [];

  const pick = (
    name: string,
    bare: string,
    rule: Pick<Rule, 'metadata' | 'spec'>,
    via: SelectedVia,
  ) => {
    const entry = picked.get(name);
    if (entry) entry.via.push(via);
    else picked.set(name, { rule, bare, via: [via] });
  };

  for (const ref of sel.refs ?? []) {
    const rule = byName.get(ref);
    if (rule) pick(ref, ref, rule, 'ref');
    else missing.push(ref);
  }
  if (sel.selector) {
    for (const rule of rules) {
      if (matchesSelector(sel.selector, rule.metadata.labels)) {
        pick(rule.metadata.name, rule.metadata.name, rule, 'selector');
      }
    }
  }
  for (const inline of spec.inlineRules ?? []) {
    pick(
      `${set}/${inline.metadata.name}`,
      inline.metadata.name,
      inline,
      'inline',
    );
  }
  for (const ex of sel.exclude ?? []) {
    const known = picked.delete(ex) || picked.delete(`${set}/${ex}`);
    if (!known && !byName.has(ex)) missing.push(ex);
  }

  const selected = [...picked.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([name, { rule, bare, via }]): SelectedRule => {
      const { params, unknown } = mergeParams(
        rule.spec.params,
        spec.defaults,
        spec.overrides?.[bare],
      );
      return {
        name,
        bare,
        via,
        labels: rule.metadata.labels ?? {},
        spec: rule.spec,
        params,
        unknownOverrides: unknown,
      };
    });
  return { rules: selected, missing };
}

/** The kinds of an action list, in order: `dispatch, set`. */
export function actionKinds(actions: readonly Schemas['Action'][]): string[] {
  return actions.map((a) => Object.keys(a)[0] ?? 'unknown');
}

/** A commit SHA's first 12 characters. */
export function shortSHA(sha: string): string {
  return sha.slice(0, 12);
}

/** The stored verdict a dry-run (capitalised) verdict stands for. */
const STORED_VERDICT: Record<
  Schemas['EvaluationRail']['verdict'],
  Schemas['RailVerdict']['verdict']
> = {
  Allowed: 'allowed',
  Suppressed: 'suppressed',
  Deferred: 'deferred',
};

/**
 * A dry-run decision in the shape the DecisionWhy drawer shows. An EvaluationRail carries one
 * verdict and no rail name, so it becomes one row named `rails`; the action is its type.
 */
export function decisionOf(
  evaluation: Schemas['Evaluation'],
  d: Schemas['EvaluationDecision'],
): Schemas['Decision'] {
  const verdict = STORED_VERDICT[d.rail.verdict];
  const decision: Schemas['Decision'] = {
    event: evaluation.event,
    ruleSet: d.ruleSet,
    rule: d.rule,
    fired: d.rail.verdict === 'Allowed',
    rails: [
      d.rail.reason
        ? { rail: 'rails', verdict, reason: d.rail.reason }
        : { rail: 'rails', verdict },
    ],
    actions: [d.action.type],
  };
  if (d.inputs) decision.inputs = d.inputs;
  if (d.rail.reason) decision.reason = d.rail.reason;
  return decision;
}

/** The pill tone of a candidate rule's result. */
export function matchTone(
  result: Schemas['RuleMatch']['result'],
): 'success' | 'neutral' | 'danger' | 'warn' {
  switch (result) {
    case 'matched':
      return 'success';
    case 'error':
    case 'action-error':
      return 'danger';
    case 'suppressed-by-final':
      return 'warn';
    default:
      return 'neutral';
  }
}

/** A recorded event as one line, for picking it: `issue.opened · org/repo#42 · 2026-10-16 09:30:00 UTC`. */
export function eventLabel(event: Schemas['Event'], time: string): string {
  return [event.type, event.subject, time].filter(Boolean).join(' · ');
}

function oneOrList(v: Schemas['StringOrList'] | undefined): string {
  return Array.isArray(v) ? v.join(' | ') : (v ?? '');
}

/** A harness request: `codex (gpt-5*, effort medium)`. */
export function harnessText(h: Schemas['HarnessRequest'] | undefined): string {
  if (!h) return '';
  const parts = [
    oneOrList(h.config?.model),
    h.config?.reasoningEffort
      ? `effort ${oneOrList(h.config.reasoningEffort)}`
      : '',
    h.config?.prefer?.length ? `prefer ${h.config.prefer.join(', ')}` : '',
  ].filter(Boolean);
  return parts.length ? `${h.name} (${parts.join(', ')})` : h.name;
}

/** A harness constraint: `codex: gpt-5* · effort low | medium`. */
export function constraintText(c: Schemas['HarnessConstraint']): string {
  const parts = [
    oneOrList(c.models),
    c.reasoningEffort ? `effort ${oneOrList(c.reasoningEffort)}` : '',
  ].filter(Boolean);
  return parts.length ? `${c.name}: ${parts.join(' · ')}` : c.name;
}

/** A skill source: `ccf-skills@v0.9.0: lisa, resolve`. */
export function skillText(s: Schemas['SkillRef']): string {
  const at = s.commit
    ? `${s.source}@${s.commit}`
    : s.ref
      ? `${s.source}@${s.ref}`
      : s.source;
  return s.use?.length ? `${at}: ${s.use.join(', ')}` : at;
}

/** A run's limits: `40 turns, 15m, $1.50`; empty when it has none. */
export function limitsText(l: Schemas['Limits'] | undefined): string {
  if (!l) return '';
  return [
    l.maxTurns !== undefined ? `${l.maxTurns} turns` : '',
    l.maxWallClock ? `${l.maxWallClock}` : '',
    l.maxCostUSD !== undefined ? `$${l.maxCostUSD.toFixed(2)}` : '',
  ]
    .filter(Boolean)
    .join(', ');
}

/** One declared input of a TaskSpec, as a table row. */
export interface InputRow {
  name: string;
  type: string;
  values: string;
  default: string;
  required: boolean;
}

/** A TaskSpec's declared inputs, by name. */
export function inputRows(
  inputs: Schemas['TaskSpecSpec']['inputs'],
): InputRow[] {
  return Object.entries(inputs ?? {})
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([name, i]) => ({
      name,
      type: i.type ?? '',
      values: (i.enum ?? [])
        .map((v) => (typeof v === 'string' ? v : JSON.stringify(v)))
        .join(', '),
      default:
        i.default === undefined
          ? ''
          : typeof i.default === 'string'
            ? i.default
            : JSON.stringify(i.default),
      required: !!i.required,
    }));
}

/** A Group member source: `github-public: org compliance-framework, role admin`. */
export function memberText(m: Schemas['GroupSource']): string {
  const what = m.orgRole
    ? `org ${m.orgRole.org}, role ${m.orgRole.role}`
    : m.team
      ? `team ${m.team}`
      : m.users?.length
        ? `users ${m.users.join(', ')}`
        : m.userGroup
          ? `user group ${m.userGroup}`
          : 'nobody';
  return `${m.provider}: ${what}`;
}

/** The TaskSpecs that use each catalog file, as their prompt or their output schema. */
export function fileUsers(
  taskSpecs: readonly Schemas['TaskSpec'][],
): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const t of taskSpecs) {
    for (const p of [t.spec.instructions?.template, t.spec.output?.schema]) {
      if (p) (out[p] ??= []).push(t.metadata.name);
    }
  }
  for (const names of Object.values(out)) names.sort();
  return out;
}

/** A provider's labels as `k=v, k=v`. */
export function providerLabelsText(p: Schemas['Provider']): string {
  return Object.entries(p.metadata.labels ?? {})
    .map(([k, v]) => `${k}=${v}`)
    .join(', ');
}

/** What a provider watches: its orgs and channels, or everything it can see. */
export function providerScopeText(p: Schemas['Provider']): string {
  const s = p.spec.scope;
  return (
    [
      ...(s?.orgs ?? []).map((o) => `org ${o}`),
      ...(s?.channels ?? []).map((c) => `#${c}`),
    ].join(', ') || 'everything it can see'
  );
}

const HEALTH_TONE: Record<
  Schemas['ProviderHealth']['status'],
  'success' | 'warn' | 'danger' | 'neutral'
> = {
  ok: 'success',
  degraded: 'warn',
  down: 'danger',
  unknown: 'neutral',
};

/** The pill tone of a provider's health. */
export function providerHealthTone(
  status: Schemas['ProviderHealth']['status'],
): 'success' | 'warn' | 'danger' | 'neutral' {
  return HEALTH_TONE[status] ?? 'neutral';
}

/** The entries of every list + detail catalog tab, from the loaded catalog. */
export interface CatalogLists {
  taskspecs: ListItem[];
  rules: ListItem[];
  rulesets: ListItem[];
  templates: ListItem[];
  providers: ListItem[];
  queues: ListItem[];
  groups: ListItem[];
}

/** The id of a queue's or pool's entry: `Queue/<name>`, `Pool/<name>` (they may share names). */
export function queuePoolId(o: Schemas['Queue'] | Schemas['Pool']): string {
  return `${o.kind}/${o.metadata.name}`;
}

/**
 * Each tab's entries: a TaskSpec by its default harness, a rule by its triggers, a RuleSet by
 * the rules it selects, a template by the providers it has a body for, a provider by its type
 * and health, a queue by its pool, a pool by its mode, a group by its member sources.
 */
export function catalogLists(c: {
  taskSpecs: readonly Schemas['TaskSpec'][];
  rules: readonly Rule[];
  ruleSets: readonly RuleSet[];
  templates: readonly Schemas['Template'][];
  providers: readonly Schemas['ProviderView'][];
  queues: readonly Schemas['Queue'][];
  pools: readonly Schemas['Pool'][];
  groups: readonly Schemas['Group'][];
}): CatalogLists {
  const plural = (n: number, one: string) => `${n} ${one}${n === 1 ? '' : 's'}`;
  return {
    taskspecs: c.taskSpecs.map((t) => ({
      id: t.metadata.name,
      label: t.metadata.name,
      secondary: harnessText(t.spec.harness?.default) || 'the agent picks',
    })),
    rules: c.rules.map((r) => ({
      id: r.metadata.name,
      label: r.metadata.name,
      secondary: `on ${r.spec.on.join(', ')}`,
    })),
    rulesets: c.ruleSets.map((rs) => ({
      id: rs.metadata.name,
      label: rs.metadata.name,
      secondary: plural(selectRules(rs, c.rules).rules.length, 'rule'),
    })),
    templates: c.templates.map((t) => ({
      id: t.metadata.name,
      label: t.metadata.name,
      secondary: Object.keys(t.spec.bodies).sort().join(', '),
    })),
    providers: c.providers.map((v) => ({
      id: v.provider.metadata.name,
      label: v.provider.metadata.name,
      secondary: `${v.provider.spec.type} · ${v.health.status}`,
    })),
    queues: [
      ...c.queues.map((q) => ({
        id: queuePoolId(q),
        label: q.metadata.name,
        secondary: `queue on ${q.spec.pool}`,
      })),
      ...c.pools.map((p) => ({
        id: queuePoolId(p),
        label: p.metadata.name,
        secondary: `pool, ${p.spec.mode}`,
      })),
    ],
    groups: c.groups.map((g) => ({
      id: g.metadata.name,
      label: g.metadata.name,
      secondary: plural(g.spec.members.length, 'member source'),
    })),
  };
}
