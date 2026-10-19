import { describe, expect, it } from 'vitest';
import { mountWithApp } from '@/__tests__/mount';
import { workItemDetails } from '@/mocks/workitems';
import BoardLane from './BoardLane.vue';
import type { Lane } from './board';

const items = workItemDetails.slice(0, 2).map((d) => d.workItem);

describe('BoardLane', () => {
  it('is a section labelled by its title, with a card per work item', async () => {
    const lane: Lane = { id: 'running', title: 'Running', items, hidden: 0 };
    const { wrapper } = await mountWithApp(BoardLane, { props: { lane } });
    const section = wrapper.get('section');
    const heading = wrapper.get(`#${section.attributes('aria-labelledby')}`);
    expect(heading.element.tagName).toBe('H2');
    expect(heading.text()).toBe('Running 2');
    expect(wrapper.get('[data-testid="lane-count"]').text()).toBe('2');
    expect(wrapper.findAll('[data-testid="workitem-card"]')).toHaveLength(2);
    expect(wrapper.find('[data-testid="lane-hidden"]').exists()).toBe(false);
  });

  it.each([
    [1, '1 older item not shown.'],
    [3, '3 older items not shown.'],
  ])('says when %i done work items are left off', async (hidden, want) => {
    const lane: Lane = {
      id: 'done',
      title: 'Done (last 7 days)',
      items: [],
      hidden,
    };
    const { wrapper } = await mountWithApp(BoardLane, { props: { lane } });
    expect(wrapper.text()).toContain('No work items.');
    expect(wrapper.get('[data-testid="lane-hidden"]').text()).toBe(want);
  });
});
