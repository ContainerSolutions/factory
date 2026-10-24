import { afterEach, describe, expect, it } from 'vitest';
import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { http, HttpResponse } from 'msw';
import type { Schemas } from '@/api/client';
import { confirmDialog, recordCommands } from '@/__tests__/commands';
import { mountWithApp } from '@/__tests__/mount';
import { maintainer, reader } from '@/mocks/session';
import { server } from '@/mocks/node';
import WorkItemView from './WorkItemView.vue';

let mounted: VueWrapper | undefined;

async function mountItem(id: string, query = '') {
  const { wrapper, router } = await mountWithApp(WorkItemView, {
    path: `/workitems/${id}${query}`,
    props: { id },
    attachTo: document.body,
  });
  mounted = wrapper;
  return { wrapper, router };
}

afterEach(() => {
  mounted?.unmount();
  mounted = undefined;
});

const dialog = () =>
  document.body.querySelector<HTMLElement>('[role="dialog"]');

describe('WorkItemView', () => {
  it('shows the header and the graph, with tabs for the tasks and the timeline', async () => {
    const { wrapper } = await mountItem('wi_01JAB3J9T1D2E3F4G5H6J7K8LM');
    expect(wrapper.get('h1').text()).toBe('compliance-framework/docs#42');
    expect(wrapper.get('[data-testid="lane"]').text()).toBe('Running');
    expect(wrapper.findAll('[role="tab"]').map((t) => t.text())).toEqual([
      'Graph',
      'LLM tasks1',
      'Timeline',
    ]);
    expect(wrapper.get('[role="tabpanel"]').attributes('aria-labelledby')).toBe(
      'workitem-tab-graph',
    );
    expect(wrapper.findAll('[data-testid="graph-action"]')).toHaveLength(2);
    expect(wrapper.get('nav a').attributes('href')).toBe('/');
    expect(dialog()).toBeNull();
  });

  it('switches tabs through the URL', async () => {
    const { wrapper, router } = await mountItem(
      'wi_01JAB3J9T1D2E3F4G5H6J7K8LM',
    );
    await wrapper.get('[data-tab="tasks"]').trigger('click');
    await flushPromises();
    expect(router.currentRoute.value.query.tab).toBe('tasks');
    expect(wrapper.findAll('[data-testid="llm-task"]')).toHaveLength(1);
    await wrapper
      .get('[data-tab="tasks"]')
      .trigger('keydown', { key: 'ArrowRight' });
    await flushPromises();
    expect(router.currentRoute.value.query.tab).toBe('timeline');
    expect(wrapper.findAll('[data-testid="timeline-event"]')).toHaveLength(1);
  });

  it("opens an event of the graph in the timeline, and an action's why", async () => {
    const { wrapper, router } = await mountItem(
      'wi_01JAC0PIPELINE0000000000000',
    );
    await wrapper.findAll('[data-testid="graph-action"]')[3]!.trigger('click');
    await flushPromises();
    expect(dialog()!.getAttribute('aria-label')).toBe(
      'Why: docs / implement-promoted',
    );
    await wrapper.findAll('[data-testid="graph-event"]')[0]!.trigger('click');
    await flushPromises();
    expect(router.currentRoute.value.query.tab).toBe('timeline');
    const first = wrapper.findAll('[data-testid="timeline-event"]')[0]!;
    expect(first.get('button').attributes('aria-expanded')).toBe('true');
  });

  it('opens a task of the graph in the LLM tasks tab, outlined', async () => {
    const { wrapper, router } = await mountItem(
      'wi_01JAC0PIPELINE0000000000000',
    );
    await wrapper.findAll('[data-testid="graph-task"]')[1]!.trigger('click');
    await flushPromises();
    expect(router.currentRoute.value.query.tab).toBe('tasks');
    const focused = wrapper.findAll('[data-testid="llm-task"][data-focus]');
    expect(focused.map((t) => t.get('h3').text())).toEqual(['implement']);
    expect(focused[0]!.attributes('id')).toBe(
      'task-tsk_01JAC0PIPELINEIMPLEMENT0000',
    );
  });

  it('opens the "why" panel of a decision, with the catalog SHA from its task', async () => {
    const { wrapper } = await mountItem(
      'wi_01JAB3J9T1D2E3F4G5H6J7K8LM',
      '?tab=timeline',
    );
    await wrapper.get('button[aria-haspopup="dialog"]').trigger('click');
    await flushPromises();
    const why = dialog()!;
    expect(why.getAttribute('aria-label')).toBe(
      'Why: docs / triage-outside-issues',
    );
    expect(
      why.querySelector('[data-testid="why-catalog-sha"]')?.textContent?.trim(),
    ).toBe('9f1e2d3c4b5a69788796a5b4c3d2e1f0a9b8c7d6');
    expect(why.textContent).toContain('"actor.tier": "contributor"');
    expect(why.textContent).toContain('run-cap');
  });

  it('opens the panel of the decision clicked', async () => {
    const { wrapper } = await mountItem(
      'wi_01JAC0ESCALATED0000000000000',
      '?tab=timeline',
    );
    const buttons = wrapper.findAll('button[aria-haspopup="dialog"]');
    await buttons[1]!.trigger('click');
    await flushPromises();
    expect(dialog()!.getAttribute('aria-label')).toBe(
      'Why: api / escalate-needs-human',
    );
    // No task came from this decision, so its catalog commit is unknown.
    expect(
      dialog()!.querySelector('[data-testid="why-catalog-sha"]')?.textContent,
    ).toContain('not recorded');
  });

  it('says when the work item does not exist', async () => {
    const { wrapper } = await mountItem('wi_missing');
    expect(wrapper.get('h1').text()).toBe('Work item not found');
    expect(wrapper.text()).toContain('wi_missing');
  });

  it('reports a failure to load', async () => {
    server.use(
      http.get('/api/v1/workitems/:id', () =>
        HttpResponse.json<Schemas['Problem']>(
          { type: 'about:blank', title: 'Unavailable', status: 503 },
          {
            status: 503,
            headers: { 'Content-Type': 'application/problem+json' },
          },
        ),
      ),
    );
    const { wrapper } = await mountItem('wi_01JAB3J9T1D2E3F4G5H6J7K8LM');
    expect(wrapper.get('[role="alert"]').text()).toContain(
      "Couldn't load the work item: Unavailable",
    );
  });

  it('loads the new work item when the id changes', async () => {
    const { wrapper } = await mountItem('wi_01JAB3J9T1D2E3F4G5H6J7K8LM');
    await wrapper.setProps({ id: 'wi_01JAC0WAITING000000000000000' });
    await flushPromises();
    expect(wrapper.get('h1').text()).toBe('compliance-framework/ui#88');
  });
});

describe('WorkItemView controls', () => {
  const ID = 'wi_01JAB3J9T1D2E3F4G5H6J7K8LM';

  it('pauses the work item once a maintainer confirms', async () => {
    const bodies = recordCommands();
    const { wrapper } = await mountWithApp(WorkItemView, {
      path: `/workitems/${ID}`,
      props: { id: ID },
      me: maintainer,
      attachTo: document.body,
    });
    mounted = wrapper;
    await wrapper
      .get(`button[aria-label="Pause work item ${ID}"]`)
      .trigger('click');
    await flushPromises();
    expect(dialog()!.textContent).toContain(`Pause work item ${ID}?`);
    await confirmDialog('Pause');
    expect(bodies).toEqual([
      { type: 'pause', target: { scope: 'workitem', name: ID } },
    ]);
    expect(wrapper.get('[data-testid="pause-state"]').text()).toBe(
      'pause sent',
    );
  });

  it('shows a reader no controls', async () => {
    const { wrapper } = await mountWithApp(WorkItemView, {
      path: `/workitems/${ID}`,
      props: { id: ID },
      me: reader,
    });
    mounted = wrapper;
    expect(wrapper.find('[data-testid="pause-controls"]').exists()).toBe(false);
  });
});
