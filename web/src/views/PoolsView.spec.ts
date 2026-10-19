import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { http, HttpResponse } from 'msw';
import type { Schemas } from '@/api/client';
import { fakeEventSources } from '@/__tests__/eventSource';
import { confirmDialog, recordCommands } from '@/__tests__/commands';
import { mountWithApp } from '@/__tests__/mount';
import { maintainer, reader } from '@/mocks/session';
import { eventSourceKey } from '@/composables/useStream';
import { examples } from '@/mocks/examples';
import { server } from '@/mocks/node';
import { backOnline, queueViews } from '@/mocks/pools';
import PoolsView from './PoolsView.vue';

let mounted: VueWrapper | undefined;

async function mountPools() {
  const es = fakeEventSources();
  const { wrapper } = await mountWithApp(PoolsView, {
    path: '/pools',
    global: { provide: { [eventSourceKey]: es.factory } },
  });
  mounted = wrapper;
  return { wrapper, es };
}

afterEach(() => {
  mounted?.unmount();
  mounted = undefined;
  vi.useRealTimers();
});

const queueNames = (wrapper: VueWrapper) =>
  wrapper.findAll('[data-testid="queue"] h3').map((h) => h.text());

const phaseOf = (wrapper: VueWrapper, agent: string) =>
  wrapper
    .findAll('[data-testid="agent"]')
    .find((a) => a.get('h4').text() === agent)!
    .get('[data-testid="agent-phase"]')
    .text();

describe('PoolsView', () => {
  it('shows the queues by priority with their pending reasons, and the pools with their agents', async () => {
    const { wrapper, es } = await mountPools();
    expect(wrapper.get('h1').text()).toBe('Queues and pools');
    expect(queueNames(wrapper)).toEqual([
      'review',
      'triage',
      'default',
      'nightly',
    ]);
    expect(
      wrapper.findAll('[data-testid="pending-reason"]').map((r) => r.text()),
    ).toEqual([
      'anti-affinity',
      'budget',
      'agent-throttled',
      'no-capable-agent',
      'lending',
    ]);
    expect(
      wrapper.findAll('[data-testid="pool"] h3').map((h) => h.text()),
    ).toEqual(['maintainer-agents', 'ci-ephemeral']);
    expect(wrapper.findAll('[data-testid="agent"]')).toHaveLength(3);
    expect(phaseOf(wrapper, 'agt_octocat-linux-02')).toBe('Throttled');

    expect(es.last!.topics).toEqual(['agents', 'tasks']);
    expect(wrapper.get('[data-testid="stream-status"]').text()).toBe(
      'connecting',
    );
  });

  it('applies streamed agent updates, ignoring older ones (mocked SSE)', async () => {
    const { wrapper, es } = await mountPools();
    const source = es.last!;
    source.open();
    // The contract's example frame is an older version of the mock laptop: ignored.
    const frames = examples.get('streamEvents') as {
      event: string;
      id: string;
      data: unknown;
    }[];
    const older = frames.find((f) => f.event === 'agent.updated')!;
    source.emit(older.event, older.data, older.id);
    source.emit('agent.updated', backOnline, '503');
    await flushPromises();
    expect(wrapper.get('[data-testid="stream-status"]').text()).toBe('live');
    expect(phaseOf(wrapper, 'agt_ccf-bot-builder-03')).toBe('Ready');
    expect(phaseOf(wrapper, 'agt_gusfcarvalho-mbp-01')).toBe('Busy');
  });

  it('reloads the queues when a task changes', async () => {
    const { wrapper, es } = await mountPools();
    server.use(
      http.get('/api/v1/queues', () =>
        HttpResponse.json<Schemas['QueueList']>({
          items: [{ ...queueViews[1]!, depth: 0, running: 1, pending: [] }],
        }),
      ),
    );
    es.last!.open();
    es.last!.emit('task.updated', examples.get('getTask'), '600');
    await vi.waitFor(() => expect(queueNames(wrapper)).toEqual(['triage']));
    expect(wrapper.get('[data-testid="queue-running"]').text()).toBe('1 of 4');
  });

  it('reloads everything after re-creating a stream the browser gave up on', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { wrapper, es } = await mountPools();
    es.last!.open();
    server.use(
      http.get('/api/v1/agents', () =>
        HttpResponse.json<Schemas['AgentList']>({ items: [backOnline] }),
      ),
    );
    es.last!.fail('give-up');
    await vi.advanceTimersByTimeAsync(1000);
    expect(es.sources).toHaveLength(2);
    es.last!.open();
    await vi.waitFor(() =>
      expect(wrapper.findAll('[data-testid="agent"]')).toHaveLength(1),
    );
    expect(phaseOf(wrapper, 'agt_ccf-bot-builder-03')).toBe('Ready');
  });

  it('shows the error of a list that failed and the lists that loaded', async () => {
    server.use(
      http.get('/api/v1/queues', () =>
        HttpResponse.json<Schemas['Problem']>(
          { type: 'about:blank', title: 'Forbidden', status: 403 },
          {
            status: 403,
            headers: { 'Content-Type': 'application/problem+json' },
          },
        ),
      ),
    );
    const { wrapper } = await mountPools();
    expect(wrapper.get('[role="alert"]').text()).toBe(
      "Couldn't load the queues: Forbidden",
    );
    expect(wrapper.findAll('[data-testid="queue"]')).toHaveLength(0);
    expect(wrapper.findAll('[data-testid="agent"]')).toHaveLength(3);
  });

  it('says when there are no queues or pools', async () => {
    server.use(
      http.get('/api/v1/queues', () =>
        HttpResponse.json<Schemas['QueueList']>({ items: [] }),
      ),
      http.get('/api/v1/pools', () =>
        HttpResponse.json<Schemas['PoolList']>({ items: [] }),
      ),
      http.get('/api/v1/agents', () =>
        HttpResponse.json<Schemas['AgentList']>({ items: [] }),
      ),
    );
    const { wrapper } = await mountPools();
    expect(wrapper.text()).toContain('No queues.');
    expect(wrapper.text()).toContain('No pools.');
  });
});

describe('PoolsView controls', () => {
  it('pauses a queue once a maintainer confirms; there is no pool pause', async () => {
    const bodies = recordCommands();
    const es = fakeEventSources();
    const { wrapper } = await mountWithApp(PoolsView, {
      path: '/pools',
      me: maintainer,
      attachTo: document.body,
      global: { provide: { [eventSourceKey]: es.factory } },
    });
    mounted = wrapper;
    const labels = wrapper
      .findAll('button[aria-label^="Pause"]')
      .map((b) => b.attributes('aria-label'));
    expect(labels).toEqual([
      'Pause queue review',
      'Pause queue triage',
      'Pause queue default',
      'Pause queue nightly',
    ]);
    await wrapper
      .get('button[aria-label="Pause queue default"]')
      .trigger('click');
    await flushPromises();
    await confirmDialog('Pause');
    expect(bodies).toEqual([
      { type: 'pause', target: { scope: 'queue', name: 'default' } },
    ]);
  });

  it('shows a reader no controls', async () => {
    const es = fakeEventSources();
    const { wrapper } = await mountWithApp(PoolsView, {
      path: '/pools',
      me: reader,
      global: { provide: { [eventSourceKey]: es.factory } },
    });
    mounted = wrapper;
    expect(wrapper.find('[data-testid="pause-controls"]').exists()).toBe(false);
  });
});
