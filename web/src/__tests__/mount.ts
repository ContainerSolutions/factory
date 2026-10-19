import {
  flushPromises,
  mount,
  type ComponentMountingOptions,
} from '@vue/test-utils';
import { createPinia } from 'pinia';
import PrimeVue from 'primevue/config';
import type { Component } from 'vue';
import { createMemoryHistory } from 'vue-router';
import { clockKey } from '@/composables/useClock';
import { createAppRouter } from '@/router';

/** The fixtures' "now": two days after the newest done work item's last activity. */
export const NOW = new Date('2026-10-19T12:00:00Z');

/**
 * Mounts a component with the app's router (at `path`), a fresh Pinia, PrimeVue unstyled and a
 * fixed clock, and waits for its requests to settle.
 */
export async function mountWithApp<C extends Component>(
  component: C,
  {
    path = '/',
    now = NOW,
    ...options
  }: ComponentMountingOptions<C> & { path?: string; now?: Date } = {},
) {
  const router = createAppRouter(createMemoryHistory());
  await router.push(path);
  const wrapper = mount(component, {
    ...options,
    global: {
      plugins: [createPinia(), router, [PrimeVue, { unstyled: true }]],
      provide: { [clockKey]: () => now },
      ...options.global,
    },
  } as ComponentMountingOptions<C>);
  await flushPromises();
  return { wrapper, router };
}
