<script setup lang="ts">
import { computed } from 'vue';
import type { Schemas } from '@/api/client';
import StatusPill from '@/components/shared/StatusPill.vue';
import { formatTime } from '@/components/shared/format';
import { ago, budgetText, formatCount, reasonText } from './pools';

const props = defineProps<{ view: Schemas['QueueView']; now: Date }>();

const spec = computed(() => props.view.queue.spec);
const name = computed(() => props.view.queue.metadata.name);
const headingId = computed(() => `queue-${name.value}`);
const budget = computed(() => budgetText(spec.value.budget));
const running = computed(() =>
  typeof spec.value.concurrency === 'number'
    ? `${formatCount(props.view.running)} of ${formatCount(spec.value.concurrency)}`
    : formatCount(props.view.running),
);
const retry = computed(() => {
  const r = spec.value.retry;
  if (!r) return '';
  const on = r.on?.length ? ` on ${r.on.join(', ')}` : '';
  return `${r.max} ${r.max === 1 ? 'time' : 'times'}${on}`;
});
</script>

<template>
  <section
    :aria-labelledby="headingId"
    class="rounded-lg border border-ccf-200 p-4 dark:border-ccf-800"
    data-testid="queue"
  >
    <div class="mb-2 flex flex-wrap items-center gap-2">
      <h3 :id="headingId" class="font-mono font-semibold">{{ name }}</h3>
      <span
        class="rounded-full bg-ccf-200 px-2 text-xs font-medium dark:bg-ccf-800"
        data-testid="queue-depth"
        >{{ formatCount(view.depth) }} pending</span
      >
      <div class="ms-auto"><slot name="actions" /></div>
    </div>
    <dl class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
      <dt class="text-ccf-600 dark:text-ccf-400">Pool</dt>
      <dd class="font-mono text-xs">{{ spec.pool }}</dd>
      <dt class="text-ccf-600 dark:text-ccf-400">Running</dt>
      <dd data-testid="queue-running">{{ running }}</dd>
      <template v-if="spec.priority !== undefined">
        <dt class="text-ccf-600 dark:text-ccf-400">Priority</dt>
        <dd>{{ spec.priority }}</dd>
      </template>
      <template v-if="budget">
        <dt class="text-ccf-600 dark:text-ccf-400">Budget</dt>
        <dd>{{ budget }}</dd>
      </template>
      <template v-if="retry">
        <dt class="text-ccf-600 dark:text-ccf-400">Retry</dt>
        <dd>{{ retry }}</dd>
      </template>
    </dl>

    <h4 class="mt-3 mb-1 text-sm font-semibold">Pending</h4>
    <ul v-if="view.pending.length" class="flex flex-col gap-2" role="list">
      <li
        v-for="p in view.pending"
        :key="p.task"
        class="text-sm"
        data-testid="pending-task"
      >
        <div class="flex flex-wrap items-center gap-2">
          <code class="text-xs break-all">{{ p.task }}</code>
          <StatusPill
            :label="p.reason ?? 'no reason yet'"
            :tone="p.reason ? 'warn' : 'neutral'"
            data-testid="pending-reason"
          />
          <time
            v-if="p.since"
            :datetime="p.since"
            :title="formatTime(p.since)"
            class="text-xs text-ccf-600 dark:text-ccf-400"
            >waiting {{ ago(p.since, now) }}</time
          >
        </div>
        <p
          v-if="reasonText(p.reason)"
          class="mt-0.5 text-xs text-ccf-600 dark:text-ccf-400"
          data-testid="pending-why"
        >
          {{ reasonText(p.reason) }}
        </p>
      </li>
    </ul>
    <p v-else class="text-sm text-ccf-500 dark:text-ccf-400">
      Nothing pending.
    </p>
  </section>
</template>
