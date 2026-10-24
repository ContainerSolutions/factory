import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import type { Schemas } from '@/api/client';
import { providerViews, rules, ruleSets } from '@/mocks/catalog';
import RuleSetCard from './RuleSetCard.vue';

const providers = providerViews.map((v) => v.provider);

function mountCard(ruleSet: Schemas['RuleSet']) {
  return mount(RuleSetCard, {
    props: { ruleSet, rules: [...rules], providers },
  });
}

describe('RuleSetCard', () => {
  it('shows where the RuleSet applies and how it selects', () => {
    const wrapper = mountCard(ruleSets[0]!);
    expect(wrapper.get('h3').text()).toBe('docs');
    expect(wrapper.get('[data-testid="ruleset-providers"]').text()).toBe(
      'family=github → github-public, github-enterprise',
    );
    expect(wrapper.text()).toContain('compliance-framework/docs');
    expect(wrapper.get('[data-testid="ruleset-selection"]').text()).toBe(
      'refs merge-approved; selector pack=intake; 1 inline; minus triage-maintainer-issues',
    );
  });

  it('lists the selected rules and opens one, with how it was picked and its params after merging', async () => {
    const wrapper = mountCard(ruleSets[0]!);
    const items = () => wrapper.findAll('[data-testid="list-item"]');
    expect(items().map((b) => b.find('span').text())).toEqual([
      'docs/label-docs',
      'merge-approved',
      'triage-outside-issues',
    ]);
    // The first is open by default; with no params, it says so.
    expect(items()[0]!.attributes('aria-current')).toBe('true');
    const shown = () => wrapper.get('[data-testid="selected-rule"]');
    expect(shown().get('[data-testid="rule"] h3').text()).toBe(
      'docs/label-docs',
    );
    expect(shown().text()).toContain('by inline');
    expect(shown().get('[data-testid="rule-params"]').text()).toContain('none');

    await items()[1]!.trigger('click');
    expect(items()[1]!.attributes('aria-current')).toBe('true');
    expect(items()[0]!.attributes('aria-current')).toBeUndefined();
    expect(shown().text()).toContain('by ref');
    expect(shown().text()).toContain('final');
    expect(shown().get('[data-testid="rule-actions"]').text()).toContain(
      'effect',
    );
    const params = shown().get('[aria-label="Params of merge-approved"]');
    expect(params.text()).toContain('mergeMethod: rebase');
    expect(params.text()).toContain('minApprovals: 2');

    await items()[2]!.trigger('click');
    expect(shown().text()).toContain('by selector');
    // `where` is CEL from the catalog: shown as text.
    expect(shown().get('[data-testid="rule-where"]').text()).toBe(
      "actor.tier != 'maintainer' && !actor.isBot",
    );
    expect(
      shown().get('[aria-label="Params of triage-outside-issues"]').text(),
    ).toContain('harness: claude-code');
  });

  it('matches providers by expressions and the type label', () => {
    const wrapper = mountCard(ruleSets[1]!);
    expect(wrapper.get('[data-testid="ruleset-providers"]').text()).toBe(
      'network in (public) → github-public',
    );
    const slack = mountCard({
      ...ruleSets[1]!,
      spec: {
        ...ruleSets[1]!.spec,
        providerSelector: { matchLabels: { type: 'slack' } },
      },
    });
    expect(slack.get('[data-testid="ruleset-providers"]').text()).toBe(
      'type=slack → slack-ccf',
    );
  });

  it('says what it names that does not exist, and when it selects nothing', () => {
    const wrapper = mountCard({
      kind: 'RuleSet',
      metadata: { name: 'empty' },
      spec: {
        providerSelector: { matchLabels: { family: 'none' } },
        rules: { refs: ['gone'] },
      },
    });
    expect(wrapper.get('[data-testid="ruleset-missing"]').text()).toContain(
      'gone',
    );
    expect(wrapper.text()).toContain('Selects no rules.');
    expect(wrapper.get('[data-testid="ruleset-providers"]').text()).toContain(
      '→ none',
    );
  });
});
