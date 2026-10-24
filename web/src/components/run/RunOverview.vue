<script setup lang="ts">
import { computed } from 'vue';
import type { Schemas } from '@/api/client';
import StatusPill from '@/components/shared/StatusPill.vue';
import { formatTime } from '@/components/shared/format';
import { runStateOf } from './run';

const props = defineProps<{ run: Schemas['Run'] }>();

const state = computed(() => runStateOf(props.run));
const result = computed(() => props.run.status?.result);
</script>

<template>
  <section aria-labelledby="overview-heading">
    <h2 id="overview-heading" class="mb-2 text-sm font-semibold">Run</h2>
    <dl class="kv-grid gap-x-4 gap-y-1 text-sm">
      <dt class="text-ccf-600 dark:text-ccf-400">State</dt>
      <dd data-testid="run-state"><StatusPill :label="state" /></dd>
      <dt class="text-ccf-600 dark:text-ccf-400">Agent</dt>
      <dd class="font-mono text-xs break-all" data-testid="run-agent">
        {{ run.spec.agent }}
      </dd>
      <dt class="text-ccf-600 dark:text-ccf-400">Owner</dt>
      <dd class="break-all" data-testid="run-owner">
        {{ run.spec.owner ?? 'not recorded' }}
      </dd>
      <dt class="text-ccf-600 dark:text-ccf-400">Task</dt>
      <dd class="font-mono text-xs break-all">{{ run.spec.task }}</dd>
      <dt class="text-ccf-600 dark:text-ccf-400">Attempt</dt>
      <dd>{{ run.spec.attempt }}</dd>
      <template v-if="run.status?.startedAt">
        <dt class="text-ccf-600 dark:text-ccf-400">Started</dt>
        <dd>
          <time :datetime="run.status.startedAt">{{
            formatTime(run.status.startedAt)
          }}</time>
        </dd>
      </template>
      <template v-if="run.status?.endedAt">
        <dt class="text-ccf-600 dark:text-ccf-400">Ended</dt>
        <dd>
          <time :datetime="run.status.endedAt">{{
            formatTime(run.status.endedAt)
          }}</time>
        </dd>
      </template>
    </dl>

    <template v-if="result">
      <h2 class="mt-4 mb-2 text-sm font-semibold">Result</h2>
      <div class="flex flex-wrap items-center gap-2 text-sm">
        <StatusPill :label="result.status" />
        <StatusPill
          v-if="result.outcome"
          :label="`outcome: ${result.outcome}`"
          tone="neutral"
        />
      </div>
      <!-- Written by the harness: text only. -->
      <p
        v-if="result.summary"
        class="mt-2 text-sm text-ccf-700 dark:text-ccf-300"
        data-testid="result-summary"
      >
        {{ result.summary }}
      </p>
      <p
        v-if="result.question"
        class="mt-2 text-sm"
        data-testid="result-question"
      >
        <span class="font-medium">Question:</span> {{ result.question }}
      </p>
    </template>
  </section>
</template>
