import {
  flushPromises,
  mount,
  type ComponentMountingOptions,
} from '@vue/test-utils';
import { createPinia, setActivePinia } from 'pinia';
import PrimeVue from 'primevue/config';
import type { Component } from 'vue';
import { createMemoryHistory } from 'vue-router';
import { clockKey } from '@/composables/useClock';
import type { Schemas } from '@/api/client';
import { createAppRouter } from '@/router';
import { useSessionStore } from '@/stores/session';

/** The fixtures' "now": two days after the newest done work item's last activity. */
export const NOW = new Date('2026-10-19T12:00:00Z');

/**
 * Mounts a component with the app's router (at `path`), a fresh Pinia, PrimeVue unstyled and a
 * fixed clock, and waits for its requests to settle. With `me`, that user is signed in (a
 * maintainer sees the controls, a reader doesn't); otherwise the session isn't checked.
 */
export async function mountWithApp<C extends Component>(
  component: C,
  {
    path = '/',
    now = NOW,
    me,
    ...options
  }: ComponentMountingOptions<C> & {
    path?: string;
    now?: Date;
    me?: Schemas['Me'];
  } = {},
) {
  const pinia = createPinia();
  setActivePinia(pinia);
  if (me) {
    const session = useSessionStore();
    session.me = me;
    session.status = 'signed-in';
  }
  const router = createAppRouter(createMemoryHistory());
  await router.push(path);
  const wrapper = mount(component, {
    ...options,
    global: {
      plugins: [pinia, router, [PrimeVue, { unstyled: true }]],
      provide: { [clockKey]: () => now },
      ...options.global,
    },
  } as ComponentMountingOptions<C>);
  await flushPromises();
  return { wrapper, router };
}
