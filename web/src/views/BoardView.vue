<script setup lang="ts">
import { computed, onMounted } from 'vue';
import { useRoute, useRouter, type LocationQuery } from 'vue-router';
import PageHeader from '@/components/shared/PageHeader.vue';
import BoardFilters from '@/components/workitem/BoardFilters.vue';
import BoardLane from '@/components/workitem/BoardLane.vue';
import {
  boardLanes,
  facets,
  NO_FILTERS,
  type BoardFilters as Filters,
} from '@/components/workitem/board';
import { useClock } from '@/composables/useClock';
import { useStream } from '@/composables/useStream';
import { useWorkItemsStore } from '@/stores/workitems';
import Message from '@/volt/Message.vue';
import SecondaryButton from '@/volt/SecondaryButton.vue';

const store = useWorkItemsStore();
const route = useRoute();
const router = useRouter();
const now = useClock();

const keys = Object.keys(NO_FILTERS) as (keyof Filters)[];

function fromQuery(query: LocationQuery): Filters {
  const filters = { ...NO_FILTERS };
  for (const key of keys) {
    const v = query[key];
    filters[key] = typeof v === 'string' ? v : '';
  }
  return filters;
}

// The filters live in the URL, so a filtered board can be shared and survives a reload.
const filters = computed<Filters>({
  get: () => fromQuery(route.query),
  set: (value) => {
    const query = Object.fromEntries(
      keys.filter((k) => value[k]).map((k) => [k, value[k]]),
    );
    router.replace({ query });
  },
});

const available = computed(() => facets(store.items));
const lanes = computed(() => boardLanes(store.items, filters.value, now()));
const needsHuman = computed(() => lanes.value[0]!);
const others = computed(() => lanes.value.slice(1));

onMounted(() => {
  store.load();
});

// Live updates; after a re-created stream (which can't resume) the first page is reloaded.
useStream(
  ['workitems'],
  { 'workitem.updated': (item) => store.upsert(item) },
  { onResync: () => store.load() },
);
</script>

<template>
  <PageHeader title="Board">
    Work items by state. Filters apply to the loaded work items.
  </PageHeader>

  <BoardFilters v-model="filters" :facets="available" />

  <Message v-if="store.error" severity="error" class="mb-4" role="alert">
    Couldn't load the work items: {{ store.error }}
  </Message>

  <p
    v-if="store.loading && !store.loaded"
    class="text-sm text-ccf-600 dark:text-ccf-400"
    role="status"
  >
    Loading work items…
  </p>

  <div v-else-if="store.loaded" class="flex flex-col gap-4">
    <BoardLane :lane="needsHuman" />
    <div class="grid gap-4 md:grid-cols-3">
      <BoardLane v-for="lane in others" :key="lane.id" :lane="lane" />
    </div>
    <div v-if="store.nextCursor">
      <SecondaryButton
        label="Load more"
        :loading="store.loading"
        @click="store.loadMore()"
      />
    </div>
  </div>
</template>
