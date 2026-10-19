<script setup lang="ts">
import { computed } from 'vue';
import Select from '@/volt/Select.vue';
import { NO_FILTERS, type BoardFilters } from './board';

const props = defineProps<{
  /** The values each filter can take. */
  facets: Record<keyof BoardFilters, string[]>;
}>();

const filters = defineModel<BoardFilters>({ required: true });

const fields: { key: keyof BoardFilters; label: string; all: string }[] = [
  { key: 'ruleSet', label: 'RuleSet', all: 'All RuleSets' },
  { key: 'provider', label: 'Provider', all: 'All providers' },
  { key: 'repo', label: 'Repository', all: 'All repositories' },
  { key: 'taskSpec', label: 'TaskSpec', all: 'All TaskSpecs' },
];

// An explicit "all" option rather than a clear icon, so clearing works from the keyboard. A set
// filter whose value is no longer a facet stays selectable.
const options = computed(
  () =>
    Object.fromEntries(
      fields.map((f) => {
        const values = new Set(props.facets[f.key]);
        if (filters.value[f.key]) values.add(filters.value[f.key]);
        return [
          f.key,
          [
            { label: f.all, value: '' },
            ...[...values].sort().map((v) => ({ label: v, value: v })),
          ],
        ];
      }),
    ) as Record<keyof BoardFilters, { label: string; value: string }[]>,
);

function set(key: keyof BoardFilters, value: unknown): void {
  filters.value = {
    ...filters.value,
    [key]: typeof value === 'string' ? value : '',
  };
}

const active = computed(() => fields.some((f) => filters.value[f.key]));

function clear(): void {
  filters.value = { ...NO_FILTERS };
}
</script>

<template>
  <form
    role="search"
    aria-label="Filter work items"
    class="mb-6 flex flex-wrap items-end gap-4"
    @submit.prevent
  >
    <div v-for="f in fields" :key="f.key" class="flex flex-col gap-1">
      <span
        :id="`filter-${f.key}`"
        class="text-xs font-medium text-ccf-600 dark:text-ccf-400"
        >{{ f.label }}</span
      >
      <Select
        :model-value="filters[f.key]"
        :options="options[f.key]"
        option-label="label"
        option-value="value"
        :placeholder="f.all"
        :aria-labelledby="`filter-${f.key}`"
        :data-testid="`filter-${f.key}`"
        class="min-w-48"
        @update:model-value="set(f.key, $event)"
      />
    </div>
    <button
      v-if="active"
      type="button"
      class="rounded px-2 py-2 text-sm text-ccf-600 underline hover:text-ccf-900 dark:text-ccf-400 dark:hover:text-ccf-100"
      @click="clear"
    >
      Clear filters
    </button>
  </form>
</template>
