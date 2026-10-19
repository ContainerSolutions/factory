import { afterEach, describe, expect, it } from 'vitest';
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils';
import { createPinia } from 'pinia';
import PrimeVue from 'primevue/config';
import { http, HttpResponse } from 'msw';
import { createMemoryHistory } from 'vue-router';
import { confirmDialog, recordCommands } from '@/__tests__/commands';
import { server } from '@/mocks/node';
import { reader } from '@/mocks/session';
import { useSessionStore } from '@/stores/session';
import App from './App.vue';
import { createAppRouter } from './router';
import { installSessionGuard } from './router/session';

let mounted: VueWrapper | undefined;
let removeGuard: (() => void) | undefined;

async function mountAt(path: string) {
  const pinia = createPinia();
  const router = createAppRouter(createMemoryHistory());
  removeGuard = installSessionGuard(router, useSessionStore(pinia));
  await router.push(path);
  const wrapper = mount(App, {
    attachTo: document.body,
    global: {
      plugins: [pinia, router, [PrimeVue, { unstyled: true }]],
    },
  });
  mounted = wrapper;
  await flushPromises();
  return { wrapper, router };
}

afterEach(() => {
  mounted?.unmount();
  mounted = undefined;
  removeGuard?.();
});

const signedOut = () =>
  http.get('/api/v1/auth/me', () =>
    HttpResponse.json(
      { title: 'Unauthorized', status: 401 },
      {
        status: 401,
        headers: { 'Content-Type': 'application/problem+json' },
      },
    ),
  );

describe('App shell', () => {
  it('renders the navigation and the signed-in user', async () => {
    const { wrapper } = await mountAt('/');
    const links = wrapper.get('nav[aria-label="Main"]').findAll('a');
    expect(links.map((a) => [a.text(), a.attributes('href')])).toEqual([
      ['Board', '/'],
      ['Pools', '/pools'],
      ['Catalog', '/catalog'],
    ]);
    expect(wrapper.get('[data-testid="session-login"]').text()).toBe(
      'gusfcarvalho',
    );
    expect(wrapper.find('[data-testid="read-only"]').exists()).toBe(false);
    expect(wrapper.get('h1').text()).toBe('Board');
  });

  it.each([
    [
      '/workitems/wi_01JAB3J9T1D2E3F4G5H6J7K8LM',
      'compliance-framework/docs#42',
    ],
    ['/pools', 'Queues and pools'],
    ['/catalog', 'Catalog'],
    ['/nowhere', 'Not found'],
  ])('routes %s to its view', async (path, title) => {
    const { wrapper } = await mountAt(path);
    expect(wrapper.get('h1').text()).toBe(title);
  });

  it('navigates back to the board from an unknown page', async () => {
    const { wrapper, router } = await mountAt('/nowhere');
    await wrapper.get('main button').trigger('click');
    await flushPromises();
    expect(router.currentRoute.value.name).toBe('board');
  });

  it('pauses the whole factory from the header', async () => {
    const bodies = recordCommands();
    const { wrapper } = await mountAt('/');
    await wrapper
      .get('header button[aria-label="Pause the whole factory"]')
      .trigger('click');
    await flushPromises();
    await confirmDialog('Pause');
    expect(bodies).toEqual([{ type: 'pause', target: { scope: 'global' } }]);
    expect(wrapper.get('header [data-testid="pause-state"]').text()).toBe(
      'pause sent',
    );
  });

  it('marks a reader read-only, without controls', async () => {
    server.use(http.get('/api/v1/auth/me', () => HttpResponse.json(reader)));
    const { wrapper } = await mountAt('/');
    expect(wrapper.get('[data-testid="session-login"]').text()).toBe('octocat');
    expect(wrapper.get('[data-testid="read-only"]').text()).toBe('read-only');
    expect(wrapper.find('[data-testid="pause-controls"]').exists()).toBe(false);
  });

  it('sends a user without a session to the sign-in page, alone', async () => {
    server.use(signedOut());
    const { wrapper, router } = await mountAt('/pools');
    expect(router.currentRoute.value.fullPath).toBe('/login?returnTo=/pools');
    expect(wrapper.get('h1').text()).toBe('Sign in');
    expect(wrapper.find('nav[aria-label="Main"]').exists()).toBe(false);
    expect(wrapper.get('[data-testid="sign-in"]').attributes('href')).toBe(
      '/api/v1/auth/login?returnTo=%2Fpools',
    );
  });

  it('signs out to the sign-in page', async () => {
    const { wrapper, router } = await mountAt('/pools');
    await wrapper
      .findAll('header button')
      .find((b) => b.text() === 'Sign out')!
      .trigger('click');
    await flushPromises();
    expect(router.currentRoute.value.name).toBe('login');
    expect(wrapper.find('[data-testid="session-login"]').exists()).toBe(false);
  });

  it('says when signing out failed', async () => {
    server.use(
      http.post('/api/v1/auth/logout', () =>
        HttpResponse.json(
          { title: 'Bad Gateway', status: 502 },
          { status: 502 },
        ),
      ),
    );
    const { wrapper, router } = await mountAt('/pools');
    await wrapper
      .findAll('header button')
      .find((b) => b.text() === 'Sign out')!
      .trigger('click');
    await flushPromises();
    expect(router.currentRoute.value.name).toBe('pools');
    expect(wrapper.get('[role="alert"]').text()).toBe(
      "Couldn't sign out: Bad Gateway",
    );
  });
});
