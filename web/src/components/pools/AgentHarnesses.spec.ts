import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import type { Schemas } from '@/api/client';
import { agents } from '@/mocks/pools';
import AgentHarnesses from './AgentHarnesses.vue';

const offers = agents[0]!.spec.capabilities.harnesses!;

const cells = (wrapper: ReturnType<typeof mount>) =>
  wrapper
    .findAll('[data-testid="harness-offer"]')
    .map((row) => row.findAll('td').map((td) => td.text()));

describe('AgentHarnesses', () => {
  it('shows each offer: version, auth, models with the default marked, and efforts', () => {
    const wrapper = mount(AgentHarnesses, { props: { offers } });
    expect(cells(wrapper)).toEqual([
      [
        'codex 0.46.0',
        'subscription',
        'gpt-5-codex(default)',
        'low, medium, high',
      ],
      [
        'claude-code 2.1.3',
        'apiKey',
        'claude-sonnet-5-5claude-opus-5-5(default)',
        'low, medium, high, max',
      ],
    ]);
    expect(wrapper.findAll('[data-testid="default-model"]')).toHaveLength(2);
  });

  it("shows a default model the offer doesn't list, and an offer with no models", () => {
    const odd: Schemas['HarnessOffer'][] = [
      { name: 'codex', models: ['gpt-5-codex'], defaultModel: 'gpt-5' },
      { name: 'claude-code' },
    ];
    const wrapper = mount(AgentHarnesses, { props: { offers: odd } });
    expect(cells(wrapper)).toEqual([
      ['codex', '', 'gpt-5-codex default: gpt-5', ''],
      ['claude-code', '', 'none reported', ''],
    ]);
  });

  it('says when there are no offers', () => {
    const wrapper = mount(AgentHarnesses, { props: { offers: [] } });
    expect(wrapper.find('table').exists()).toBe(false);
    expect(wrapper.text()).toBe('No harness offers.');
  });
});
