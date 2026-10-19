import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { flushPromises } from '@vue/test-utils';
import { createPinia, setActivePinia } from 'pinia';
import { http, HttpResponse } from 'msw';
import { createMemoryHistory, type Router } from 'vue-router';
import { api } from '@/api/client';
import { server } from '@/mocks/node';
import { useSessionStore } from '@/stores/session';
import { createAppRouter } from '.';
import { installSessionGuard, loginRoute, safeReturnTo } from './session';

function status(code: number) {
  return () =>
    HttpResponse.json(
      { title: code === 401 ? 'Unauthorized' : 'Unavailable', status: code },
      {
        status: code,
        headers: { 'Content-Type': 'application/problem+json' },
      },
    );
}

let router: Router;
let remove: () => void;

beforeEach(() => {
  setActivePinia(createPinia());
  router = createAppRouter(createMemoryHistory());
  remove = installSessionGuard(router, useSessionStore());
});

afterEach(() => remove());

describe('safeReturnTo', () => {
  it.each([
    ['/runs/run_1?x=1', '/runs/run_1?x=1'],
    [['/pools', '/other'], '/pools'],
    ['/', '/'],
    ['https://evil.example', '/'],
    ['//evil.example/x', '/'],
    ['/\\evil.example', '/'],
    ['/login', '/'],
    ['/login?returnTo=/x', '/'],
    [undefined, '/'],
    [42, '/'],
  ])('%j → %s', (value, want) => {
    expect(safeReturnTo(value)).toBe(want);
  });

  it('builds the sign-in route without a returnTo for the board', () => {
    expect(loginRoute('/')).toEqual({ name: 'login', query: {} });
    expect(loginRoute('/pools')).toEqual({
      name: 'login',
      query: { returnTo: '/pools' },
    });
  });
});

describe('installSessionGuard', () => {
  it('lets a signed-in user through', async () => {
    await router.push('/pools');
    expect(router.currentRoute.value.name).toBe('pools');
  });

  it('sends a user without a session to sign in, coming back after', async () => {
    server.use(http.get('/api/v1/auth/me', status(401)));
    await router.push('/runs/run_1?tab=transcript');
    expect(router.currentRoute.value.name).toBe('login');
    expect(router.currentRoute.value.query).toEqual({
      returnTo: '/runs/run_1?tab=transcript',
    });
  });

  it('sends a signed-in user from the sign-in page to where they were going', async () => {
    await router.push('/login?returnTo=/catalog');
    expect(router.currentRoute.value.name).toBe('catalog');
  });

  it("doesn't follow a returnTo off the site", async () => {
    await router.push('/login?returnTo=//evil.example');
    expect(router.currentRoute.value.fullPath).toBe('/');
  });

  it('shows the sign-in page without a session', async () => {
    server.use(http.get('/api/v1/auth/me', status(401)));
    await router.push('/login');
    expect(router.currentRoute.value.name).toBe('login');
  });

  it('renders the pages when the session check fails some other way', async () => {
    server.use(http.get('/api/v1/auth/me', status(503)));
    await router.push('/pools');
    expect(router.currentRoute.value.name).toBe('pools');
    expect(useSessionStore().status).toBe('error');
  });

  it('sends the user to sign in when a request answers 401 later', async () => {
    await router.push('/pools?x=1');
    server.use(http.get('/api/v1/queues', status(401)));
    await api.GET('/queues');
    await flushPromises();
    expect(useSessionStore().status).toBe('signed-out');
    expect(router.currentRoute.value.name).toBe('login');
    expect(router.currentRoute.value.query).toEqual({ returnTo: '/pools?x=1' });
  });

  it('ignores a 401 from the auth routes themselves', async () => {
    await router.push('/pools');
    server.use(http.get('/api/v1/auth/me', status(401)));
    await api.GET('/auth/me');
    await flushPromises();
    expect(router.currentRoute.value.name).toBe('pools');
  });
});
