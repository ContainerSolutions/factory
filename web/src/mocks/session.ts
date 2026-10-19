import { http, HttpResponse, type HttpHandler } from 'msw';
import { API_BASE, type Schemas } from '@/api/client';
import { examples } from './examples';

/**
 * Who the mock API thinks is signed in, from a cookie (the real session is a cookie too, so a
 * Playwright test picks a user with `context.addCookies`): `reader` is a member of the readers
 * Group only, `signed-out` has no session, anything else (or no cookie) is getMe's example, a
 * maintainer. `POST /auth/logout` signs out until the page reloads, which the dev server's mock
 * sign-in does; it sets no cookie, since MSW would keep a mocked one in its own store.
 */
export const MOCK_SESSION_COOKIE = 'factory-mock-session';

export type MockSession = 'maintainer' | 'reader' | 'signed-out';

export const maintainer = examples.get('getMe') as Schemas['Me'];

export const reader: Schemas['Me'] = {
  login: 'octocat',
  name: 'The Octocat',
  isAdmin: false,
};

let signedOut = false;

/** Forgets a mock sign-out (the specs do after each test). */
export function resetMockSession(): void {
  signedOut = false;
}

export function mockSession(cookies: Record<string, string>): MockSession {
  if (signedOut) return 'signed-out';
  const value = cookies[MOCK_SESSION_COOKIE];
  return value === 'reader' || value === 'signed-out' ? value : 'maintainer';
}

export function problem(
  status: number,
  title: string,
  code: string,
  detail: string,
  errors?: Schemas['FieldError'][],
): Response {
  const body: Schemas['Problem'] = {
    type: 'about:blank',
    title,
    status,
    code,
    detail,
  };
  if (errors) body.errors = errors;
  return HttpResponse.json<Schemas['Problem']>(body, {
    status,
    headers: { 'Content-Type': 'application/problem+json' },
  });
}

/** `GET /auth/me` per the mock session, and `POST /auth/logout`, which ends it. */
export const sessionHandlers: HttpHandler[] = [
  http.get(`${API_BASE}/auth/me`, ({ cookies }) => {
    switch (mockSession(cookies)) {
      case 'signed-out':
        return problem(401, 'Unauthorized', 'unauthenticated', 'no session');
      case 'reader':
        return HttpResponse.json<Schemas['Me']>(reader);
      default:
        return HttpResponse.json<Schemas['Me']>(maintainer);
    }
  }),
  http.post(`${API_BASE}/auth/logout`, () => {
    signedOut = true;
    return new HttpResponse(null, { status: 204 });
  }),
];
