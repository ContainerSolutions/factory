<script setup lang="ts">
import { computed, ref, useId } from 'vue';
import CodeBlock from './CodeBlock.vue';
import { yamlTokens } from './highlight';

/** A "YAML" toggle that shows an object as highlighted YAML, as the API returns it. */
const props = defineProps<{ value: unknown; label: string }>();

const open = ref(false);
const id = useId();
const tokens = computed(() => (open.value ? yamlTokens(props.value) : []));
</script>

<template>
  <div class="mt-3">
    <button
      type="button"
      class="text-xs font-medium text-ccf-600 underline dark:text-ccf-400"
      :aria-expanded="open"
      :aria-controls="id"
      data-testid="yaml-toggle"
      @click="open = !open"
    >
      {{ open ? 'Hide YAML' : 'Show YAML' }}
    </button>
    <div v-if="open" :id="id" class="mt-2">
      <CodeBlock :tokens="tokens" :label="label" />
    </div>
  </div>
</template>
