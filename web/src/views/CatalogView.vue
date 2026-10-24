<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue';
import { useRoute, useRouter } from 'vue-router';
import CatalogFiles from '@/components/catalog/CatalogFiles.vue';
import CatalogStatus from '@/components/catalog/CatalogStatus.vue';
import DryRunPanel from '@/components/catalog/DryRunPanel.vue';
import GroupCard from '@/components/catalog/GroupCard.vue';
import ProviderCard from '@/components/catalog/ProviderCard.vue';
import QueuePoolCard from '@/components/catalog/QueuePoolCard.vue';
import RuleCard from '@/components/catalog/RuleCard.vue';
import RuleSetCard from '@/components/catalog/RuleSetCard.vue';
import TaskSpecCard from '@/components/catalog/TaskSpecCard.vue';
import TemplateCard from '@/components/catalog/TemplateCard.vue';
import {
  catalogLists,
  fileUsers,
  queuePoolId,
  type CatalogLists,
} from '@/components/catalog/catalog';
import ListDetail from '@/components/shared/ListDetail.vue';
import { pickItem } from '@/components/shared/list';
import PageHeader from '@/components/shared/PageHeader.vue';
import TabList from '@/components/shared/TabList.vue';
import { pickTab, tabPanelAttrs, type Tab } from '@/components/shared/tabs';
import { useCatalogStore } from '@/stores/catalog';
import Message from '@/volt/Message.vue';
import SecondaryButton from '@/volt/SecondaryButton.vue';

const store = useCatalogStore();
const route = useRoute();
const router = useRouter();

const providerObjects = computed(() => store.providers.map((v) => v.provider));
const resyncRequested = ref(false);

const prompts = computed(() => store.files.filter((f) => f.kind === 'prompt'));
const schemas = computed(() => store.files.filter((f) => f.kind === 'schema'));
const usedBy = computed(() => fileUsers(store.taskSpecs));

const tabs = computed<Tab[]>(() => [
  { id: 'taskspecs', label: 'Task Specs', count: store.taskSpecs.length },
  { id: 'prompts', label: 'Prompts', count: prompts.value.length },
  { id: 'rules', label: 'Rules', count: store.rules.length },
  { id: 'rulesets', label: 'RuleSets', count: store.ruleSets.length },
  { id: 'templates', label: 'Templates', count: store.templates.length },
  { id: 'providers', label: 'Providers', count: store.providers.length },
  {
    id: 'queues',
    label: 'Queues and Pools',
    count: store.queues.length + store.pools.length,
  },
  { id: 'groups', label: 'Groups', count: store.groups.length },
  { id: 'schemas', label: 'Result Schemas', count: schemas.value.length },
]);

/**
 * The tab and the picked object live in the URL (?tab=rules&item=…, ?tab=prompts&file=…), so
 * links can open them.
 */
const tab = computed(() => pickTab(tabs.value, route.query.tab));
const file = computed(() =>
  typeof route.query.file === 'string' ? route.query.file : '',
);

function selectTab(id: string): void {
  router.replace({
    query: { ...route.query, tab: id, file: undefined, item: undefined },
  });
}

/** Each object tab's list, and the entry shown on it: the one in the URL, else the first. */
const lists = computed(() =>
  catalogLists({
    taskSpecs: store.taskSpecs,
    rules: store.rules,
    ruleSets: store.ruleSets,
    templates: store.templates,
    providers: store.providers,
    queues: store.queues,
    pools: store.pools,
    groups: store.groups,
  }),
);
const listed = computed(() =>
  tab.value in lists.value ? lists.value[tab.value as keyof CatalogLists] : [],
);
const item = computed(() => pickItem(listed.value, route.query.item));

function selectItem(id: string): void {
  router.replace({ query: { ...route.query, item: id } });
}

const byName = <T extends { metadata: { name: string } }>(
  objects: readonly T[],
): T | undefined => objects.find((o) => o.metadata.name === item.value);
const picked = computed(() => ({
  taskspecs: byName(store.taskSpecs),
  rules: byName(store.rules),
  rulesets: byName(store.ruleSets),
  templates: byName(store.templates),
  groups: byName(store.groups),
}));
const shownProvider = computed(() =>
  store.providers.find((v) => v.provider.metadata.name === item.value),
);
const shownQueuePool = computed(() =>
  [...store.queues, ...store.pools].find((o) => queuePoolId(o) === item.value),
);

const LIST_LABEL: Record<string, string> = {
  taskspecs: 'TaskSpecs',
  rules: 'Rules',
  rulesets: 'RuleSets',
  templates: 'Templates',
  providers: 'Providers',
  queues: 'Queues and pools',
  groups: 'Groups',
};
const EMPTY: Record<string, string> = {
  taskspecs: 'No TaskSpecs.',
  rules: 'No Rules.',
  rulesets: 'No RuleSets.',
  templates: 'No Templates.',
  providers: 'No providers.',
  queues: 'No queues or pools.',
  groups: 'No Groups.',
};

function selectFile(path: string): void {
  router.replace({ query: { ...route.query, file: path } });
}

/** The file shown on a files tab: the one in the URL when it is of that kind, else the first. */
function shown(files: readonly { path: string }[]): string {
  return files.some((f) => f.path === file.value)
    ? file.value
    : (files[0]?.path ?? '');
}
const promptShown = computed(() => shown(prompts.value));
const schemaShown = computed(() => shown(schemas.value));

// filesVersion: a reload drops the loaded files, so the shown one is fetched again.
watch(
  [tab, promptShown, schemaShown, () => store.filesVersion],
  () => {
    const p =
      tab.value === 'prompts'
        ? promptShown.value
        : tab.value === 'schemas'
          ? schemaShown.value
          : '';
    if (p) store.loadFile(p);
  },
  { immediate: true },
);

onMounted(() => {
  store.load();
});

function resynced(): void {
  resyncRequested.value = true;
}

async function reload(): Promise<void> {
  resyncRequested.value = false;
  await store.load({ quiet: true });
}
</script>

<template>
  <PageHeader title="Catalog">
    The configuration the dispatcher runs, one tab per kind: TaskSpecs and their
    prompts and result schemas, rules and RuleSets, templates, providers, queues
    and pools, and groups.
  </PageHeader>

  <p
    v-if="store.loading"
    class="mb-4 text-sm text-ccf-600 dark:text-ccf-400"
    role="status"
  >
    Loading…
  </p>

  <template v-else>
    <Message
      v-if="store.summaryError"
      severity="error"
      class="mb-4"
      role="alert"
    >
      Couldn't load the catalog: {{ store.summaryError }}
    </Message>
    <Message
      v-if="resyncRequested"
      severity="info"
      class="mb-4"
      role="status"
      data-testid="resync-requested"
    >
      <span class="flex flex-wrap items-center gap-3">
        Resync requested: the dispatcher re-reads the catalog in the background.
        <SecondaryButton size="small" label="Reload" @click="reload" />
      </span>
    </Message>
    <CatalogStatus
      v-if="store.summary"
      :summary="store.summary"
      @resynced="resynced"
    />
    <Message
      v-if="store.objectErrors.length"
      severity="warn"
      class="mb-4"
      role="status"
    >
      Some objects didn't load:
      <ul class="mt-1 list-disc ps-5">
        <li v-for="e in store.objectErrors" :key="e">{{ e }}</li>
      </ul>
    </Message>

    <TabList
      :tabs="tabs"
      :model-value="tab"
      label="Catalog kinds"
      id-prefix="catalog"
      @update:model-value="selectTab"
    />

    <section
      v-if="tab in LIST_LABEL"
      v-bind="tabPanelAttrs('catalog', tab)"
      class="mb-8"
    >
      <Message
        v-if="tab === 'providers' && store.providersError"
        severity="error"
        class="mb-4"
        role="alert"
      >
        Couldn't load the providers: {{ store.providersError }}
      </Message>
      <ListDetail
        v-if="listed.length"
        :items="listed"
        :selected="item"
        :label="LIST_LABEL[tab]!"
        @select="selectItem"
      >
        <TaskSpecCard
          v-if="tab === 'taskspecs' && picked.taskspecs"
          :task-spec="picked.taskspecs"
        />
        <RuleCard
          v-else-if="tab === 'rules' && picked.rules"
          :rule="picked.rules"
        />
        <RuleSetCard
          v-else-if="tab === 'rulesets' && picked.rulesets"
          :rule-set="picked.rulesets"
          :rules="store.rules"
          :providers="providerObjects"
        />
        <TemplateCard
          v-else-if="tab === 'templates' && picked.templates"
          :template="picked.templates"
        />
        <ProviderCard
          v-else-if="tab === 'providers' && shownProvider"
          :view="shownProvider"
        />
        <QueuePoolCard
          v-else-if="tab === 'queues' && shownQueuePool"
          :object="shownQueuePool"
        />
        <GroupCard
          v-else-if="tab === 'groups' && picked.groups"
          :group="picked.groups"
        />
      </ListDetail>
      <p
        v-else-if="tab !== 'providers' || store.providersLoaded"
        class="text-sm text-ccf-500 dark:text-ccf-400"
      >
        {{ EMPTY[tab] }}
      </p>
    </section>

    <section
      v-else-if="tab === 'prompts' || tab === 'schemas'"
      v-bind="tabPanelAttrs('catalog', tab)"
      class="mb-8"
    >
      <Message
        v-if="store.filesError"
        severity="error"
        class="mb-4"
        role="alert"
      >
        Couldn't load the catalog's files: {{ store.filesError }}
      </Message>
      <CatalogFiles
        v-if="(tab === 'prompts' ? prompts : schemas).length"
        :files="tab === 'prompts' ? prompts : schemas"
        :selected="tab === 'prompts' ? promptShown : schemaShown"
        :file="
          store.fileContents[tab === 'prompts' ? promptShown : schemaShown]
        "
        :error="store.fileErrors[tab === 'prompts' ? promptShown : schemaShown]"
        :used-by="usedBy"
        :label="tab === 'prompts' ? 'Prompts' : 'Result schemas'"
        @select="selectFile"
      />
      <p v-else class="text-sm text-ccf-500 dark:text-ccf-400">
        {{ tab === 'prompts' ? 'No prompts.' : 'No result schemas.' }}
      </p>
    </section>

    <DryRunPanel
      class="border-t border-ccf-200 pt-6 dark:border-ccf-800"
      :catalog-sha="store.summary?.sha"
    />
  </template>
</template>
