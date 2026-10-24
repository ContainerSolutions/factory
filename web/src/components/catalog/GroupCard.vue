<script setup lang="ts">
import { computed } from 'vue';
import type { Schemas } from '@/api/client';
import ObjectYaml from '@/components/shared/ObjectYaml.vue';
import { memberText } from './catalog';

/** One Group: its member sources and how long resolved members are cached. */
const props = defineProps<{ group: Schemas['Group'] }>();

const name = computed(() => props.group.metadata.name);
const headingId = computed(() => `group-${name.value}`);
</script>

<template>
  <section
    :aria-labelledby="headingId"
    class="rounded-lg border border-ccf-200 p-4 dark:border-ccf-800"
    data-testid="group"
  >
    <h3 :id="headingId" class="mb-2 font-mono font-semibold">{{ name }}</h3>
    <ul class="list-disc ps-5 text-sm" data-testid="group-members">
      <li v-for="(m, i) in group.spec.members" :key="i">{{ memberText(m) }}</li>
    </ul>
    <p
      v-if="group.spec.cache"
      class="mt-1 text-xs text-ccf-600 dark:text-ccf-400"
    >
      cached for {{ group.spec.cache.ttl }}
    </p>
    <ObjectYaml :value="group" :label="`YAML of Group ${name}`" />
  </section>
</template>
