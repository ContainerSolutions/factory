import { describe, expect, it } from 'vitest';
import type { Schemas } from '@/api/client';
import { examples } from '@/mocks/examples';
import { providerViews, rules, ruleSets } from '@/mocks/catalog';
import {
  actionKinds,
  decisionOf,
  eventLabel,
  formatSelector,
  matchesSelector,
  matchTone,
  mergeParams,
  providerLabels,
  selectRules,
  shortSHA,
} from './catalog';

const labels = { family: 'github', network: 'public' };

describe('matchesSelector', () => {
  it.each([
    [undefined, true],
    [{}, true],
    [{ matchLabels: { family: 'github' } }, true],
    [{ matchLabels: { family: 'slack' } }, false],
    [{ matchLabels: { tier: 'x' } }, false],
    [
      {
        matchExpressions: [
          { key: 'network', operator: 'In' as const, values: ['public'] },
        ],
      },
      true,
    ],
    [
      {
        matchExpressions: [
          { key: 'missing', operator: 'In' as const, values: ['x'] },
        ],
      },
      false,
    ],
    [
      {
        matchExpressions: [
          { key: 'network', operator: 'NotIn' as const, values: ['public'] },
        ],
      },
      false,
    ],
    [
      {
        matchExpressions: [
          { key: 'missing', operator: 'NotIn' as const, values: ['x'] },
        ],
      },
      true,
    ],
    [
      { matchExpressions: [{ key: 'family', operator: 'Exists' as const }] },
      true,
    ],
    [
      {
        matchExpressions: [
          { key: 'family', operator: 'DoesNotExist' as const },
        ],
      },
      false,
    ],
  ])('%j → %s', (selector, want) => {
    expect(matchesSelector(selector, labels)).toBe(want);
  });

  it('formats selectors', () => {
    expect(formatSelector(undefined)).toBe('everything');
    expect(
      formatSelector({
        matchLabels: { family: 'github' },
        matchExpressions: [
          { key: 'network', operator: 'In', values: ['public', 'enterprise'] },
          { key: 'tier', operator: 'NotIn', values: ['x'] },
          { key: 'a', operator: 'Exists' },
          { key: 'b', operator: 'DoesNotExist' },
        ],
      }),
    ).toBe(
      'family=github, network in (public, enterprise), tier notin (x), a, !b',
    );
  });

  it("adds a provider's type to its labels unless it sets one", () => {
    expect(providerLabels(providerViews[2]!.provider)).toEqual({
      type: 'slack',
      family: 'slack',
    });
  });
});

describe('mergeParams', () => {
  it('applies declared defaults, then overrides, reporting undeclared overrides', () => {
    expect(
      mergeParams(
        { a: 1, b: 2, c: 3 },
        { b: 20, z: 'ignored' },
        { c: 30, y: 'unknown', x: 'unknown' },
      ),
    ).toEqual({ params: { a: 1, b: 20, c: 30 }, unknown: ['x', 'y'] });
  });

  it('takes anything that is not an object as nothing', () => {
    expect(mergeParams(undefined, [1], 'x')).toEqual({
      params: {},
      unknown: [],
    });
  });
});

describe('selectRules', () => {
  it('resolves the docs RuleSet: refs, selector and inline rules, minus exclude, with params after overrides', () => {
    const { rules: selected, missing } = selectRules(ruleSets[0]!, rules);
    expect(missing).toEqual([]);
    expect(
      selected.map((r) => [r.name, r.via, r.params, r.unknownOverrides]),
    ).toEqual([
      ['docs/label-docs', ['inline'], {}, []],
      [
        'merge-approved',
        ['ref'],
        { mergeMethod: 'rebase', minApprovals: 2 },
        [],
      ],
      [
        'triage-outside-issues',
        ['selector'],
        { queue: 'triage', harness: 'claude-code' },
        [],
      ],
    ]);
  });

  it('reports refs and excludes that name no rule, and merges how a rule was picked', () => {
    const ruleSet: Schemas['RuleSet'] = {
      kind: 'RuleSet',
      metadata: { name: 'x' },
      spec: {
        providerSelector: {},
        rules: {
          refs: ['merge-approved', 'gone'],
          selector: { matchLabels: { pack: 'merge' } },
          exclude: ['also-gone'],
        },
        overrides: { 'merge-approved': { nope: 1 } },
      },
    };
    const { rules: selected, missing } = selectRules(ruleSet, rules);
    expect(missing).toEqual(['gone', 'also-gone']);
    expect(selected.map((r) => [r.name, r.via, r.unknownOverrides])).toEqual([
      ['merge-approved', ['ref', 'selector'], ['nope']],
    ]);
  });

  it('excludes an inline rule by its bare name', () => {
    const docs = ruleSets[0]!;
    const ruleSet: Schemas['RuleSet'] = {
      ...docs,
      spec: {
        ...docs.spec,
        rules: { ...docs.spec.rules, exclude: ['label-docs'] },
      },
    };
    expect(selectRules(ruleSet, rules).rules.map((r) => r.name)).toEqual([
      'merge-approved',
      'triage-maintainer-issues',
      'triage-outside-issues',
    ]);
  });
});

describe('dry-run helpers', () => {
  const evaluation = examples.get('dryRunCatalog') as Schemas['Evaluation'];

  it('shapes a dry-run decision for the why drawer', () => {
    expect(decisionOf(evaluation, evaluation.decisions[0]!)).toEqual({
      event: evaluation.event,
      ruleSet: 'docs',
      rule: 'triage-outside-issues',
      fired: true,
      rails: [{ rail: 'rails', verdict: 'allowed' }],
      actions: ['dispatch'],
      inputs: { 'actor.tier': 'contributor', 'actor.isBot': false },
    });
    expect(
      decisionOf(evaluation, {
        ruleSet: 'a',
        rule: 'b',
        action: { type: 'set' },
        rail: { verdict: 'Suppressed', reason: 'kill-switch' },
      }),
    ).toEqual({
      event: evaluation.event,
      ruleSet: 'a',
      rule: 'b',
      fired: false,
      rails: [{ rail: 'rails', verdict: 'suppressed', reason: 'kill-switch' }],
      actions: ['set'],
      reason: 'kill-switch',
    });
  });

  it.each([
    ['matched', 'success'],
    ['not-matched', 'neutral'],
    ['error', 'danger'],
    ['action-error', 'danger'],
    ['suppressed-by-final', 'warn'],
    ['duplicate', 'neutral'],
  ] as const)('tones %s as %s', (result, tone) => {
    expect(matchTone(result)).toBe(tone);
  });

  it('labels events, actions and SHAs', () => {
    const event = {
      type: 'issue.opened',
      subject: 'o/r#1',
    } as Schemas['Event'];
    expect(eventLabel(event, 'then')).toBe('issue.opened · o/r#1 · then');
    expect(
      actionKinds([
        { dispatch: { taskSpec: 't', queue: 'q' } },
        { cancel: { taskSpec: 't' } },
      ]),
    ).toEqual(['dispatch', 'cancel']);
    expect(shortSHA('9f1e2d3c4b5a69788796a5b4c3d2e1f0')).toBe('9f1e2d3c4b5a');
  });
});
