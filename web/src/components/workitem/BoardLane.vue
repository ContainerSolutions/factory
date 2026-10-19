<script setup lang="ts">
import type { Lane } from './board';
import WorkItemCard from './WorkItemCard.vue';

const props = defineProps<{ lane: Lane }>();

const headingId = `lane-${props.lane.id}`;
</script>

<template>
  <section
    :aria-labelledby="headingId"
    class="rounded-lg border border-ccf-200 bg-ccf-100/50 p-3 dark:border-ccf-800 dark:bg-ccf-900/50"
    :data-testid="`lane-${lane.id}`"
  >
    <h2
      :id="headingId"
      class="mb-3 flex items-center gap-2 text-sm font-semibold"
    >
      {{ lane.title }}
      <span
        class="rounded-full bg-ccf-200 px-2 text-xs font-medium dark:bg-ccf-800"
        data-testid="lane-count"
        >{{ lane.items.length }}</span
      >
    </h2>
    <ul v-if="lane.items.length" class="flex flex-col gap-2" role="list">
      <li v-for="item in lane.items" :key="item.metadata.name">
        <WorkItemCard :item="item" />
      </li>
    </ul>
    <p v-else class="text-sm text-ccf-500 dark:text-ccf-400">No work items.</p>
    <p
      v-if="lane.hidden"
      class="mt-2 text-xs text-ccf-500 dark:text-ccf-400"
      data-testid="lane-hidden"
    >
      {{ lane.hidden }} older {{ lane.hidden === 1 ? 'item' : 'items' }} not
      shown.
    </p>
  </section>
</template>
