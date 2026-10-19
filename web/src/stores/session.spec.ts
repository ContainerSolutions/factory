import { beforeEach, describe, expect, it } from 'vitest';
import { createPinia, setActivePinia } from 'pinia';
import { http, HttpResponse } from 'msw';
import { server } from '@/mocks/node';
import { examples } from '@/mocks/examples';
import { reader } from '@/mocks/session';
import { useSessionStore } from './session';

function unauthorized() {
  return http.get('/api/v1/auth/me', () =>
    HttpResponse.json(
      {
        type: 'about:blank',
        title: 'Unauthorized',
        status: 401,
        code: 'unauthenticated',
      },
      {
        status: 401,
        headers: { 'Content-Type': 'application/problem+json' },
      },
    ),
  );
}

describe('session store', () => {
  beforeEach(() => setActivePinia(createPinia()));

  it('loads the signed-in user', async () => {
    const session = useSessionStore();
    expect(session.loaded).toBe(false);
    await session.load();
    expect(session.loaded).toBe(true);
    expect(session.status).toBe('signed-in');
    expect(session.me).toEqual(examples.get('getMe'));
    expect(session.canAct).toBe(true);
  });

  it('has no user without a session', async () => {
    server.use(unauthorized());
    const session = useSessionStore();
    await session.load();
    expect(session.loaded).toBe(true);
    expect(session.status).toBe('signed-out');
    expect(session.me).toBeNull();
    expect(session.canAct).toBe(false);
  });

  it("doesn't let a reader act", async () => {
    server.use(http.get('/api/v1/auth/me', () => HttpResponse.json(reader)));
    const session = useSessionStore();
    await session.load();
    expect(session.me?.login).toBe('octocat');
    expect(session.canAct).toBe(false);
  });

  it('says why the check failed when it is not a 401', async () => {
    server.use(
      http.get('/api/v1/auth/me', () =>
        HttpResponse.json(
          { title: 'Service Unavailable', status: 503, detail: 'starting' },
          { status: 503 },
        ),
      ),
    );
    const session = useSessionStore();
    await session.load();
    expect(session.status).toBe('error');
    expect(session.error).toBe('Service Unavailable: starting');
    expect(session.me).toBeNull();
  });

  it('survives a network failure', async () => {
    server.use(http.get('/api/v1/auth/me', () => HttpResponse.error()));
    const session = useSessionStore();
    await session.load();
    expect(session.status).toBe('error');
    expect(session.error).toBeTruthy();
  });

  it('checks once however often it is asked', async () => {
    let calls = 0;
    server.use(
      http.get('/api/v1/auth/me', () => {
        calls++;
        return HttpResponse.json(examples.get('getMe') as object);
      }),
    );
    const session = useSessionStore();
    await Promise.all([session.ensure(), session.ensure()]);
    await session.ensure();
    expect(calls).toBe(1);
    expect(session.status).toBe('signed-in');
  });

  it('signs out', async () => {
    const session = useSessionStore();
    await session.load();
    expect(await session.signOut()).toBeNull();
    expect(session.status).toBe('signed-out');
    expect(session.me).toBeNull();
  });

  it('stays signed in when signing out fails', async () => {
    server.use(
      http.post('/api/v1/auth/logout', () =>
        HttpResponse.json(
          { title: 'Internal Server Error', status: 500 },
          { status: 500 },
        ),
      ),
    );
    const session = useSessionStore();
    await session.load();
    expect(await session.signOut()).toBe('Internal Server Error');
    expect(session.status).toBe('signed-in');
  });
});
