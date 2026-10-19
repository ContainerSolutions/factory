import { beforeEach, describe, expect, it } from 'vitest';
import { createPinia, setActivePinia } from 'pinia';
import { delay, http, HttpResponse } from 'msw';
import type { Schemas } from '@/api/client';
import { agents, backOnline, poolViews, queueViews } from '@/mocks/pools';
import { server } from '@/mocks/node';
import { MAX_PAGES, usePoolsStore } from './pools';

function problem(status: number, title: string) {
  return HttpResponse.json<Schemas['Problem']>(
    { type: 'about:blank', title, status },
    { status, headers: { 'Content-Type': 'application/problem+json' } },
  );
}

const names = (list: { metadata: { name: string } }[]) =>
  list.map((x) => x.metadata.name);

describe('pools store', () => {
  beforeEach(() => setActivePinia(createPinia()));

  it('loads the queues, pools and agents', async () => {
    const store = usePoolsStore();
    const loading = store.load();
    expect(store.loading).toBe(true);
    await loading;
    expect(store.loading).toBe(false);
    expect(store.queues).toEqual(queueViews);
    expect(store.pools).toEqual(poolViews);
    expect(store.agents).toEqual(agents);
    expect([store.queuesLoaded, store.poolsLoaded, store.agentsLoaded]).toEqual(
      [true, true, true],
    );
  });

  it('follows the cursor to the last page', async () => {
    const seen: (string | null)[] = [];
    server.use(
      http.get('/api/v1/agents', ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('cursor');
        seen.push(cursor);
        return HttpResponse.json<Schemas['AgentList']>(
          cursor
            ? { items: agents.slice(1) }
            : { items: agents.slice(0, 1), nextCursor: 'page-2' },
        );
      }),
    );
    const store = usePoolsStore();
    await store.load();
    expect(seen).toEqual([null, 'page-2']);
    expect(names(store.agents)).toEqual(names([...agents]));
  });

  it('stops following a cursor after MAX_PAGES pages', async () => {
    let pages = 0;
    server.use(
      http.get('/api/v1/pools', () => {
        pages++;
        return HttpResponse.json<Schemas['PoolList']>({
          items: [],
          nextCursor: 'more',
        });
      }),
    );
    const store = usePoolsStore();
    await store.load();
    expect(pages).toBe(MAX_PAGES);
    expect(store.poolsLoaded).toBe(true);
  });

  it('fails each list on its own', async () => {
    server.use(
      http.get('/api/v1/queues', () => problem(503, 'Service Unavailable')),
      http.get('/api/v1/agents', () => HttpResponse.error()),
    );
    const store = usePoolsStore();
    await store.load();
    expect(store.queuesError).toBe('Service Unavailable');
    expect(store.queuesLoaded).toBe(false);
    expect(store.agentsError).toBeTruthy();
    expect(store.poolsError).toBeNull();
    expect(store.pools).toEqual(poolViews);
  });

  it('keeps what it has when a quiet reload fails', async () => {
    const store = usePoolsStore();
    await store.load();
    server.use(
      http.get('/api/v1/queues', () => problem(500, 'Internal Server Error')),
    );
    await store.load({ quiet: true });
    expect(store.queues).toEqual(queueViews);
    expect(store.queuesError).toBeNull();
  });

  it('applies agent updates: newer replaces, older is ignored, unknown is added', async () => {
    const store = usePoolsStore();
    // Before the agents are loaded, an update is dropped: the load will have it.
    store.upsertAgent(backOnline);
    expect(store.agents).toEqual([]);

    await store.load();
    store.upsertAgent(backOnline);
    expect(store.agents[2]!.status!.phase).toBe('Ready');

    store.upsertAgent({
      ...agents[0]!,
      metadata: { ...agents[0]!.metadata, resourceVersion: '412' },
      status: { ...agents[0]!.status!, phase: 'Ready' },
    });
    expect(store.agents[0]!.status!.phase).toBe('Busy');

    const newcomer: Schemas['Agent'] = {
      ...agents[0]!,
      metadata: { name: 'agt_new-host', resourceVersion: '600' },
    };
    store.upsertAgent(newcomer);
    expect(names(store.agents)).toEqual([
      ...names([...agents]),
      'agt_new-host',
    ]);
  });

  it('coalesces queue reloads that arrive while one runs into one more', async () => {
    const store = usePoolsStore();
    await store.load();
    let requests = 0;
    server.use(
      http.get('/api/v1/queues', async () => {
        requests++;
        await delay(20);
        return HttpResponse.json<Schemas['QueueList']>({
          items: queueViews.slice(0, requests),
        });
      }),
    );
    await Promise.all([
      store.refreshQueues(),
      store.refreshQueues(),
      store.refreshQueues(),
    ]);
    expect(requests).toBe(2);
    expect(store.queues).toHaveLength(2);

    await store.refreshQueues();
    expect(requests).toBe(3);
  });
});
