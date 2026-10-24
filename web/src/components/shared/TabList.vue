<script setup lang="ts">
import { tabId, panelId, type Tab } from './tabs';

/**
 * A row of tabs (WAI-ARIA tabs, automatic activation): the selected tab is the only one in the
 * tab order; the arrow keys, Home and End move between tabs and select them. The parent renders
 * the panels, each with tabPanelAttrs(idPrefix, id).
 */
const props = defineProps<{
  tabs: readonly Tab[];
  modelValue: string;
  label: string;
  idPrefix: string;
}>();
const emit = defineEmits<{ 'update:modelValue': [id: string] }>();

const buttons: (HTMLButtonElement | null)[] = [];

function select(i: number): void {
  const tab = props.tabs[i];
  if (!tab) return;
  emit('update:modelValue', tab.id);
  buttons[i]?.focus();
}

function onKey(e: KeyboardEvent, i: number): void {
  const n = props.tabs.length;
  const next =
    e.key === 'ArrowRight'
      ? (i + 1) % n
      : e.key === 'ArrowLeft'
        ? (i - 1 + n) % n
        : e.key === 'Home'
          ? 0
          : e.key === 'End'
            ? n - 1
            : -1;
  if (next < 0) return;
  e.preventDefault();
  select(next);
}
</script>

<template>
  <div
    role="tablist"
    :aria-label="label"
    class="mb-4 flex flex-wrap gap-1 border-b border-ccf-200 dark:border-ccf-800"
  >
    <button
      v-for="(t, i) in tabs"
      :id="tabId(idPrefix, t.id)"
      :key="t.id"
      :ref="(el) => (buttons[i] = el as HTMLButtonElement | null)"
      type="button"
      role="tab"
      :aria-selected="t.id === modelValue"
      :aria-controls="panelId(idPrefix, t.id)"
      :tabindex="t.id === modelValue ? 0 : -1"
      class="-mb-px border-b-2 px-3 py-2 text-sm font-medium focus-visible:outline-2 focus-visible:outline-sky-600"
      :class="
        t.id === modelValue
          ? 'border-ccf-900 text-ccf-900 dark:border-ccf-100 dark:text-ccf-100'
          : 'border-transparent text-ccf-600 hover:text-ccf-900 dark:text-ccf-400 dark:hover:text-ccf-100'
      "
      :data-tab="t.id"
      @click="select(i)"
      @keydown="onKey($event, i)"
    >
      {{ t.label
      }}<span
        v-if="t.count !== undefined"
        class="ms-1.5 rounded-full bg-ccf-100 px-1.5 text-xs text-ccf-700 dark:bg-ccf-800 dark:text-ccf-200"
        >{{ t.count }}</span
      >
    </button>
  </div>
</template>
