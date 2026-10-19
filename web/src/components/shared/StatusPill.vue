<script setup lang="ts">
import { computed } from 'vue';
import { toneOf, type Tone } from './format';

// The label is always shown: colour is never the only status signal.
const props = defineProps<{ label: string; tone?: Tone }>();

const classes: Record<Tone, string> = {
  neutral:
    'border-ccf-300 bg-ccf-100 text-ccf-700 dark:border-ccf-700 dark:bg-ccf-800 dark:text-ccf-200',
  info: 'border-sky-300 bg-sky-50 text-sky-800 dark:border-sky-700 dark:bg-sky-950 dark:text-sky-200',
  success:
    'border-green-300 bg-green-50 text-green-800 dark:border-green-700 dark:bg-green-950 dark:text-green-200',
  warn: 'border-amber-300 bg-amber-50 text-amber-800 dark:border-amber-700 dark:bg-amber-950 dark:text-amber-200',
  danger:
    'border-red-300 bg-red-50 text-red-800 dark:border-red-700 dark:bg-red-950 dark:text-red-200',
};

const tone = computed(() => props.tone ?? toneOf(props.label));
</script>

<template>
  <span
    class="inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium whitespace-nowrap"
    :class="classes[tone]"
    :data-tone="tone"
    >{{ label }}</span
  >
</template>
