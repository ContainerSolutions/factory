import { afterEach, describe, expect, it } from 'vitest';
import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { http, HttpResponse } from 'msw';
import type { Schemas } from '@/api/client';
import { mountWithApp } from '@/__tests__/mount';
import { server } from '@/mocks/node';
import WorkItemView from './WorkItemView.vue';

let mounted: VueWrapper | undefined;

async function mountItem(id: string) {
  const { wrapper, router } = await mountWithApp(WorkItemView, {
    path: `/workitems/${id}`,
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
  it('shows the header and the timeline', async () => {
    const { wrapper } = await mountItem('wi_01JAB3J9T1D2E3F4G5H6J7K8LM');
    expect(wrapper.get('h1').text()).toBe('compliance-framework/docs#42');
    expect(wrapper.get('[data-testid="lane"]').text()).toBe('Running');
    expect(
      wrapper
        .findAll('[data-testid="timeline-entry"]')
        .map((e) => e.attributes('data-kind')),
    ).toEqual(['event', 'decision', 'task', 'run', 'effect']);
    expect(wrapper.get('nav a').attributes('href')).toBe('/');
    expect(dialog()).toBeNull();
  });

  it('opens the "why" panel of a decision, with the catalog SHA from its task', async () => {
    const { wrapper } = await mountItem('wi_01JAB3J9T1D2E3F4G5H6J7K8LM');
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
    const { wrapper } = await mountItem('wi_01JAC0ESCALATED0000000000000');
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
