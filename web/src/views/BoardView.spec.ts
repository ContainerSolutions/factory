import { describe, expect, it } from 'vitest';
import { flushPromises } from '@vue/test-utils';
import { http, HttpResponse } from 'msw';
import Select from 'primevue/select';
import type { Schemas } from '@/api/client';
import { mountWithApp } from '@/__tests__/mount';
import { server } from '@/mocks/node';
import { workItemDetails } from '@/mocks/workitems';
import BoardView from './BoardView.vue';

type Wrapper = Awaited<ReturnType<typeof mountWithApp>>['wrapper'];

function lanes(wrapper: Wrapper) {
  return wrapper
    .findAll('section[data-testid^="lane-"]')
    .map((s) => [
      s.attributes('data-testid'),
      s.findAll('h3').map((h) => h.text()),
    ]);
}

describe('BoardView', () => {
  it('shows the work items by lane, needs-human on top', async () => {
    const { wrapper } = await mountWithApp(BoardView);
    expect(wrapper.get('h1').text()).toBe('Board');
    expect(lanes(wrapper)).toEqual([
      [
        'lane-needs-human',
        ['compliance-framework/api#310', 'compliance-framework/docs#57'],
      ],
      [
        'lane-running',
        ['platform/release-notes#7', 'compliance-framework/docs#42'],
      ],
      ['lane-waiting', ['compliance-framework/ui#88']],
      ['lane-done', ['compliance-framework/docs#40']],
    ]);
    expect(wrapper.get('[data-testid="lane-hidden"]').text()).toBe(
      '1 older item not shown.',
    );
  });

  it('filters from the URL query', async () => {
    const { wrapper } = await mountWithApp(BoardView, {
      path: '/?ruleSet=docs&taskSpec=triage',
    });
    expect(lanes(wrapper)).toEqual([
      ['lane-needs-human', []],
      ['lane-running', ['compliance-framework/docs#42']],
      ['lane-waiting', []],
      ['lane-done', ['compliance-framework/docs#40']],
    ]);
  });

  it('writes a changed filter to the URL query', async () => {
    const { wrapper, router } = await mountWithApp(BoardView, {
      path: '/?repo=compliance-framework/docs',
    });
    const provider = wrapper
      .findAllComponents(Select)
      .filter((c) => c.props('options') !== undefined)[1]!;
    provider.vm.$emit('update:modelValue', 'github-public');
    await flushPromises();
    expect(router.currentRoute.value.query).toEqual({
      provider: 'github-public',
      repo: 'compliance-framework/docs',
    });

    await wrapper.get('form button').trigger('click');
    await flushPromises();
    expect(router.currentRoute.value.query).toEqual({});
  });

  it('loads more when there is a next page', async () => {
    const items = workItemDetails.map((d) => d.workItem);
    server.use(
      http.get('/api/v1/workitems', ({ request }) =>
        HttpResponse.json<Schemas['WorkItemList']>(
          new URL(request.url).searchParams.get('cursor')
            ? { items: items.slice(1) }
            : { items: items.slice(0, 1), nextCursor: 'next' },
        ),
      ),
    );
    const { wrapper } = await mountWithApp(BoardView);
    expect(wrapper.findAll('[data-testid="workitem-card"]')).toHaveLength(1);
    const more = wrapper
      .findAll('button')
      .find((b) => b.text() === 'Load more')!;
    await more.trigger('click');
    await flushPromises();
    expect(wrapper.findAll('[data-testid="workitem-card"]')).toHaveLength(6);
    expect(
      wrapper.findAll('button').some((b) => b.text() === 'Load more'),
    ).toBe(false);
  });

  it('reports a failure to load', async () => {
    server.use(
      http.get('/api/v1/workitems', () =>
        HttpResponse.json<Schemas['Problem']>(
          { type: 'about:blank', title: 'Unauthorized', status: 401 },
          {
            status: 401,
            headers: { 'Content-Type': 'application/problem+json' },
          },
        ),
      ),
    );
    const { wrapper } = await mountWithApp(BoardView);
    expect(wrapper.get('[role="alert"]').text()).toContain(
      "Couldn't load the work items: Unauthorized",
    );
    expect(wrapper.find('section[data-testid^="lane-"]').exists()).toBe(false);
  });
});
