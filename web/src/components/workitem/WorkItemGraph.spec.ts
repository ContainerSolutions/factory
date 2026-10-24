import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import { example, workItemDetails } from '@/mocks/workitems';
import WorkItemGraph from './WorkItemGraph.vue';

const pipeline = workItemDetails.find(
  (d) => d.workItem.metadata.name === 'wi_01JAC0PIPELINE0000000000000',
)!;

describe('WorkItemGraph', () => {
  it('draws events, actions with their rule, state and shape, and the links', () => {
    const wrapper = mount(WorkItemGraph, { props: { detail: pipeline } });
    expect(wrapper.findAll('[data-testid="graph-event"]')).toHaveLength(6);
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
      ['effect', 'applied', '2 3'],
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
    const links = wrapper.findAll('[data-testid="graph-link"]');
    expect(
      links.map((l) => [
        l.attributes('data-cause'),
        l.attributes('data-inferred'),
      ]),
    ).toEqual([
      ['task', 'false'],
      ['run', 'false'],
      ['task', 'false'],
      ['run', 'false'],
      ['effect', 'false'],
      ['notify', 'false'],
      ['task', 'false'],
      ['inferred', 'true'],
    ]);
    expect(links[4]!.attributes('stroke-dasharray')).toBeUndefined();
    expect(links[7]!.attributes('stroke-dasharray')).toBe('6 4');
    // A dispatch → its task is a straight arrow along the row.
    expect(links[0]!.attributes('d')).toMatch(/^M [\d.]+ [\d.]+ H [\d.]+$/);
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
    await events[3]!.trigger('keydown', { key: ' ' });
    await events[3]!.trigger('keydown', { key: 'x' });
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

  it('draws each task by its phase, and opens it by click or key', async () => {
    const wrapper = mount(WorkItemGraph, { props: { detail: pipeline } });
    const tasks = wrapper.findAll('[data-testid="graph-task"]');
    expect(
      tasks.map((t) => [
        t.attributes('data-phase'),
        t.findAll('text').map((x) => x.text()),
      ]),
    ).toEqual([
      ['Succeeded', ['triage', 'Succeeded · 1 attempt', 'outcome: promote']],
      [
        'Succeeded',
        ['implement', 'Succeeded · 2 attempts', 'outcome: pr-opened'],
      ],
      [
        'Pending',
        [
          'docs-review',
          'Pending · 0 attempts',
          'waiting: anti-affinity: waiti…',
        ],
      ],
    ]);
    // Coloured as the LLM tasks tab colours the phase, and named in words too.
    expect(tasks[2]!.get('rect').classes()).toContain('fill-amber-50');
    expect(tasks[2]!.attributes()).toMatchObject({
      role: 'button',
      tabindex: '0',
    });
    expect(tasks[2]!.attributes('aria-label')).toBe(
      'Task docs-review: Pending, 0 attempts, waiting: anti-affinity: waiting for an agent other than the author. Show it in LLM tasks',
    );
    await tasks[1]!.trigger('click');
    await tasks[2]!.trigger('keydown', { key: 'Enter' });
    expect(wrapper.emitted('task')).toEqual([
      ['tsk_01JAC0PIPELINEIMPLEMENT0000'],
      ['tsk_01JAC0PIPELINEREVIEW0000000'],
    ]);
  });
});
