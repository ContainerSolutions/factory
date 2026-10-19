import { afterAll, afterEach, beforeAll } from 'vitest';
import { server } from '@/mocks/node';

// Every spec runs against the MSW handlers built from ../api/examples. A request no handler
// matches fails the spec, so a typo'd path can't silently hit the network.
beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());
