import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import type { Schemas } from '@/api/client';
import { examples } from '@/mocks/examples';
import { workItemDetails } from '@/mocks/workitems';
import WorkItemGraph from './WorkItemGraph.vue';

const pipeline = workItemDetails.find(
  (d) => d.workItem.metadata.name === 'wi_01JAC0PIPELINE0000000000000',
)!;
const example = examples.get('getWorkItem') as Schemas['WorkItemDetail'];

describe('WorkItemGraph', () => {
  it('draws events, actions with their rule, state and shape, and the links', () => {
    const wrapper = mount(WorkItemGraph, { props: { detail: pipeline } });
    expect(wrapper.findAll('[data-testid="graph-event"]')).toHaveLength(4);
    const actions = wrapper.findAll('[data-testid="graph-action"]');
    expect(
      actions.map((a) => [
        a.attributes('data-kind'),
        a.attributes('data-state'),
        a.get('rect').attributes('stroke-dasharray') ?? 'solid',
      ]),
    ).toEqual([
      ['dispatch', 'Succeeded', 'solid'],
      ['set', 'done', '2 3'],
      ['effect', 'applied', '2 3'],
      ['dispatch', 'Succeeded', 'solid'],
      ['notify', 'done', '2 3'],
      ['dispatch', 'Pending', 'solid'],
    ]);
    // The state is text too, not only the stroke's colour.
    expect(actions[5]!.findAll('text').map((t) => t.text())).toEqual([
      'docs/review-factory-prs',
      'dispatch · docs-review',
      'Pending',
    ]);
    expect(actions[3]!.findAll('text')[2]!.text()).toBe(
      'Succeeded · 2 attempts',
    );
    expect(
      wrapper
        .findAll('[data-testid="graph-link"]')
        .map((l) => l.attributes('data-inferred')),
    ).toEqual(['false', 'false', 'true']);
    expect(
      wrapper
        .findAll('[data-testid="graph-link"]')[2]!
        .attributes('stroke-dasharray'),
    ).toBe('6 4');
  });

  it("opens an action's why and an event's row, by click or key", async () => {
    const wrapper = mount(WorkItemGraph, { props: { detail: pipeline } });
    const actions = wrapper.findAll('[data-testid="graph-action"]');
    expect(actions[3]!.attributes()).toMatchObject({
      role: 'button',
      tabindex: '0',
    });
    expect(actions[3]!.attributes('aria-label')).toBe(
      'dispatch · implement: Succeeded, by docs/implement-promoted. Why?',
    );
    await actions[3]!.trigger('click');
    await actions[0]!.trigger('keydown', { key: 'Enter' });
    expect(wrapper.emitted('why')).toEqual([[3], [0]]);
    const events = wrapper.findAll('[data-testid="graph-event"]');
    await events[2]!.trigger('keydown', { key: ' ' });
    await events[2]!.trigger('keydown', { key: 'x' });
    expect(wrapper.emitted('event')).toEqual([
      ['event/github-public/gh-docs-78-opened'],
    ]);
  });

  it('renders event text as text', () => {
    const wrapper = mount(WorkItemGraph, {
      props: {
        detail: {
          ...example,
          events: [{ ...example.events[0]!, id: '<b>x</b>' }],
          workItem: { ...example.workItem, status: {} },
        },
      },
    });
    expect(wrapper.find('b').exists()).toBe(false);
    expect(wrapper.get('[data-testid="graph-event"] title').text()).toBe(
      '<b>x</b>',
    );
  });

  it('says when nothing has happened', () => {
    const wrapper = mount(WorkItemGraph, {
      props: {
        detail: {
          ...example,
          events: [],
          workItem: { ...example.workItem, status: {} },
        },
      },
    });
    expect(wrapper.find('svg').exists()).toBe(false);
    expect(wrapper.text()).toContain('Nothing has happened yet.');
  });
});
