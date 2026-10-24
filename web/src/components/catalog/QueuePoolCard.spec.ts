import { describe, expect, it } from 'vitest';
import type { VueWrapper } from '@vue/test-utils';
import { mountWithApp } from '@/__tests__/mount';
import { poolViews, queueViews } from '@/mocks/pools';
import QueuePoolCard from './QueuePoolCard.vue';

const pairs = (wrapper: VueWrapper) => {
  const dds = wrapper.findAll('dd').map((d) => d.text());
  return wrapper.findAll('dt').map((d, i) => [d.text(), dds[i]]);
};

describe('QueuePoolCard', () => {
  it('shows a configured queue, and links to the live state', async () => {
    const { wrapper } = await mountWithApp(QueuePoolCard, {
      props: { object: queueViews[2]!.queue },
    });
    expect(wrapper.get('h3').text()).toBe('Queue default');
    expect(pairs(wrapper)).toEqual([
      ['Pool', 'maintainer-agents'],
      ['Concurrency', '2'],
      ['Priority', '50'],
      ['Budget', '5,000,000 tokens, $20.00 a day'],
    ]);
    expect(wrapper.get('a').attributes('href')).toBe('/pools');
  });

  it.each<[number, string, string]>([
    [0, 'registered', '—'],
    [1, 'ephemeral', '0–4 agents'],
  ])('shows pool %i: %s, %s', async (i, mode, scaling) => {
    const { wrapper } = await mountWithApp(QueuePoolCard, {
      props: { object: poolViews[i]!.pool },
    });
    expect(pairs(wrapper)).toEqual([
      ['Mode', mode],
      ['Scaling', scaling],
    ]);
  });
});
