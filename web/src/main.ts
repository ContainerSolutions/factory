import './assets/main.css';

import { createApp } from 'vue';
import { createPinia } from 'pinia';
import PrimeVue from 'primevue/config';

import App from './App.vue';
import { createAppRouter } from './router';

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
  app.use(createPinia());
  app.use(createAppRouter());
  app.use(PrimeVue, { unstyled: true });
  app.mount('#app');
});
