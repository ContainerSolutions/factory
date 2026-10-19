import createClient from 'openapi-fetch';
import type { components, paths } from './schema';

/** The API's base path on the dispatcher's public listener (the spec's `servers`). */
export const API_BASE = '/api/v1';

export type Schemas = components['schemas'];

const unauthorized = new Set<() => void>();

/**
 * Calls `listener` whenever an API request answers 401 (the session cookie expired or was
 * revoked), except the `/auth/*` routes, whose 401 is how signing in is checked. Returns a
 * function that removes the listener.
 */
export function onUnauthorized(listener: () => void): () => void {
  unauthorized.add(listener);
  return () => unauthorized.delete(listener);
}

/**
 * A typed client for the dispatcher's API. The UI is served from the same origin as the API,
 * so the session cookie goes with every request and nothing else is needed for auth.
 */
export function createApiClient(origin: string = window.location.origin) {
  const client = createClient<paths>({
    baseUrl: new URL(API_BASE, origin).href,
    credentials: 'same-origin',
    // Looked up per request, not captured at creation, so a fetch patched later (MSW in the
    // specs) is the one used.
    fetch: (request) => globalThis.fetch(request),
  });
  client.use({
    onResponse({ schemaPath, response }) {
      if (response.status === 401 && !schemaPath.startsWith('/auth/')) {
        for (const listener of unauthorized) listener();
      }
      return undefined;
    },
  });
  return client;
}

export type ApiClient = ReturnType<typeof createApiClient>;

export const api = createApiClient();

/** A failed response as one line: the Problem's title and detail, else the HTTP status. */
export function describeProblem(problem: unknown, response: Response): string {
  const p = (problem ?? {}) as Partial<Schemas['Problem']>;
  const title = typeof p.title === 'string' && p.title ? p.title : '';
  const detail = typeof p.detail === 'string' && p.detail ? p.detail : '';
  if (title && detail) return `${title}: ${detail}`;
  return title || detail || `HTTP ${response.status}`;
}
