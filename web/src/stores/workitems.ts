import { defineStore } from 'pinia';
import { ref } from 'vue';
import { api, describeProblem, type Schemas } from '@/api/client';
import { describeCommandFailure } from '@/components/controls/commands';

/** The answer to an admin request (replay, close, delete): its body, or why it was refused. */
export type AdminOutcome<T> =
  | { ok: true; data: T }
  | { ok: false; status?: number; code?: string; message: string };

const ADMIN = 'replay events, close or delete work items';

function refused<T>(problem: unknown, response: Response): AdminOutcome<T> {
  const code = (problem as Partial<Schemas['Problem']> | undefined)?.code;
  return {
    ok: false,
    status: response.status,
    ...(code ? { code } : {}),
    message: describeCommandFailure(problem, response, ADMIN),
  };
}

function failed<T>(e: unknown): AdminOutcome<T> {
  return { ok: false, message: e instanceof Error ? e.message : String(e) };
}

/** Whether `a` is an older version than `b`; versions that aren't numbers never are. */
function isOlder(a: Schemas['WorkItem'], b: Schemas['WorkItem']): boolean {
  const va = Number(a.metadata.resourceVersion);
  const vb = Number(b.metadata.resourceVersion);
  return Number.isFinite(va) && Number.isFinite(vb) && va < vb;
}

/** Work items for the board (`GET /workitems`) and the open work item (`GET /workitems/{id}`). */
export const useWorkItemsStore = defineStore('workitems', () => {
  const items = ref<Schemas['WorkItem'][]>([]);
  const nextCursor = ref<string | undefined>();
  const loading = ref(false);
  const loaded = ref(false);
  const error = ref<string | null>(null);

  async function fetchPage(cursor?: string): Promise<void> {
    loading.value = true;
    error.value = null;
    try {
      const {
        data,
        error: problem,
        response,
      } = await api.GET('/workitems', {
        params: { query: cursor ? { cursor } : {} },
      });
      if (!data) {
        error.value = describeProblem(problem, response);
        return;
      }
      items.value = cursor ? [...items.value, ...data.items] : data.items;
      nextCursor.value = data.nextCursor;
      loaded.value = true;
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e);
    } finally {
      loading.value = false;
    }
  }

  /** Loads the first page, replacing what was loaded. */
  function load(): Promise<void> {
    return fetchPage();
  }

  /** Appends the next page; a no-op on the last page. */
  async function loadMore(): Promise<void> {
    if (nextCursor.value) await fetchPage(nextCursor.value);
  }

  /**
   * Applies a `workitem.updated` event to the loaded work items: replaces the one with the same
   * name, unless the loaded one is newer (a larger `resourceVersion`), or adds it first. Nothing
   * changes before the first page is loaded, which will have it.
   */
  function upsert(item: Schemas['WorkItem']): void {
    if (!loaded.value) return;
    const i = items.value.findIndex(
      (w) => w.metadata.name === item.metadata.name,
    );
    if (i < 0) {
      items.value = [item, ...items.value];
      return;
    }
    if (isOlder(item, items.value[i]!)) return;
    items.value = items.value.map((w, j) => (j === i ? item : w));
  }

  const current = ref<Schemas['WorkItemDetail'] | null>(null);
  const currentLoading = ref(false);
  const currentError = ref<string | null>(null);
  const currentNotFound = ref(false);
  let requested = '';

  /**
   * Loads one work item with its timeline. A quiet load (after a replay) keeps what is shown, and
   * a failure leaves the page as it was. A response for an earlier request is dropped.
   */
  async function loadItem(
    id: string,
    { quiet = false }: { quiet?: boolean } = {},
  ): Promise<void> {
    if (!quiet || requested !== id) {
      requested = id;
      current.value = null;
      currentError.value = null;
      currentNotFound.value = false;
      currentLoading.value = true;
    }
    try {
      const {
        data,
        error: problem,
        response,
      } = await api.GET('/workitems/{id}', { params: { path: { id } } });
      if (requested !== id) return;
      if (data) {
        current.value = data;
      } else if (quiet) {
        return;
      } else if (response.status === 404) {
        currentNotFound.value = true;
      } else {
        currentError.value = describeProblem(problem, response);
      }
    } catch (e) {
      if (requested === id && !quiet) {
        currentError.value = e instanceof Error ? e.message : String(e);
      }
    } finally {
      if (requested === id) currentLoading.value = false;
    }
  }

  /** Replays a stored event through the rules (`POST /events/{id}/replay`, admin). */
  async function replayEvent(
    event: string,
  ): Promise<AdminOutcome<Schemas['ReplayResult']>> {
    try {
      const {
        data,
        error: problem,
        response,
      } = await api.POST('/events/{id}/replay', {
        params: { path: { id: event } },
      });
      return data ? { ok: true, data } : refused(problem, response);
    } catch (e) {
      return failed(e);
    }
  }

  /**
   * Closes a work item (`POST /workitems/{id}/close`, admin), and shows the closed item on the
   * board and on its page.
   */
  async function closeItem(
    id: string,
    body: Schemas['CloseWorkItemRequest'],
  ): Promise<AdminOutcome<Schemas['WorkItem']>> {
    try {
      const {
        data,
        error: problem,
        response,
      } = await api.POST('/workitems/{id}/close', {
        params: { path: { id } },
        body,
      });
      if (!data) return refused(problem, response);
      upsert(data);
      if (current.value?.workItem.metadata.name === id) {
        current.value = { ...current.value, workItem: data };
      }
      return { ok: true, data };
    } catch (e) {
      return failed(e);
    }
  }

  /**
   * Deletes a work item and everything keyed to it (`DELETE /workitems/{id}`, admin). Without
   * `force` a leased task refuses it: `409` with code `task-leased`.
   */
  async function deleteItem(
    id: string,
    force = false,
  ): Promise<AdminOutcome<Schemas['WorkItemDeleted']>> {
    try {
      const {
        data,
        error: problem,
        response,
      } = await api.DELETE('/workitems/{id}', {
        params: { path: { id }, query: force ? { force } : {} },
      });
      if (!data) return refused(problem, response);
      items.value = items.value.filter((w) => w.metadata.name !== id);
      return { ok: true, data };
    } catch (e) {
      return failed(e);
    }
  }

  return {
    items,
    nextCursor,
    loading,
    loaded,
    error,
    load,
    loadMore,
    upsert,
    current,
    currentLoading,
    currentError,
    currentNotFound,
    loadItem,
    replayEvent,
    closeItem,
    deleteItem,
  };
});
