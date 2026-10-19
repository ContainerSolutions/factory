import { describe, expect, it } from 'vitest';
import { api, API_BASE } from '@/api/client';
import { examples } from './examples';
import { exampleResponse, mswPath } from './handlers';
import { operations } from './operations.gen';

describe('mock operations', () => {
  it('has an example for every operation with a body, and an operation for every example', () => {
    const withBody = operations
      .filter((op) => op.contentType)
      .map((op) => op.id);
    expect([...examples.keys()].sort()).toEqual([...withBody].sort());
  });

  it.each([
    ['/api/v1/tasks/{id}/context/{n}', '/api/v1/tasks/:id/context/:n'],
    ['/api/v1/workitems', '/api/v1/workitems'],
  ])('maps %s to %s', (path, want) => {
    expect(mswPath(path)).toBe(want);
  });
});

describe('mock handlers', () => {
  it('serves a JSON example through the typed client', async () => {
    const { data, response } = await api.GET('/auth/me');
    expect(response.status).toBe(200);
    expect(data).toEqual(examples.get('getMe'));
  });

  it('serves path templates and the success status', async () => {
    const { data, response } = await api.GET('/tasks/{id}', {
      params: { path: { id: 'tsk_any' } },
    });
    expect(response.status).toBe(200);
    expect(data).toEqual(examples.get('getTask'));

    const res = await fetch(`${window.location.origin}${API_BASE}/agents`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    });
    expect(res.status).toBe(201);
    expect(await res.json()).toEqual(examples.get('registerAgent'));
  });

  it('serves a text example as text', async () => {
    const op = operations.find((o) => o.id === 'getHealthz')!;
    const res = exampleResponse(op, examples.get('getHealthz'));
    expect(res.headers.get('Content-Type')).toBe('text/plain');
    expect(await res.text()).toBe(examples.get('getHealthz'));
  });

  it('serves the transcript gzip-compressed', async () => {
    const res = await fetch(
      `${window.location.origin}${API_BASE}/runs/run_01JAB3M3P4Q5R6S7T8V9W0X1YZ/transcript`,
    );
    expect(res.headers.get('Content-Type')).toBe('application/gzip');
    const text = await new Response(
      res.body!.pipeThrough(new DecompressionStream('gzip')),
    ).text();
    expect(text).toBe(examples.get('getTranscript'));
  });

  it('answers a bodyless redirect with a Location', () => {
    const op = operations.find((o) => o.id === 'login')!;
    const res = exampleResponse(op, undefined);
    expect(res.status).toBe(302);
    expect(res.headers.get('Location')).toBe('/');
  });
});
