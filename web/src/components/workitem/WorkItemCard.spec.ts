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
      'stackNext: 1',
      'stackTotal: 2',
      'review: needs-human',
      expect.stringMatching(/^lastReview: \{"round":3,/),
    ]);
    expect(pills[2]!.get('span').attributes('data-tone')).toBe('danger');
    // A long value is cut to the card; its title holds it whole.
    expect(pills[3]!.get('span').attributes('title')).toBe(pills[3]!.text());
    expect(wrapper.text()).toContain('api · 2026-10-17 14:02:01 UTC');
  });

  it('shows pending approvals and structured state as text', async () => {
    const item = byName('wi_01JAC0APPROVAL00000000000000');
    const { wrapper } = await mountWithApp(WorkItemCard, { props: { item } });
    expect(
      wrapper.findAll('ul[aria-label="State"] li').map((p) => p.text()),
    ).toEqual(['stackNext: 0', 'stackTotal: 3', 'approval implement: Pending']);
  });

  it('shows that an admin closed it', async () => {
    const item: Schemas['WorkItem'] = {
      ...byName('wi_01JAC0ESCALATED0000000000000'),
    };
    item.status = {
      ...item.status,
      closed: { reason: 'wont-do', at: '2026-10-18T00:00:00Z' },
    };
    const { wrapper } = await mountWithApp(WorkItemCard, { props: { item } });
    expect(wrapper.get('[data-testid="card-closed"]').text()).toBe(
      "Closed: won't do",
    );
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
