import { describe, expect, it } from 'vitest';
import type { VueWrapper } from '@vue/test-utils';
import { mountWithApp } from '@/__tests__/mount';
import { poolViews, queueViews } from '@/mocks/pools';
import QueuesAndPools from './QueuesAndPools.vue';

const cells = (wrapper: VueWrapper, testid: string) =>
  wrapper
    .findAll(`[data-testid="${testid}"]`)
    .map((r) => r.findAll('th, td').map((c) => c.text()));

describe('QueuesAndPools', () => {
  it('shows the configured queues and pools, and links to their live state', async () => {
    const { wrapper } = await mountWithApp(QueuesAndPools, {
      props: {
        queues: queueViews.map((v) => v.queue),
        pools: poolViews.map((v) => v.pool),
      },
    });
    expect(cells(wrapper, 'catalog-queue')[2]).toEqual([
      'default',
      'maintainer-agents',
      '2',
      '50',
      '5,000,000 tokens, $20.00 a day',
    ]);
    expect(cells(wrapper, 'catalog-pool')).toEqual([
      ['maintainer-agents', 'registered', '—'],
      ['ci-ephemeral', 'ephemeral', '0–4 agents'],
    ]);
    expect(wrapper.get('a').attributes('href')).toBe('/pools');
  });
});
