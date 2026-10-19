import { defineStore } from 'pinia';
import { ref } from 'vue';
import { api, describeProblem, type Schemas } from '@/api/client';

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

  const current = ref<Schemas['WorkItemDetail'] | null>(null);
  const currentLoading = ref(false);
  const currentError = ref<string | null>(null);
  const currentNotFound = ref(false);
  let requested = '';

  /** Loads one work item with its timeline. A response for an earlier request is dropped. */
  async function loadItem(id: string): Promise<void> {
    requested = id;
    current.value = null;
    currentError.value = null;
    currentNotFound.value = false;
    currentLoading.value = true;
    try {
      const {
        data,
        error: problem,
        response,
      } = await api.GET('/workitems/{id}', { params: { path: { id } } });
      if (requested !== id) return;
      if (data) {
        current.value = data;
      } else if (response.status === 404) {
        currentNotFound.value = true;
      } else {
        currentError.value = describeProblem(problem, response);
      }
    } catch (e) {
      if (requested === id) {
        currentError.value = e instanceof Error ? e.message : String(e);
      }
    } finally {
      if (requested === id) currentLoading.value = false;
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
    current,
    currentLoading,
    currentError,
    currentNotFound,
    loadItem,
  };
});
