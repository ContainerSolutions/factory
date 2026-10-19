import { beforeEach, describe, expect, it } from 'vitest';
import { createPinia, setActivePinia } from 'pinia';
import { http, HttpResponse } from 'msw';
import { catalogSummary, providerViews, ruleSets } from '@/mocks/catalog';
import { examples } from '@/mocks/examples';
import { server } from '@/mocks/node';
import { useCatalogStore } from './catalog';

describe('catalog store', () => {
  beforeEach(() => setActivePinia(createPinia()));

  it('loads the summary, the providers and every object it shows in full', async () => {
    const store = useCatalogStore();
    await store.load();
    expect(store.loading).toBe(false);
    expect(store.summary).toEqual(catalogSummary);
    expect(store.providers).toEqual(providerViews);
    expect(store.ruleSets.map((r) => r.metadata.name)).toEqual(['api', 'docs']);
    expect(store.ruleSets[1]).toEqual(ruleSets[0]);
    expect(store.rules.map((r) => r.metadata.name)).toEqual([
      'implement-review-feedback',
      'merge-approved',
      'review-factory-prs',
      'triage-maintainer-issues',
      'triage-outside-issues',
    ]);
    expect(store.taskSpecs.map((t) => t.metadata.name)).toEqual([
      'implement',
      'review',
      'triage',
    ]);
    expect(store.queues).toHaveLength(4);
    expect(store.pools.map((p) => p.metadata.name)).toEqual([
      'ci-ephemeral',
      'maintainer-agents',
    ]);
    expect(store.namesOf('Group')).toEqual(['maintainers', 'readers']);
    expect(store.objectErrors).toEqual([]);
  });

  it('only fetches the kinds it shows', async () => {
    const fetched: string[] = [];
    server.events.on('request:start', ({ request }) => {
      const path = new URL(request.url).pathname;
      if (path.startsWith('/api/v1/catalog/')) fetched.push(path);
    });
    await useCatalogStore().load();
    server.events.removeAllListeners();
    expect(fetched.some((p) => p.includes('/Provider/'))).toBe(false);
    expect(fetched.some((p) => p.includes('/Group/'))).toBe(false);
    expect(fetched).toContain('/api/v1/catalog/RuleSet/docs');
  });

  it('reports each object that fails, keeping the others', async () => {
    server.use(
      http.get('/api/v1/catalog/RuleSet/api', () =>
        HttpResponse.json(
          { title: 'Not Found', status: 404, detail: 'gone' },
          { status: 404 },
        ),
      ),
    );
    const store = useCatalogStore();
    await store.load();
    expect(store.objectErrors).toEqual(['RuleSet api: Not Found: gone']);
    expect(store.ruleSets.map((r) => r.metadata.name)).toEqual(['docs']);
  });

  it('reports a failed summary and failed providers on their own', async () => {
    server.use(
      http.get('/api/v1/catalog', () =>
        HttpResponse.json(
          { title: 'Unavailable', status: 503 },
          { status: 503 },
        ),
      ),
      http.get('/api/v1/providers', () => HttpResponse.error()),
    );
    const store = useCatalogStore();
    await store.load();
    expect(store.summaryError).toBe('Unavailable');
    expect(store.providersError).toBeTruthy();
    expect(store.summary).toBeNull();
  });

  it('dry-runs an event', async () => {
    const event = (examples.get('getWorkItem') as { events: unknown[] })
      .events[0] as never;
    const outcome = await useCatalogStore().dryRun({
      event,
      workItem: 'wi_1',
    });
    expect(outcome.ok).toBe(true);
    if (outcome.ok) {
      expect(outcome.evaluation.workItem).toBe('wi_1');
      expect(outcome.evaluation.decisions).toHaveLength(2);
    }
  });

  it("reports a dry run's field errors", async () => {
    const outcome = await useCatalogStore().dryRun({ event: {} as never });
    expect(outcome).toEqual({
      ok: false,
      message: 'Bad Request: event.id: required',
      fields: [{ field: 'event.id', message: 'event.id: required' }],
    });
  });

  it("lists a work item's recorded events, newest first", async () => {
    const store = useCatalogStore();
    const items = await store.recentWorkItems();
    expect('items' in items && items.items.length).toBeGreaterThan(0);
    const result = await store.recordedEvents(
      'wi_01JAC0ESCALATED0000000000000',
    );
    expect('events' in result).toBe(true);
    if ('events' in result) {
      const times = result.events.map((e) => Date.parse(e.time));
      expect(times).toEqual([...times].sort((a, b) => b - a));
    }
    expect(await store.recordedEvents('wi_nope')).toEqual({
      error: expect.stringContaining('Not Found'),
    });
  });
});
