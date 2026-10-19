<script setup lang="ts">
import { computed, ref, watch } from 'vue';
import { RouterLink } from 'vue-router';
import DecisionWhy from '@/components/shared/DecisionWhy.vue';
import PageHeader from '@/components/shared/PageHeader.vue';
import { titleOf } from '@/components/workitem/board';
import { catalogSHAFor } from '@/components/workitem/timeline';
import WorkItemHeader from '@/components/workitem/WorkItemHeader.vue';
import WorkItemTimeline from '@/components/workitem/WorkItemTimeline.vue';
import { useWorkItemsStore } from '@/stores/workitems';
import Message from '@/volt/Message.vue';

const props = defineProps<{ id: string }>();

const store = useWorkItemsStore();

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
    <WorkItemHeader :item="detail.workItem" />
    <WorkItemTimeline :detail="detail" @why="whyIndex = $event" />
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
