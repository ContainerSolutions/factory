import { describe, expect, it } from 'vitest';
import { API_BASE } from '@/api/client';
import { MOCK_SESSION_COOKIE, maintainer, reader } from './session';

const url = (path: string) => `${window.location.origin}${API_BASE}${path}`;

function as(session: string): RequestInit {
  return { headers: { cookie: `${MOCK_SESSION_COOKIE}=${session}` } };
}

async function command(body: unknown, init: RequestInit = {}) {
  return fetch(url('/commands'), {
    ...init,
    method: 'POST',
    headers: { ...init.headers, 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
}

describe('mock session', () => {
  it('signs in as the maintainer by default, or as the cookie says', async () => {
    expect(await (await fetch(url('/auth/me'))).json()).toEqual(maintainer);
    expect(await (await fetch(url('/auth/me'), as('reader'))).json()).toEqual(
      reader,
    );
    expect((await fetch(url('/auth/me'), as('signed-out'))).status).toBe(401);
  });

  it('signs out until reset', async () => {
    expect((await fetch(url('/auth/logout'), { method: 'POST' })).status).toBe(
      204,
    );
    expect((await fetch(url('/auth/me'))).status).toBe(401);
    expect((await command({ type: 'resync-catalog' })).status).toBe(401);
  });
});

describe('mock commands', () => {
  it.each([
    [{ type: 'pause', target: { scope: 'global' } }, 202, { type: 'pause' }],
    [
      { type: 'pause', target: { scope: 'workitem', name: 'wi_1' } },
      202,
      { type: 'pause', event: 'factory/cmd-01JAB3R5S6T7V8W9X0Y1Z2A3BC' },
    ],
    [{ type: 'pause', target: { scope: 'pool', name: 'p' } }, 400, undefined],
    [{ type: 'resume', target: { scope: 'queue' } }, 400, undefined],
    [{ type: 'cancel', target: {} }, 400, undefined],
    [
      { type: 'redispatch', target: { task: 't' }, args: { harness: {} } },
      400,
      undefined,
    ],
    [{ type: 'dispatch' }, 400, undefined],
    [{ type: 'nope' }, 400, undefined],
  ])('answers %j with %i', async (body, status, want) => {
    const res = await command(body);
    expect(res.status).toBe(status);
    if (want) expect(await res.json()).toEqual(want);
  });

  it('refuses a reader', async () => {
    const res = await command({ type: 'resync-catalog' }, as('reader'));
    expect(res.status).toBe(403);
    expect((await res.json()).detail).toBe(
      'commands need the maintainers Group',
    );
  });
});
