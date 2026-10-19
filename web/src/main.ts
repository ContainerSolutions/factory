import './assets/main.css';

import { createApp } from 'vue';
import { createPinia } from 'pinia';
import PrimeVue from 'primevue/config';

import App from './App.vue';
import { createAppRouter } from './router';
import { installSessionGuard } from './router/session';
import { useSessionStore } from './stores/session';

// `npm run dev:mock` serves the API from the MSW handlers (src/mocks) built from
// ../api/examples. The branch is dropped from every other build.
async function startMocks(): Promise<void> {
  if (import.meta.env.MODE === 'mock') {
    const { worker } = await import('./mocks/browser');
    await worker.start({ onUnhandledRequest: 'bypass' });
  }
}

startMocks().then(() => {
  const app = createApp(App);
  const pinia = createPinia();
  app.use(pinia);
  // The guard goes on before the router is installed, which runs the first navigation.
  const router = createAppRouter();
  installSessionGuard(router, useSessionStore(pinia));
  app.use(router);
  app.use(PrimeVue, { unstyled: true });
  app.mount('#app');
});
