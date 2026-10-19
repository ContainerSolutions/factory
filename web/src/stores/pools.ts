import { defineStore } from 'pinia';
import { ref } from 'vue';
import { api, type Schemas } from '@/api/client';
import { collect, pageQuery as query, type PageResult } from '@/api/pages';

export { MAX_PAGES } from '@/api/pages';

function message(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/** Whether `a` is an older version than `b`; versions that aren't numbers never are. */
function isOlder(a: Schemas['Agent'], b: Schemas['Agent']): boolean {
  const va = Number(a.metadata.resourceVersion);
  const vb = Number(b.metadata.resourceVersion);
  return Number.isFinite(va) && Number.isFinite(vb) && va < vb;
}

/** One list of the pools view: its items, whether it loaded, and why it didn't. */
function useList<T>(fetchPage: (cursor?: string) => Promise<PageResult<T>>) {
  const items = ref<T[]>([]);
  const loaded = ref(false);
  const error = ref<string | null>(null);

  /** Loads every page. A quiet load keeps what is shown when it fails. */
  async function load({ quiet = false }: { quiet?: boolean } = {}) {
    if (!quiet) error.value = null;
    try {
      const result = await collect(fetchPage);
      if ('items' in result) {
        items.value = result.items;
        loaded.value = true;
        error.value = null;
      } else if (!quiet || !loaded.value) {
        error.value = result.error;
      }
    } catch (e) {
      if (!quiet || !loaded.value) error.value = message(e);
    }
  }

  return { items, loaded, error, load };
}

/** The queues (`GET /queues`), pools (`GET /pools`) and agents (`GET /agents`) of the pools view. */
export const usePoolsStore = defineStore('pools', () => {
  const queues = useList<Schemas['QueueView']>((cursor) =>
    api.GET('/queues', query(cursor)),
  );
  const pools = useList<Schemas['PoolView']>((cursor) =>
    api.GET('/pools', query(cursor)),
  );
  const agents = useList<Schemas['Agent']>((cursor) =>
    api.GET('/agents', query(cursor)),
  );
  const loading = ref(false);

  /** Loads the three lists; each fails on its own, so one error doesn't hide the others. */
  async function load({ quiet = false }: { quiet?: boolean } = {}) {
    if (!quiet) loading.value = true;
    try {
      await Promise.all([
        queues.load({ quiet }),
        pools.load({ quiet }),
        agents.load({ quiet }),
      ]);
    } finally {
      if (!quiet) loading.value = false;
    }
  }

  let queuesReload: Promise<void> | null = null;
  let queuesStale = false;

  /**
   * Reloads the queues quietly. The stream has no queue topic, so a task change (which moves a
   * queue's depth, running count and pending reasons) reloads them. Changes that arrive while a
   * reload runs are coalesced into one more reload after it.
   */
  function refreshQueues(): Promise<void> {
    if (queuesReload) {
      queuesStale = true;
      return queuesReload;
    }
    queuesReload = (async () => {
      try {
        do {
          queuesStale = false;
          await queues.load({ quiet: true });
        } while (queuesStale);
      } finally {
        queuesReload = null;
      }
    })();
    return queuesReload;
  }

  /**
   * Applies an `agent.updated` event: replaces the agent with the same name unless the loaded one
   * is newer (a larger `resourceVersion`), or adds it. Nothing changes before the agents are
   * loaded, which will have it.
   */
  function upsertAgent(agent: Schemas['Agent']): void {
    if (!agents.loaded.value) return;
    const list = agents.items.value;
    const i = list.findIndex((a) => a.metadata.name === agent.metadata.name);
    if (i < 0) {
      agents.items.value = [...list, agent];
      return;
    }
    if (isOlder(agent, list[i]!)) return;
    agents.items.value = list.map((a, j) => (j === i ? agent : a));
  }

  return {
    queues: queues.items,
    queuesLoaded: queues.loaded,
    queuesError: queues.error,
    pools: pools.items,
    poolsLoaded: pools.loaded,
    poolsError: pools.error,
    agents: agents.items,
    agentsLoaded: agents.loaded,
    agentsError: agents.error,
    loading,
    load,
    refreshQueues,
    upsertAgent,
  };
});
