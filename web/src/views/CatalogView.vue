<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue';
import { useRoute, useRouter } from 'vue-router';
import CatalogFiles from '@/components/catalog/CatalogFiles.vue';
import CatalogStatus from '@/components/catalog/CatalogStatus.vue';
import DryRunPanel from '@/components/catalog/DryRunPanel.vue';
import GroupCard from '@/components/catalog/GroupCard.vue';
import ProviderTable from '@/components/catalog/ProviderTable.vue';
import QueuesAndPools from '@/components/catalog/QueuesAndPools.vue';
import RuleCard from '@/components/catalog/RuleCard.vue';
import RuleSetCard from '@/components/catalog/RuleSetCard.vue';
import TaskSpecCard from '@/components/catalog/TaskSpecCard.vue';
import TemplateCard from '@/components/catalog/TemplateCard.vue';
import { fileUsers } from '@/components/catalog/catalog';
import ObjectYaml from '@/components/shared/ObjectYaml.vue';
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

/** The tab and the picked file live in the URL (?tab=prompts&file=…), so links can open them. */
const tab = computed(() => pickTab(tabs.value, route.query.tab));
const file = computed(() =>
  typeof route.query.file === 'string' ? route.query.file : '',
);

function selectTab(id: string): void {
  router.replace({ query: { ...route.query, tab: id, file: undefined } });
}

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

watch(
  [tab, promptShown, schemaShown],
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
      v-if="tab === 'taskspecs'"
      v-bind="tabPanelAttrs('catalog', 'taskspecs')"
      class="mb-8"
    >
      <div v-if="store.taskSpecs.length" class="grid gap-4 xl:grid-cols-2">
        <TaskSpecCard
          v-for="t in store.taskSpecs"
          :key="t.metadata.name"
          :task-spec="t"
        />
      </div>
      <p v-else class="text-sm text-ccf-500 dark:text-ccf-400">No TaskSpecs.</p>
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

    <section
      v-else-if="tab === 'rules'"
      v-bind="tabPanelAttrs('catalog', 'rules')"
      class="mb-8"
    >
      <div v-if="store.rules.length" class="grid gap-4 xl:grid-cols-2">
        <RuleCard v-for="r in store.rules" :key="r.metadata.name" :rule="r" />
      </div>
      <p v-else class="text-sm text-ccf-500 dark:text-ccf-400">No Rules.</p>
    </section>

    <section
      v-else-if="tab === 'rulesets'"
      v-bind="tabPanelAttrs('catalog', 'rulesets')"
      class="mb-8"
    >
      <div v-if="store.ruleSets.length" class="grid gap-4 xl:grid-cols-2">
        <RuleSetCard
          v-for="rs in store.ruleSets"
          :key="rs.metadata.name"
          :rule-set="rs"
          :rules="store.rules"
          :providers="providerObjects"
        />
      </div>
      <p v-else class="text-sm text-ccf-500 dark:text-ccf-400">No RuleSets.</p>
    </section>

    <section
      v-else-if="tab === 'templates'"
      v-bind="tabPanelAttrs('catalog', 'templates')"
      class="mb-8"
    >
      <div v-if="store.templates.length" class="grid gap-4 xl:grid-cols-2">
        <TemplateCard
          v-for="t in store.templates"
          :key="t.metadata.name"
          :template="t"
        />
      </div>
      <p v-else class="text-sm text-ccf-500 dark:text-ccf-400">No Templates.</p>
    </section>

    <section
      v-else-if="tab === 'providers'"
      v-bind="tabPanelAttrs('catalog', 'providers')"
      class="mb-8"
    >
      <Message
        v-if="store.providersError"
        severity="error"
        class="mb-4"
        role="alert"
      >
        Couldn't load the providers: {{ store.providersError }}
      </Message>
      <template v-if="store.providers.length">
        <ProviderTable :providers="store.providers" />
        <ul class="mt-4 flex flex-col gap-2">
          <li v-for="p in providerObjects" :key="p.metadata.name">
            <span class="font-mono text-xs font-semibold">{{
              p.metadata.name
            }}</span>
            <ObjectYaml
              :value="p"
              :label="`YAML of Provider ${p.metadata.name}`"
            />
          </li>
        </ul>
      </template>
      <p
        v-else-if="store.providersLoaded"
        class="text-sm text-ccf-500 dark:text-ccf-400"
      >
        No providers.
      </p>
    </section>

    <section
      v-else-if="tab === 'queues'"
      v-bind="tabPanelAttrs('catalog', 'queues')"
      class="mb-8"
    >
      <QueuesAndPools :queues="store.queues" :pools="store.pools" />
      <ul class="mt-4 flex flex-col gap-2">
        <li
          v-for="o in [...store.queues, ...store.pools]"
          :key="`${o.kind}/${o.metadata.name}`"
        >
          <span class="font-mono text-xs font-semibold"
            >{{ o.kind }} {{ o.metadata.name }}</span
          >
          <ObjectYaml
            :value="o"
            :label="`YAML of ${o.kind} ${o.metadata.name}`"
          />
        </li>
      </ul>
    </section>

    <section
      v-else-if="tab === 'groups'"
      v-bind="tabPanelAttrs('catalog', 'groups')"
      class="mb-8"
    >
      <div v-if="store.groups.length" class="grid gap-4 xl:grid-cols-2">
        <GroupCard
          v-for="g in store.groups"
          :key="g.metadata.name"
          :group="g"
        />
      </div>
      <p v-else class="text-sm text-ccf-500 dark:text-ccf-400">No Groups.</p>
    </section>

    <DryRunPanel
      class="border-t border-ccf-200 pt-6 dark:border-ccf-800"
      :catalog-sha="store.summary?.sha"
    />
  </template>
</template>
