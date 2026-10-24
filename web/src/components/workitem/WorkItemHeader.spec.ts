import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import type { Schemas } from '@/api/client';
import { workItemDetails } from '@/mocks/workitems';
import WorkItemHeader from './WorkItemHeader.vue';

const byName = (name: string) =>
  workItemDetails.find((d) => d.workItem.metadata.name === name)!.workItem;

function pairs(wrapper: ReturnType<typeof mount>, testid: string) {
  const dl = wrapper.get(`[data-testid="${testid}"]`);
  const dts = dl.findAll('dt').map((d) => d.text());
  const dds = dl.findAll('dd').map((d) => d.text());
  return dts.map((dt, i) => [dt, dds[i]]);
}

describe('WorkItemHeader', () => {
  it('shows the links across providers, state keys, counters and lane', () => {
    const wrapper = mount(WorkItemHeader, {
      props: { item: byName('wi_01JAC0ESCALATED0000000000000') },
    });
    expect(
      wrapper
        .findAll('[data-testid="links"] li')
        .map((li) => li.findAll('span').map((s) => s.text())),
    ).toEqual([
      ['github-public', 'pr', 'compliance-framework/api#310', 'primary'],
      ['slack-ccf', 'thread', '#ccf-api/1729070000.000100'],
      [
        'github-public',
        'branch',
        'compliance-framework/api@factory/wi_01JAC0ESCALATED0000000000000/implement-review-feedback-round-3-oscal-profile-flag-visibility',
      ],
    ]);
    expect(pairs(wrapper, 'state-keys')).toEqual([
      ['stackNext', '1'],
      ['stackTotal', '2'],
      ['review', 'needs-human'],
      ['lastReview', expect.stringMatching(/^\{"round":3,/)],
    ]);
    expect(pairs(wrapper, 'counters')).toEqual([
      ['implement', '3'],
      ['docs-review', '3'],
    ]);
    expect(wrapper.get('[data-testid="lane"]').text()).toBe('Needs human');
    expect(wrapper.text()).toContain('github-public:gusfcarvalho (maintainer)');
    expect(wrapper.find('[data-testid="approvals"]').exists()).toBe(false);
  });

  it('shows structured state as JSON and the approvals', () => {
    const wrapper = mount(WorkItemHeader, {
      props: {
        item: {
          ...byName('wi_01JAC0APPROVAL00000000000000'),
          status: {
            state: { plan: ['a', 'b'] },
            approvals: [
              { name: 'implement', phase: 'Pending' },
              { name: 'merge', phase: 'Granted' },
            ],
          },
        },
      },
    });
    expect(pairs(wrapper, 'state-keys')).toEqual([['plan', '["a","b"]']]);
    expect(
      wrapper
        .findAll('[data-testid="approvals"] li')
        .map((li) => li.findAll('span').map((s) => s.text())),
    ).toEqual([
      ['implement', 'Pending'],
      ['merge', 'Granted'],
    ]);
  });

  it('says when there is nothing to show', () => {
    const item: Schemas['WorkItem'] = {
      kind: 'WorkItem',
      metadata: { name: 'wi_bare' },
      spec: {},
    };
    const wrapper = mount(WorkItemHeader, { props: { item } });
    expect(wrapper.text()).toContain('No links.');
    expect(wrapper.text()).toContain('No state set.');
    expect(wrapper.find('[data-testid="counters"]').exists()).toBe(false);
    expect(wrapper.get('[data-testid="lane"]').text()).toBe('Waiting');
  });
});
