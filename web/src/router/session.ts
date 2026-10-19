import type { RouteLocationRaw, Router } from 'vue-router';
import { onUnauthorized } from '@/api/client';
import type { useSessionStore } from '@/stores/session';

type SessionStore = ReturnType<typeof useSessionStore>;

/**
 * A same-origin path to return to after signing in, or `/`. Anything else (another origin,
 * `//host`, `/\host`, the sign-in page itself) is dropped, so the login link can't be made to
 * send the user elsewhere.
 */
export function safeReturnTo(value: unknown): string {
  const path = Array.isArray(value) ? value[0] : value;
  if (typeof path !== 'string' || !path.startsWith('/')) return '/';
  if (path.startsWith('//') || path.startsWith('/\\')) return '/';
  if (path === '/login' || path.startsWith('/login?')) return '/';
  return path;
}

/** The sign-in page, coming back to `returnTo`. */
export function loginRoute(returnTo: string): RouteLocationRaw {
  const path = safeReturnTo(returnTo);
  return { name: 'login', query: path === '/' ? {} : { returnTo: path } };
}

/**
 * Sends the user to the sign-in page when there is no session: before every navigation to a page
 * that needs one (`GET /auth/me` answered 401), and when any request answers 401 later (the
 * session expired). The sign-in page sends a signed-in user on to where they were going. When the
 * check fails some other way (the dispatcher is down), the pages render and show their own errors.
 * Returns a function that removes the 401 listener.
 */
export function installSessionGuard(
  router: Router,
  session: SessionStore,
): () => void {
  router.beforeEach(async (to) => {
    await session.ensure();
    if (to.meta.public) {
      return to.name === 'login' && session.status === 'signed-in'
        ? safeReturnTo(to.query.returnTo)
        : true;
    }
    return session.status === 'signed-out' ? loginRoute(to.fullPath) : true;
  });

  return onUnauthorized(() => {
    session.expire();
    const current = router.currentRoute.value;
    if (!current.meta.public) router.replace(loginRoute(current.fullPath));
  });
}
