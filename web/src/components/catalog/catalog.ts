import type { Schemas } from '@/api/client';

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
