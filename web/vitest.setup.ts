import { afterAll, afterEach, beforeAll } from 'vitest';
import { server } from '@/mocks/node';
import { resetMockSession } from '@/mocks/session';

// Every spec runs against the MSW handlers built from ../api/examples. A request no handler
// matches fails the spec, so a typo'd path can't silently hit the network.
beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterEach(() => {
  server.resetHandlers();
  resetMockSession();
});
afterAll(() => server.close());

// jsdom has no matchMedia, which PrimeVue's Select and Drawer call. Nothing matches.
if (typeof window !== 'undefined' && !window.matchMedia) {
  window.matchMedia = (query: string): MediaQueryList => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  });
}
