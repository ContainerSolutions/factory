import createClient from 'openapi-fetch';
import type { components, paths } from './schema';

/** The API's base path on the dispatcher's public listener (the spec's `servers`). */
export const API_BASE = '/api/v1';

export type Schemas = components['schemas'];

/**
 * A typed client for the dispatcher's API. The UI is served from the same origin as the API,
 * so the session cookie goes with every request and nothing else is needed for auth.
 */
export function createApiClient(origin: string = window.location.origin) {
  return createClient<paths>({
    baseUrl: new URL(API_BASE, origin).href,
    credentials: 'same-origin',
    // Looked up per request, not captured at creation, so a fetch patched later (MSW in the
    // specs) is the one used.
    fetch: (request) => globalThis.fetch(request),
  });
}

export type ApiClient = ReturnType<typeof createApiClient>;

export const api = createApiClient();
