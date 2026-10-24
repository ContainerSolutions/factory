import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import { rules } from '@/mocks/catalog';
import RuleCard from './RuleCard.vue';

function mountRule(name: string) {
  return mount(RuleCard, {
    props: { rule: rules.find((r) => r.metadata.name === name)! },
  });
}

describe('RuleCard', () => {
  it('shows on, a highlighted where, the params and each action', () => {
    const wrapper = mountRule('triage-outside-issues');
    expect(wrapper.get('h3').text()).toBe('triage-outside-issues');
    expect(wrapper.text()).toContain('pack=intake');
    expect(wrapper.get('[data-testid="rule-on"]').text()).toBe('issue.opened');
    const where = wrapper.get('[data-testid="rule-where"] pre');
    expect(where.text()).toBe("actor.tier != 'maintainer' && !actor.isBot");
    expect(where.findAll('[data-kind="string"]').map((s) => s.text())).toEqual([
      "'maintainer'",
    ]);
    expect(wrapper.get('[data-testid="rule-params"] pre').text()).toBe(
      'queue: triage\nharness: codex',
    );
    const actions = wrapper.findAll('[data-testid="rule-actions"] > li');
    expect(actions.map((a) => a.get('span').text())).toEqual([
      'dispatch',
      'set',
    ]);
    expect(actions[0]!.get('pre').text()).toBe(
      'taskSpec: triage\nqueue: "{{ params.queue }}"\ncontext:\n  - event.object',
    );
  });

  it('says a rule without where always matches, and marks a final rule', () => {
    const wrapper = mountRule('review-factory-prs');
    expect(wrapper.get('[data-testid="rule-where"]').text()).toBe('always');
    expect(wrapper.find('[data-testid="rule-params"]').exists()).toBe(false);
    expect(mountRule('merge-approved').text()).toContain('final');
  });
});
