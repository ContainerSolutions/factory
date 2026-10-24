<script setup lang="ts">
import { computed } from 'vue';
import type { Schemas } from '@/api/client';
import { formatCost, formatCount } from './run';

const props = defineProps<{ run: Schemas['Run'] }>();

// The harness that ran and its effective config come with the Result only.
const harness = computed(() => props.run.status?.result?.harness);
const harnessName = computed(() =>
  [harness.value?.name, harness.value?.version].filter(Boolean).join(' '),
);
// The run's usage, else the one its Result reported.
const usage = computed(() => props.run.status?.usage ?? harness.value?.usage);
</script>

<template>
  <section aria-labelledby="harness-heading">
    <h2 id="harness-heading" class="mb-2 text-sm font-semibold">Harness</h2>
    <dl
      v-if="harness"
      class="kv-grid gap-x-4 gap-y-1 text-sm"
      data-testid="harness"
    >
      <dt class="text-ccf-600 dark:text-ccf-400">Name</dt>
      <dd>{{ harnessName }}</dd>
      <template v-if="harness.auth">
        <dt class="text-ccf-600 dark:text-ccf-400">Auth</dt>
        <dd>{{ harness.auth }}</dd>
      </template>
      <dt class="text-ccf-600 dark:text-ccf-400">Model</dt>
      <dd class="font-mono text-xs">{{ harness.effectiveConfig.model }}</dd>
      <template v-if="harness.effectiveConfig.reasoningEffort">
        <dt class="text-ccf-600 dark:text-ccf-400">Reasoning effort</dt>
        <dd>{{ harness.effectiveConfig.reasoningEffort }}</dd>
      </template>
    </dl>
    <p v-else class="text-sm text-ccf-500" data-testid="harness">
      The effective config is reported with the Result.
    </p>

    <h2 class="mt-4 mb-2 text-sm font-semibold">Usage</h2>
    <dl
      v-if="usage"
      class="kv-grid gap-x-4 gap-y-1 text-sm"
      data-testid="usage"
    >
      <dt class="text-ccf-600 dark:text-ccf-400">Input tokens</dt>
      <dd>{{ formatCount(usage.inputTokens) }}</dd>
      <dt class="text-ccf-600 dark:text-ccf-400">Output tokens</dt>
      <dd>{{ formatCount(usage.outputTokens) }}</dd>
      <template v-if="usage.turns !== undefined">
        <dt class="text-ccf-600 dark:text-ccf-400">Turns</dt>
        <dd>{{ formatCount(usage.turns) }}</dd>
      </template>
      <template v-if="usage.wallClock">
        <dt class="text-ccf-600 dark:text-ccf-400">Wall clock</dt>
        <dd>{{ usage.wallClock }}</dd>
      </template>
      <dt class="text-ccf-600 dark:text-ccf-400">Cost</dt>
      <dd>{{ formatCost(usage.costUSD) }}</dd>
    </dl>
    <p v-else class="text-sm text-ccf-500" data-testid="usage">
      No usage reported yet.
    </p>
  </section>
</template>
