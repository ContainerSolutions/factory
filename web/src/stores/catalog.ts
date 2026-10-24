import { defineStore } from 'pinia';
import { computed, ref } from 'vue';
import { api, describeProblem, type Schemas } from '@/api/client';
import { collect, pageQuery } from '@/api/pages';

type Kind = Schemas['CatalogKind'];
type CatalogObject = Schemas['CatalogObject'];

/** The kinds whose objects the catalog view shows in full (each is one `GET /catalog/{kind}/{name}`). */
export const DETAIL_KINDS = [
  'RuleSet',
  'Rule',
  'TaskSpec',
  'Queue',
  'Pool',
  'Group',
  'Template',
] as const satisfies readonly Kind[];

type DetailKind = (typeof DETAIL_KINDS)[number];

/** How many object requests run at once. */
const PARALLEL = 6;

function message(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/** Runs `fn` over `items`, at most `limit` at a time. */
async function eachLimit<T>(
  items: readonly T[],
  limit: number,
  fn: (item: T) => Promise<void>,
): Promise<void> {
  let next = 0;
  const worker = async () => {
    while (next < items.length) await fn(items[next++]!);
  };
  await Promise.all(
    Array.from({ length: Math.min(limit, items.length) }, worker),
  );
}

export type DryRunOutcome =
  | { ok: true; evaluation: Schemas['Evaluation'] }
  | { ok: false; message: string; fields: Schemas['FieldError'][] };

/**
 * The loaded catalog (`GET /catalog`), its providers with their health (`GET /providers`), the
 * RuleSets, Rules, TaskSpecs, Queues, Pools, Groups and Templates it lists, each fetched in full,
 * and its prompt and schema files (`GET /catalog/files`; each file's content on demand). The
 * contract has no bulk read of catalog objects, so that is one request per object.
 */
export const useCatalogStore = defineStore('catalog', () => {
  const summary = ref<Schemas['CatalogSummary'] | null>(null);
  const summaryError = ref<string | null>(null);
  const providers = ref<Schemas['ProviderView'][]>([]);
  const providersLoaded = ref(false);
  const providersError = ref<string | null>(null);
  const objects = ref<Partial<Record<Kind, Record<string, CatalogObject>>>>({});
  /** One line per object that didn't load: `RuleSet docs: Not Found: …`. */
  const objectErrors = ref<string[]>([]);
  const loading = ref(false);
  const files = ref<Schemas['CatalogFileRef'][]>([]);
  const filesError = ref<string | null>(null);
  /** The files fetched so far, by path, and why the others failed. */
  const fileContents = ref<Record<string, Schemas['CatalogFile']>>({});
  const fileErrors = ref<Record<string, string>>({});
  /** Bumped when the file contents are dropped (a load): a file fetched before is stale. */
  const filesVersion = ref(0);

  function of<K extends DetailKind>(kind: K) {
    return computed(() => {
      const byName: Record<string, CatalogObject> = objects.value[kind] ?? {};
      return Object.values(byName).sort((a, b) =>
        a.metadata.name.localeCompare(b.metadata.name),
      ) as Extract<CatalogObject, { kind: K }>[];
    });
  }
  const ruleSets = of('RuleSet');
  const rules = of('Rule');
  const taskSpecs = of('TaskSpec');
  const queues = of('Queue');
  const pools = of('Pool');
  const groups = of('Group');
  const templates = of('Template');

  /** The names of the listed objects of `kind`, sorted. */
  function namesOf(kind: Kind): string[] {
    return (summary.value?.objects ?? [])
      .filter((o) => o.kind === kind)
      .map((o) => o.name)
      .sort();
  }

  async function loadObjects(list: Schemas['CatalogObjectRef'][]) {
    const wanted = list.filter((o) =>
      (DETAIL_KINDS as readonly Kind[]).includes(o.kind),
    );
    const next: Partial<Record<Kind, Record<string, CatalogObject>>> = {};
    const errors: string[] = [];
    await eachLimit(wanted, PARALLEL, async ({ kind, name }) => {
      try {
        const { data, error, response } = await api.GET(
          '/catalog/{kind}/{name}',
          { params: { path: { kind, name } } },
        );
        if (data) (next[kind] ??= {})[name] = data;
        else
          errors.push(`${kind} ${name}: ${describeProblem(error, response)}`);
      } catch (e) {
        errors.push(`${kind} ${name}: ${message(e)}`);
      }
    });
    objects.value = next;
    objectErrors.value = errors.sort();
  }

  async function loadSummary(): Promise<void> {
    try {
      const { data, error, response } = await api.GET('/catalog');
      if (!data) {
        summaryError.value = describeProblem(error, response);
        return;
      }
      summary.value = data;
      summaryError.value = null;
      await loadObjects(data.objects);
    } catch (e) {
      summaryError.value = message(e);
    }
  }

  async function loadProviders(): Promise<void> {
    try {
      const result = await collect((cursor) =>
        api.GET('/providers', pageQuery(cursor)),
      );
      if ('items' in result) {
        providers.value = result.items;
        providersLoaded.value = true;
        providersError.value = null;
      } else {
        providersError.value = result.error;
      }
    } catch (e) {
      providersError.value = message(e);
    }
  }

  async function loadFiles(): Promise<void> {
    fileContents.value = {};
    fileErrors.value = {};
    filesVersion.value++;
    try {
      const { data, error, response } = await api.GET('/catalog/files');
      if (data) {
        files.value = data.items;
        filesError.value = null;
      } else filesError.value = describeProblem(error, response);
    } catch (e) {
      filesError.value = message(e);
    }
  }

  /** Fetches a file's content (`GET /catalog/file`) unless it is already loaded. */
  async function loadFile(path: string): Promise<void> {
    if (fileContents.value[path]) return;
    const version = filesVersion.value;
    try {
      const { data, error, response } = await api.GET('/catalog/file', {
        params: { query: { path } },
      });
      // A load since this request started dropped the old snapshot's files: keep none of it.
      if (version !== filesVersion.value) return;
      if (data) {
        fileContents.value = { ...fileContents.value, [path]: data };
        const rest = { ...fileErrors.value };
        delete rest[path];
        fileErrors.value = rest;
      } else {
        fileErrors.value = {
          ...fileErrors.value,
          [path]: describeProblem(error, response),
        };
      }
    } catch (e) {
      if (version !== filesVersion.value) return;
      fileErrors.value = { ...fileErrors.value, [path]: message(e) };
    }
  }

  /** Loads the summary, its objects, the providers and the file list. A quiet load keeps "Loading…" off. */
  async function load({ quiet = false }: { quiet?: boolean } = {}) {
    if (!quiet) loading.value = true;
    try {
      await Promise.all([loadSummary(), loadProviders(), loadFiles()]);
    } finally {
      if (!quiet) loading.value = false;
    }
  }

  /** Evaluates an event against the loaded catalog (`POST /catalog/dry-run`); nothing is stored. */
  async function dryRun(
    request: Schemas['DryRunRequest'],
  ): Promise<DryRunOutcome> {
    try {
      const { data, error, response } = await api.POST('/catalog/dry-run', {
        body: request,
      });
      if (data) return { ok: true, evaluation: data };
      const problem = (error ?? {}) as Partial<Schemas['Problem']>;
      return {
        ok: false,
        message: describeProblem(error, response),
        fields: Array.isArray(problem.errors) ? problem.errors : [],
      };
    } catch (e) {
      return { ok: false, message: message(e), fields: [] };
    }
  }

  /** The first page of work items, whose recorded events the dry-run can replay. */
  async function recentWorkItems(): Promise<
    { items: Schemas['WorkItem'][] } | { error: string }
  > {
    try {
      const { data, error, response } = await api.GET('/workitems');
      return data
        ? { items: data.items }
        : { error: describeProblem(error, response) };
    } catch (e) {
      return { error: message(e) };
    }
  }

  /** A work item's recorded events (`GET /workitems/{id}`), newest first. */
  async function recordedEvents(
    workItem: string,
  ): Promise<{ events: Schemas['Event'][] } | { error: string }> {
    try {
      const { data, error, response } = await api.GET('/workitems/{id}', {
        params: { path: { id: workItem } },
      });
      if (!data) return { error: describeProblem(error, response) };
      const events = [...data.events].sort(
        (a, b) => Date.parse(b.time) - Date.parse(a.time),
      );
      return { events };
    } catch (e) {
      return { error: message(e) };
    }
  }

  return {
    summary,
    summaryError,
    providers,
    providersLoaded,
    providersError,
    objects,
    objectErrors,
    loading,
    ruleSets,
    rules,
    taskSpecs,
    queues,
    pools,
    groups,
    templates,
    files,
    filesError,
    fileContents,
    fileErrors,
    filesVersion,
    namesOf,
    load,
    loadFile,
    dryRun,
    recentWorkItems,
    recordedEvents,
  };
});
