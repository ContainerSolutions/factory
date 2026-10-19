import { beforeEach, describe, expect, it } from 'vitest';
import { createPinia, setActivePinia } from 'pinia';
import { http, HttpResponse } from 'msw';
import { server } from '@/mocks/node';
import { examples } from '@/mocks/examples';
import { useSessionStore } from './session';

describe('session store', () => {
  beforeEach(() => setActivePinia(createPinia()));

  it('loads the signed-in user', async () => {
    const session = useSessionStore();
    await session.load();
    expect(session.loaded).toBe(true);
    expect(session.me).toEqual(examples.get('getMe'));
  });

  it('has no user without a session', async () => {
    server.use(
      http.get('/api/v1/auth/me', () =>
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
      ),
    );
    const session = useSessionStore();
    await session.load();
    expect(session.loaded).toBe(true);
    expect(session.me).toBeNull();
  });
});
