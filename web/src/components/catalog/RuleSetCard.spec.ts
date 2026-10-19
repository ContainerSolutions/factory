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

  it('lists each selected rule with how it was picked and its effective params', () => {
    const wrapper = mountCard(ruleSets[0]!);
    const selected = wrapper.findAll('[data-testid="selected-rule"]');
    expect(
      selected.map((r) => r.get('[data-testid="rule-name"]').text()),
    ).toEqual(['docs/label-docs', 'merge-approved', 'triage-outside-issues']);
    const merge = selected[1]!;
    expect(merge.text()).toContain('by ref');
    expect(merge.text()).toContain('final');
    expect(merge.get('[data-testid="rule-actions"]').text()).toBe('effect');
    expect(
      JSON.parse(merge.get('[aria-label="Params of merge-approved"]').text()),
    ).toEqual({ mergeMethod: 'rebase', minApprovals: 2 });
    const triage = selected[2]!;
    expect(triage.text()).toContain('by selector');
    // `where` is CEL from the catalog: shown as text.
    expect(triage.text()).toContain(
      "actor.tier != 'maintainer' && !actor.isBot",
    );
    expect(triage.get('[data-testid="rule-actions"]').text()).toBe(
      'dispatch, set',
    );
    // No params, no params block.
    expect(selected[0]!.find('[aria-label^="Params"]').exists()).toBe(false);
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
