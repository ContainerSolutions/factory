import { describe, expect, it } from 'vitest';
import type { Schemas } from '@/api/client';
import { mountWithApp } from '@/__tests__/mount';
import { workItemDetails } from '@/mocks/workitems';
import WorkItemCard from './WorkItemCard.vue';

const byName = (name: string) =>
  workItemDetails.find((d) => d.workItem.metadata.name === name)!.workItem;

describe('WorkItemCard', () => {
  it('links to the work item and shows its provider, state and RuleSets', async () => {
    const item = byName('wi_01JAC0ESCALATED0000000000000');
    const { wrapper } = await mountWithApp(WorkItemCard, { props: { item } });
    const link = wrapper.get('h3 a');
    expect(link.text()).toBe('compliance-framework/api#310');
    expect(link.attributes('href')).toBe(
      '/workitems/wi_01JAC0ESCALATED0000000000000',
    );
    expect(wrapper.get('[data-testid="card-provider"]').text()).toBe(
      'github-public · pr',
    );
    const pills = wrapper.findAll('ul[aria-label="State"] li');
    expect(pills.map((p) => p.text())).toEqual([
      'implement: done',
      'review: needs-human',
    ]);
    expect(pills[1]!.get('span').attributes('data-tone')).toBe('danger');
    expect(wrapper.text()).toContain('api · 2026-10-17 14:02:01 UTC');
  });

  it('shows pending approvals and structured state as text', async () => {
    const item = byName('wi_01JAC0APPROVAL00000000000000');
    const { wrapper } = await mountWithApp(WorkItemCard, { props: { item } });
    expect(
      wrapper.findAll('ul[aria-label="State"] li').map((p) => p.text()),
    ).toEqual([
      'plan: planned',
      'stackNext: 0',
      'stackTotal: 3',
      'approval implement: Pending',
    ]);
  });

  it('falls back to the name without links, state or decisions', async () => {
    const item: Schemas['WorkItem'] = {
      kind: 'WorkItem',
      metadata: { name: 'wi_bare' },
      spec: {},
    };
    const { wrapper } = await mountWithApp(WorkItemCard, { props: { item } });
    expect(wrapper.get('h3').text()).toBe('wi_bare');
    expect(wrapper.find('[data-testid="card-provider"]').exists()).toBe(false);
    expect(wrapper.find('ul').exists()).toBe(false);
    expect(wrapper.find('time').exists()).toBe(false);
  });
});
