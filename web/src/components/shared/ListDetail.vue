<script setup lang="ts">
import type { ListItem } from './list';

/**
 * A list of names to pick from, beside the picked one's details (the default slot). Each name is
 * a button, the picked one `aria-current`; a short secondary line says what it is. On narrow
 * widths the list stacks above the details.
 */
defineProps<{
  items: readonly ListItem[];
  selected: string;
  /** The list's accessible name. */
  label: string;
}>();
const emit = defineEmits<{ select: [id: string] }>();
</script>

<template>
  <div
    class="grid gap-4 lg:grid-cols-[fit-content(20rem)_minmax(0,1fr)]"
    data-testid="list-detail"
  >
    <nav :aria-label="label" class="lg:min-w-48">
      <ul class="flex flex-col gap-1 text-sm">
        <li v-for="i in items" :key="i.id">
          <button
            type="button"
            class="w-full rounded px-2 py-1 text-left [overflow-wrap:anywhere] hover:bg-ccf-100 dark:hover:bg-ccf-800"
            :class="i.id === selected ? 'bg-ccf-100 dark:bg-ccf-800' : ''"
            :aria-current="i.id === selected ? 'true' : undefined"
            data-testid="list-item"
            @click="emit('select', i.id)"
          >
            <span
              class="block font-mono text-xs"
              :class="i.id === selected ? 'font-semibold' : ''"
              >{{ i.label }}</span
            >
            <span
              v-if="i.secondary"
              class="block text-xs text-ccf-600 dark:text-ccf-400"
              data-testid="list-item-secondary"
              >{{ i.secondary }}</span
            >
          </button>
        </li>
      </ul>
    </nav>
    <div v-if="selected" data-testid="list-detail-pane">
      <slot />
    </div>
  </div>
</template>
