<script setup lang="ts">
import { computed } from 'vue';
import { RouterLink } from 'vue-router';
import type { Schemas } from '@/api/client';
import ObjectYaml from '@/components/shared/ObjectYaml.vue';
import { budgetText } from '@/components/pools/pools';

/** One queue or pool as configured; the pools view has its live state. */
const props = defineProps<{ object: Schemas['Queue'] | Schemas['Pool'] }>();

const name = computed(() => props.object.metadata.name);
const headingId = computed(() => `${props.object.kind}-${name.value}`);
</script>

<template>
  <section
    :aria-labelledby="headingId"
    class="rounded-lg border border-ccf-200 p-4 dark:border-ccf-800"
    :data-testid="object.kind === 'Queue' ? 'catalog-queue' : 'catalog-pool'"
  >
    <h3 :id="headingId" class="mb-2 font-mono font-semibold">
      <span class="font-sans text-sm font-normal text-ccf-600 dark:text-ccf-400"
        >{{ object.kind }}
      </span>
      {{ name }}
    </h3>
    <dl v-if="object.kind === 'Queue'" class="kv-grid gap-x-4 gap-y-1 text-sm">
      <dt class="text-ccf-600 dark:text-ccf-400">Pool</dt>
      <dd class="font-mono text-xs leading-5">{{ object.spec.pool }}</dd>
      <dt class="text-ccf-600 dark:text-ccf-400">Concurrency</dt>
      <dd>{{ object.spec.concurrency ?? '—' }}</dd>
      <dt class="text-ccf-600 dark:text-ccf-400">Priority</dt>
      <dd>{{ object.spec.priority ?? '—' }}</dd>
      <dt class="text-ccf-600 dark:text-ccf-400">Budget</dt>
      <dd>{{ budgetText(object.spec.budget) || '—' }}</dd>
    </dl>
    <dl v-else class="kv-grid gap-x-4 gap-y-1 text-sm">
      <dt class="text-ccf-600 dark:text-ccf-400">Mode</dt>
      <dd>{{ object.spec.mode }}</dd>
      <dt class="text-ccf-600 dark:text-ccf-400">Scaling</dt>
      <dd>
        {{
          object.spec.scaling
            ? `${object.spec.scaling.min}–${object.spec.scaling.max} agents`
            : '—'
        }}
      </dd>
    </dl>
    <p class="mt-2 text-sm">
      <RouterLink
        :to="{ name: 'pools' }"
        class="text-ccf-600 underline dark:text-ccf-400"
        >Live queues, pools and agents →</RouterLink
      >
    </p>
    <ObjectYaml :value="object" :label="`YAML of ${object.kind} ${name}`" />
  </section>
</template>
