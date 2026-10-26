import { beforeEach, describe, expect, it } from 'vitest';
import { createPinia, setActivePinia } from 'pinia';
import { delay, http, HttpResponse } from 'msw';
import type { Schemas } from '@/api/client';
import { server } from '@/mocks/node';
import { workItemDetails } from '@/mocks/workitems';
import { useWorkItemsStore } from './workitems';

const items = workItemDetails.map((d) => d.workItem);

function problem(status: number, title: string, detail?: string) {
  return HttpResponse.json<Schemas['Problem']>(
    { type: 'about:blank', title, status, detail },
    { status, headers: { 'Content-Type': 'application/problem+json' } },
  );
}

describe('work items store', () => {
  beforeEach(() => setActivePinia(createPinia()));

  it('loads the board from the mock work items', async () => {
    const store = useWorkItemsStore();
    await store.load();
    expect(store.loaded).toBe(true);
    expect(store.error).toBeNull();
    expect(store.items.map((i) => i.metadata.name)).toEqual(
      items.map((i) => i.metadata.name),
    );
    expect(store.nextCursor).toBeUndefined();
  });

  it('pages with the cursor and appends', async () => {
    const seen: (string | null)[] = [];
    server.use(
      http.get('/api/v1/workitems', ({ request }) => {
        const cursor = new URL(request.url).searchParams.get('cursor');
        seen.push(cursor);
        return HttpResponse.json<Schemas['WorkItemList']>(
          cursor
            ? { items: items.slice(2) }
            : { items: items.slice(0, 2), nextCursor: 'c2' },
        );
      }),
    );
    const store = useWorkItemsStore();
    await store.load();
    expect(store.items).toHaveLength(2);
    expect(store.nextCursor).toBe('c2');
    await store.loadMore();
    expect(store.items).toHaveLength(items.length);
    expect(store.nextCursor).toBeUndefined();
    await store.loadMore();
    expect(seen).toEqual([null, 'c2']);
  });

  it('reports a failed list', async () => {
    server.use(
      http.get('/api/v1/workitems', () =>
        problem(400, 'Bad Request', 'bad cursor'),
      ),
    );
    const store = useWorkItemsStore();
    await store.load();
    expect(store.error).toBe('Bad Request: bad cursor');
    expect(store.loaded).toBe(false);
    expect(store.loading).toBe(false);
  });

  it('loads one work item with its timeline', async () => {
    const store = useWorkItemsStore();
    await store.loadItem('wi_01JAB3J9T1D2E3F4G5H6J7K8LM');
    expect(store.current?.workItem.metadata.name).toBe(
      'wi_01JAB3J9T1D2E3F4G5H6J7K8LM',
    );
    expect(store.current?.runs).toHaveLength(1);
    expect(store.currentLoading).toBe(false);
  });

  it('shows a closed work item on the board and its page', async () => {
    const store = useWorkItemsStore();
    const id = 'wi_01JAB3J9T1D2E3F4G5H6J7K8LM';
    await store.load();
    await store.loadItem(id);
    const outcome = await store.closeItem(id, { reason: 'wont-do' });
    expect(outcome.ok).toBe(true);
    expect(store.current?.workItem.status?.closed?.reason).toBe('wont-do');
    expect(
      store.items.find((i) => i.metadata.name === id)?.status?.closed?.reason,
    ).toBe('wont-do');
  });

  it('drops a deleted work item from the board, and says why a delete was refused', async () => {
    const store = useWorkItemsStore();
    const id = 'wi_01JAB3J9T1D2E3F4G5H6J7K8LM';
    await store.load();
    const refused = await store.deleteItem(id);
    expect(refused).toEqual(
      expect.objectContaining({ ok: false, status: 409, code: 'task-leased' }),
    );
    expect(store.items.some((i) => i.metadata.name === id)).toBe(true);
    const outcome = await store.deleteItem(id, true);
    expect(outcome.ok).toBe(true);
    expect(store.items.some((i) => i.metadata.name === id)).toBe(false);
  });

  it('keeps the page on a quiet reload that fails', async () => {
    const store = useWorkItemsStore();
    const id = 'wi_01JAB3J9T1D2E3F4G5H6J7K8LM';
    await store.loadItem(id);
    server.use(
      http.get('/api/v1/workitems/:id', () => problem(500, 'Internal')),
    );
    await store.loadItem(id, { quiet: true });
    expect(store.current?.workItem.metadata.name).toBe(id);
    expect(store.currentError).toBeNull();
  });

  it('tells a missing work item from a failure', async () => {
    const store = useWorkItemsStore();
    await store.loadItem('wi_missing');
    expect(store.currentNotFound).toBe(true);
    expect(store.currentError).toBeNull();

    server.use(
      http.get('/api/v1/workitems/:id', () => problem(500, 'Internal Error')),
    );
    await store.loadItem('wi_missing');
    expect(store.currentNotFound).toBe(false);
    expect(store.currentError).toBe('Internal Error');
  });

  describe('upsert', () => {
    const [first, second] = items as [Schemas['WorkItem'], Schemas['WorkItem']];
    const version = (item: Schemas['WorkItem'], v: string) => ({
      ...item,
      metadata: { ...item.metadata, resourceVersion: v },
    });

    it('replaces a loaded work item, adds a new one first, and keeps a newer one', async () => {
      const store = useWorkItemsStore();
      store.upsert(first);
      expect(store.items).toEqual([]);

      await store.load();
      const updated = version(second, '7');
      store.upsert(updated);
      expect(store.items[1]).toEqual(updated);
      expect(store.items).toHaveLength(items.length);

      store.upsert(version(second, '3'));
      expect(store.items[1]).toEqual(updated);

      const fresh = {
        ...first,
        metadata: { ...first.metadata, name: 'wi_new' },
      };
      store.upsert(fresh);
      expect(store.items[0]).toEqual(fresh);
      expect(store.items).toHaveLength(items.length + 1);
    });

    it('replaces when a version is not a number', async () => {
      const store = useWorkItemsStore();
      await store.load();
      const updated = version(second, 'abc');
      store.upsert(updated);
      expect(store.items[1]).toEqual(updated);
    });
  });

  it('drops the response for an earlier work item', async () => {
    server.use(
      http.get('/api/v1/workitems/:id', async ({ params }) => {
        if (params.id === 'wi_slow') await delay(50);
        const detail = workItemDetails[0]!;
        return HttpResponse.json<Schemas['WorkItemDetail']>({
          ...detail,
          workItem: {
            ...detail.workItem,
            metadata: { name: String(params.id) },
          },
        });
      }),
    );
    const store = useWorkItemsStore();
    const slow = store.loadItem('wi_slow');
    await store.loadItem('wi_fast');
    await slow;
    expect(store.current?.workItem.metadata.name).toBe('wi_fast');
  });
});
