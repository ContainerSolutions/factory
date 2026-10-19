import { describe, expect, it } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import { createPinia } from 'pinia';
import PrimeVue from 'primevue/config';
import { createMemoryHistory } from 'vue-router';
import App from './App.vue';
import { createAppRouter } from './router';

async function mountAt(path: string) {
  const router = createAppRouter(createMemoryHistory());
  await router.push(path);
  const wrapper = mount(App, {
    global: {
      plugins: [createPinia(), router, [PrimeVue, { unstyled: true }]],
    },
  });
  await flushPromises();
  return { wrapper, router };
}

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
});
