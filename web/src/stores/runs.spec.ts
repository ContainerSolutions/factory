import { beforeEach, describe, expect, it } from 'vitest';
import { createPinia, setActivePinia } from 'pinia';
import { delay, http, HttpResponse } from 'msw';
import type { Schemas } from '@/api/client';
import { examples } from '@/mocks/examples';
import { server } from '@/mocks/node';
import { liveRun } from '@/mocks/workitems';
import { MAX_STREAMED, useRunsStore } from './runs';

const example = examples.get('getRun') as Schemas['RunDetail'];
const EXAMPLE = example.run.metadata.name;
const LIVE = liveRun.metadata.name;

function problem(status: number, title: string) {
  return HttpResponse.json<Schemas['Problem']>(
    { type: 'about:blank', title, status },
    { status, headers: { 'Content-Type': 'application/problem+json' } },
  );
}

function streamed(at: string, message: string): Schemas['StreamProgress'] {
  return {
    run: LIVE,
    task: liveRun.spec.task,
    event: { at, kind: 'tool', message },
  };
}

describe('runs store', () => {
  beforeEach(() => setActivePinia(createPinia()));

  it('loads a run with its effects', async () => {
    const store = useRunsStore();
    await store.loadRun(EXAMPLE);
    expect(store.current).toEqual(example);
    expect(store.progress).toEqual(example.run.status!.progress);
    expect(store.currentLoading).toBe(false);
  });

  it('says when the run does not exist, or fails', async () => {
    const store = useRunsStore();
    await store.loadRun('run_missing');
    expect(store.currentNotFound).toBe(true);
    expect(store.current).toBeNull();

    server.use(http.get('/api/v1/runs/:id', () => problem(503, 'Unavailable')));
    await store.loadRun('run_other');
    expect(store.currentNotFound).toBe(false);
    expect(store.currentError).toBe('Unavailable');
  });

  it('merges streamed progress with the ring buffer, once each, and ignores other runs', async () => {
    const store = useRunsStore();
    const pending = store.loadRun(LIVE);
    // Streamed before the snapshot arrives: kept.
    store.addProgress(streamed('2026-10-19T11:58:00Z', 'read CHANGELOG.md'));
    await pending;
    store.addProgress({
      ...streamed('2026-10-19T11:59:00Z', 'x'),
      run: 'run_other',
    });
    // The ring buffer's newest event, streamed again after a reconnect.
    store.addProgress({
      run: LIVE,
      event: liveRun.status!.progress![1]!,
    });
    expect(store.progress.map((e) => e.message)).toEqual([
      'cloning platform/release-notes',
      'read repos.yaml',
      'read CHANGELOG.md',
    ]);
  });

  it('keeps the newest streamed events only', async () => {
    const store = useRunsStore();
    await store.loadRun(LIVE);
    for (let i = 0; i < MAX_STREAMED + 5; i++) {
      store.addProgress(
        streamed(
          new Date(Date.UTC(2026, 9, 19, 12, 0, i)).toISOString(),
          `e${i}`,
        ),
      );
    }
    const messages = store.progress.map((e) => e.message);
    expect(messages).toHaveLength(2 + MAX_STREAMED);
    expect(messages[2]).toBe('e5');
  });

  it('reloads quietly: keeps the page and the streamed progress, and a failure changes nothing', async () => {
    const store = useRunsStore();
    await store.loadRun(LIVE);
    store.addProgress(streamed('2026-10-19T11:58:00Z', 'read CHANGELOG.md'));

    const ended: Schemas['RunDetail'] = {
      run: {
        ...liveRun,
        status: {
          ...liveRun.status,
          end: 'completed',
          endedAt: '2026-10-19T12:01:00Z',
        },
      },
      effects: [],
    };
    server.use(
      http.get('/api/v1/runs/:id', async () => {
        await delay(10);
        return HttpResponse.json(ended);
      }),
    );
    const reload = store.loadRun(LIVE, { quiet: true });
    expect(store.current?.run.metadata.name).toBe(LIVE);
    await reload;
    expect(store.current).toEqual(ended);
    expect(store.progress).toHaveLength(3);

    server.use(http.get('/api/v1/runs/:id', () => problem(500, 'Boom')));
    await store.loadRun(LIVE, { quiet: true });
    expect(store.current).toEqual(ended);
    expect(store.currentError).toBeNull();
  });

  it('drops the response for an earlier run', async () => {
    server.use(
      http.get('/api/v1/runs/:id', async ({ params }) => {
        if (params.id === 'run_slow') await delay(50);
        return HttpResponse.json<Schemas['RunDetail']>({
          ...example,
          run: {
            ...example.run,
            metadata: { name: String(params.id) },
          },
        });
      }),
    );
    const store = useRunsStore();
    const slow = store.loadRun('run_slow');
    await store.loadRun('run_fast');
    await slow;
    expect(store.current?.run.metadata.name).toBe('run_fast');
  });

  it('loads, decompresses and parses the transcript', async () => {
    const store = useRunsStore();
    await store.loadRun(EXAMPLE);
    expect(store.transcriptState).toBe('idle');
    const loading = store.loadTranscript();
    expect(store.transcriptState).toBe('loading');
    await loading;
    expect(store.transcriptState).toBe('loaded');
    expect(store.transcript?.map((e) => e.type)).toEqual([
      'session',
      'message',
      'tool',
      'message',
    ]);
  });

  it('says when there is no transcript, or it fails', async () => {
    const store = useRunsStore();
    await store.loadRun(LIVE);
    await store.loadTranscript();
    expect(store.transcriptState).toBe('missing');

    server.use(
      http.get('/api/v1/runs/:id/transcript', () =>
        problem(502, 'Bad Gateway'),
      ),
    );
    await store.loadTranscript();
    expect(store.transcriptState).toBe('error');
    expect(store.transcriptError).toBe('Bad Gateway');
  });

  it('drops a transcript that arrives after another run was opened', async () => {
    server.use(
      http.get('/api/v1/runs/:id/transcript', async () => {
        await delay(30);
        return new HttpResponse('{"type":"message"}\n', {
          headers: { 'Content-Type': 'application/gzip' },
        });
      }),
    );
    const store = useRunsStore();
    await store.loadRun(EXAMPLE);
    const transcript = store.loadTranscript();
    await store.loadRun(LIVE);
    await transcript;
    expect(store.transcriptState).toBe('idle');
    expect(store.transcript).toBeNull();
  });

  it('has nothing to load before a run', async () => {
    const store = useRunsStore();
    await store.loadTranscript();
    expect(store.transcriptState).toBe('idle');
  });
});
