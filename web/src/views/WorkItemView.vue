<script setup lang="ts">
import { computed, ref, watch } from 'vue';
import { RouterLink, useRoute, useRouter } from 'vue-router';
import PauseControls from '@/components/controls/PauseControls.vue';
import WorkItemAdmin from '@/components/controls/WorkItemAdmin.vue';
import DecisionWhy from '@/components/shared/DecisionWhy.vue';
import PageHeader from '@/components/shared/PageHeader.vue';
import TabList from '@/components/shared/TabList.vue';
import { pickTab, tabPanelAttrs, type Tab } from '@/components/shared/tabs';
import { titleOf } from '@/components/workitem/board';
import { catalogSHAFor } from '@/components/workitem/timeline';
import LLMTasks from '@/components/workitem/LLMTasks.vue';
import WorkItemGraph from '@/components/workitem/WorkItemGraph.vue';
import WorkItemHeader from '@/components/workitem/WorkItemHeader.vue';
import WorkItemTimeline from '@/components/workitem/WorkItemTimeline.vue';
import { useWorkItemsStore } from '@/stores/workitems';
import Message from '@/volt/Message.vue';

const props = defineProps<{ id: string }>();

const store = useWorkItemsStore();
const route = useRoute();
const router = useRouter();

const tabs = computed<Tab[]>(() => [
  { id: 'graph', label: 'Graph' },
  {
    id: 'tasks',
    label: 'LLM tasks',
    count: detail.value?.tasks.length ?? 0,
  },
  { id: 'timeline', label: 'Timeline' },
]);
/** The tab lives in the URL (?tab=timeline). */
const tab = computed(() => pickTab(tabs.value, route.query.tab));
/** The timeline row a graph event opened, and the task a graph task opened. */
const focus = ref<string | undefined>();
const focusTask = ref<string | undefined>();

function selectTab(id: string): void {
  router.replace({ query: { ...route.query, tab: id } });
}

function showEvent(key: string): void {
  focus.value = key;
  selectTab('timeline');
}

/** A replay recorded decisions: reload the work item, keeping the page as it is meanwhile. */
function replayed(): void {
  store.loadItem(props.id, { quiet: true });
}

/** The work item is gone: back to the board. */
function deleted(): void {
  router.push({ name: 'board' });
}

function showTask(id: string): void {
  focusTask.value = id;
  selectTab('tasks');
}

const whyIndex = ref<number | null>(null);
const whyVisible = computed({
  get: () => whyIndex.value !== null,
  set: (v) => {
    if (!v) whyIndex.value = null;
  },
});

watch(
  () => props.id,
  (id) => {
    whyIndex.value = null;
    focus.value = undefined;
    focusTask.value = undefined;
    store.loadItem(id);
  },
  { immediate: true },
);

const detail = computed(() => store.current);
const decision = computed(() =>
  whyIndex.value === null
    ? null
    : (detail.value?.workItem.status?.decisions?.[whyIndex.value] ?? null),
);
const catalogSHA = computed(() =>
  decision.value && detail.value
    ? catalogSHAFor(decision.value, detail.value.tasks)
    : undefined,
);
</script>

<template>
  <nav aria-label="Breadcrumb" class="mb-2 text-sm">
    <RouterLink
      :to="{ name: 'board' }"
      class="text-ccf-600 hover:underline dark:text-ccf-400"
      >← Board</RouterLink
    >
  </nav>

  <template v-if="detail">
    <PageHeader :title="titleOf(detail.workItem)">
      Work item <code class="text-xs">{{ detail.workItem.metadata.name }}</code>
    </PageHeader>
    <div class="-mt-3 mb-6 flex flex-wrap items-center gap-4">
      <PauseControls scope="workitem" :name="detail.workItem.metadata.name" />
      <WorkItemAdmin :item="detail.workItem" @deleted="deleted" />
    </div>
    <WorkItemHeader :item="detail.workItem" />
    <TabList
      :tabs="tabs"
      :model-value="tab"
      label="Work item views"
      id-prefix="workitem"
      @update:model-value="selectTab"
    />
    <div v-bind="tabPanelAttrs('workitem', tab)" class="mb-8">
      <WorkItemGraph
        v-if="tab === 'graph'"
        :detail="detail"
        @why="whyIndex = $event"
        @event="showEvent"
        @task="showTask"
        @timeline="selectTab('timeline')"
      />
      <LLMTasks
        v-else-if="tab === 'tasks'"
        :detail="detail"
        :focus="focusTask"
      />
      <WorkItemTimeline
        v-else
        :detail="detail"
        :focus="focus"
        @why="whyIndex = $event"
        @replayed="replayed"
      />
    </div>
    <DecisionWhy
      v-model:visible="whyVisible"
      :decision="decision"
      :catalog-sha="catalogSHA"
    />
  </template>

  <template v-else-if="store.currentNotFound">
    <PageHeader title="Work item not found">
      There is no work item <code class="text-xs">{{ id }}</code
      >.
    </PageHeader>
  </template>

  <template v-else-if="store.currentError">
    <PageHeader title="Work item" />
    <Message severity="error" role="alert">
      Couldn't load the work item: {{ store.currentError }}
    </Message>
  </template>

  <template v-else>
    <PageHeader title="Work item" />
    <p class="text-sm text-ccf-600 dark:text-ccf-400" role="status">Loading…</p>
  </template>
</template>
