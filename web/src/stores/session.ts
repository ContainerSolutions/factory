import { defineStore } from 'pinia';
import { computed, ref } from 'vue';
import { api, describeProblem, type Schemas } from '@/api/client';

/**
 * Where the session stands: not checked yet, signed in, signed out (`GET /auth/me` said 401), or
 * unknown because the check failed some other way (the dispatcher is unreachable).
 */
export type SessionStatus = 'unknown' | 'signed-in' | 'signed-out' | 'error';

function message(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/**
 * The signed-in user, from `GET /auth/me`. The session itself is the dispatcher's `HttpOnly`
 * cookie: nothing about it is kept here or in the browser's storage.
 */
export const useSessionStore = defineStore('session', () => {
  const me = ref<Schemas['Me'] | null>(null);
  const status = ref<SessionStatus>('unknown');
  const error = ref<string | null>(null);
  const loaded = computed(() => status.value !== 'unknown');
  /**
   * Whether the user may send commands: a member of the `maintainers` Group (`isAdmin`). Readers
   * see no controls; the server enforces it anyway.
   */
  const canAct = computed(() => me.value?.isAdmin === true);

  /** Forgets the user: the session expired (a request answered 401) or was signed out. */
  function expire(): void {
    me.value = null;
    status.value = 'signed-out';
    error.value = null;
  }

  /** Checks the session. Never throws: a failure is `status: 'error'` with its message. */
  async function load(): Promise<void> {
    try {
      const { data, error: problem, response } = await api.GET('/auth/me');
      if (data) {
        me.value = data;
        status.value = 'signed-in';
        error.value = null;
      } else if (response.status === 401) {
        expire();
      } else {
        me.value = null;
        status.value = 'error';
        error.value = describeProblem(problem, response);
      }
    } catch (e) {
      me.value = null;
      status.value = 'error';
      error.value = message(e);
    }
  }

  let pending: Promise<void> | null = null;

  /** Checks the session once; later calls wait for the same check. */
  function ensure(): Promise<void> {
    if (status.value !== 'unknown') return Promise.resolve();
    pending ??= load().finally(() => {
      pending = null;
    });
    return pending;
  }

  /** Signs out (`POST /auth/logout`). Returns why it failed, or null. */
  async function signOut(): Promise<string | null> {
    try {
      const { error: problem, response } = await api.POST('/auth/logout');
      if (!response.ok) return describeProblem(problem, response);
    } catch (e) {
      return message(e);
    }
    expire();
    return null;
  }

  return { me, status, error, loaded, canAct, load, ensure, expire, signOut };
});
