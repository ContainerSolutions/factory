<script setup lang="ts">
import { computed } from 'vue';

// Rendered as text, never HTML: the values can come from provider events.
const props = defineProps<{ value: unknown; label: string }>();

const text = computed(() => JSON.stringify(props.value ?? null, null, 2));
</script>

<template>
  <pre
    role="region"
    :aria-label="label"
    tabindex="0"
    class="max-w-full overflow-x-auto rounded-md border border-ccf-200 bg-ccf-100 p-3 font-mono text-xs text-ccf-800 dark:border-ccf-700 dark:bg-ccf-900 dark:text-ccf-200"
    >{{ text }}</pre
  >
</template>
