import { describe, expect, it } from 'vitest';
import { nextTick } from 'vue';
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

  it("marks a replay's actions on their rule, and says why a task waits in its tooltip", () => {
    const decisions = pipeline.workItem.status!.decisions!.map((d) =>
      d.rule === 'review-factory-prs' ? { ...d, replay: true } : d,
    );
    const tasks = pipeline.tasks.map((t) =>
      t.status?.phase === 'Pending'
        ? {
            ...t,
            status: { ...t.status, reasonDetail: 'Only the author is online.' },
          }
        : t,
    );
    const wrapper = mount(WorkItemGraph, {
      props: {
        detail: {
          ...pipeline,
          tasks,
          workItem: {
            ...pipeline.workItem,
            status: { ...pipeline.workItem.status, decisions },
          },
        },
      },
    });
    const review = wrapper.findAll('[data-testid="graph-action"]')[5]!;
    expect(review.get('[data-testid="graph-rule"]').text()).toBe(
      'docs/review-factory-prs (replay)',
    );
    expect(review.attributes('aria-label')).toContain(
      'by docs/review-factory-prs (replay)',
    );
    const pending = wrapper
      .findAll('[data-testid="graph-task"]')
      .find((t) => t.attributes('data-phase') === 'Pending')!;
    expect(pending.get('title').text()).toMatch(
      /waiting: anti-affinity.*\nOnly the author is online\.$/s,
    );
  });

  it('renders event text as text', () => {
    const wrapper = mount(WorkItemGraph, {
      props: {
        detail: {
          ...example,
          events: [{ ...example.events[0]!, id: '<b>x</b>' }],
          workItem: {
            ...example.workItem,
            status: {
              decisions: example.workItem.status!.decisions!.map((d) => ({
                ...d,
                event: '<b>x</b>',
              })),
            },
          },
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

  it('hides events with no decisions, and says so with a way to the timeline', async () => {
    const epic = workItemDetails.find(
      (d) => d.workItem.metadata.name === 'wi_d235e0c50568b22418e5',
    )!;
    const wrapper = mount(WorkItemGraph, { props: { detail: epic } });
    expect(
      wrapper
        .findAll('[data-testid="graph-event"]')
        .map((e) => e.get('text').text()),
    ).toEqual(['issue.opened', 'issue.commented', 'factory.run.completed']);
    const note = wrapper.get('[data-testid="graph-hidden"]');
    expect(note.text().replace(/\s+/g, ' ')).toBe(
      '1 event with no decisions hidden; see the Timeline',
    );
    await note.get('button').trigger('click');
    expect(wrapper.emitted('timeline')).toEqual([[]]);
    expect(
      mount(WorkItemGraph, { props: { detail: pipeline } })
        .find('[data-testid="graph-hidden"]')
        .exists(),
    ).toBe(false);
  });

  describe('pan and zoom', () => {
    const transform = (w: ReturnType<typeof mount>) =>
      w.get('[data-testid="graph-content"]').attributes('transform');

    it('zooms with the controls and the keys, within the limits, and fits again', async () => {
      const wrapper = mount(WorkItemGraph, { props: { detail: pipeline } });
      // jsdom has no layout: the viewport is 0 wide, so the fit is 1×.
      expect(transform(wrapper)).toBe('translate(0 0) scale(1)');
      const button = (label: string) =>
        wrapper.get(`[data-testid="graph-controls"] [aria-label="${label}"]`);
      await button('Zoom in').trigger('click');
      expect(transform(wrapper)).toBe('translate(0 0) scale(1.25)');
      for (let i = 0; i < 10; i++) await button('Zoom in').trigger('click');
      expect(transform(wrapper)).toBe('translate(0 0) scale(3)');
      const viewport = wrapper.get('[data-testid="graph-viewport"]');
      await viewport.trigger('keydown', { key: '0' });
      expect(transform(wrapper)).toBe('translate(0 0) scale(1)');
      await viewport.trigger('keydown', { key: '-' });
      expect(transform(wrapper)).toBe('translate(0 0) scale(0.8)');
      await viewport.trigger('keydown', { key: 'ArrowRight' });
      expect(transform(wrapper)).toBe('translate(-40 0) scale(0.8)');
      await button('Actual size').trigger('click');
      expect(transform(wrapper)).toBe('translate(0 0) scale(1)');
    });

    it('pans on a wheel and zooms around the cursor on ⌘/Ctrl+wheel', async () => {
      const wrapper = mount(WorkItemGraph, { props: { detail: pipeline } });
      const viewport = wrapper.get('[data-testid="graph-viewport"]');
      await viewport.trigger('wheel', { deltaX: 10, deltaY: 30 });
      expect(transform(wrapper)).toBe('translate(-10 -30) scale(1)');
      viewport.element.dispatchEvent(
        new WheelEvent('wheel', {
          deltaY: -100,
          ctrlKey: true,
          clientX: 0,
          clientY: 0,
          bubbles: true,
          cancelable: true,
        }),
      );
      await nextTick();
      const [, x, y, k] = transform(wrapper)!.match(
        /translate\((\S+) (\S+)\) scale\((\S+)\)/,
      )!;
      expect(Number(k)).toBeCloseTo(Math.exp(0.2), 5);
      // The content under the cursor (0, 0) stays put.
      expect(Number(x)).toBeCloseTo(-10 * Math.exp(0.2), 5);
      expect(Number(y)).toBeCloseTo(-30 * Math.exp(0.2), 5);
    });

    it('pans on a drag without clicking the node it started on; a small move still clicks', async () => {
      const wrapper = mount(WorkItemGraph, {
        props: { detail: pipeline },
        attachTo: document.body,
      });
      const viewport = wrapper.get('[data-testid="graph-viewport"]');
      const task = wrapper.findAll('[data-testid="graph-task"]')[1]!;
      const press = { button: 0, pointerId: 1 };
      await task.trigger('pointerdown', {
        ...press,
        clientX: 100,
        clientY: 100,
      });
      await viewport.trigger('pointermove', {
        ...press,
        clientX: 102,
        clientY: 101,
      });
      await viewport.trigger('pointerup', {
        ...press,
        clientX: 102,
        clientY: 101,
      });
      await task.trigger('click');
      expect(wrapper.emitted('task')).toEqual([
        ['tsk_01JAC0PIPELINEIMPLEMENT0000'],
      ]);
      expect(transform(wrapper)).toBe('translate(0 0) scale(1)');

      await task.trigger('pointerdown', {
        ...press,
        clientX: 100,
        clientY: 100,
      });
      await viewport.trigger('pointermove', {
        ...press,
        clientX: 160,
        clientY: 80,
      });
      expect(viewport.classes()).toContain('cursor-grabbing');
      await viewport.trigger('pointerup', {
        ...press,
        clientX: 160,
        clientY: 80,
      });
      expect(transform(wrapper)).toBe('translate(60 -20) scale(1)');
      // The click that ends the drag doesn't open the task.
      await task.trigger('click');
      expect(wrapper.emitted('task')).toHaveLength(1);
      // The next one does.
      await task.trigger('click');
      expect(wrapper.emitted('task')).toHaveLength(2);
      wrapper.unmount();
    });
  });
});
