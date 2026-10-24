<script setup lang="ts">
import { computed } from 'vue';
import type { Schemas } from '@/api/client';
import StatusPill from '@/components/shared/StatusPill.vue';
import { formatTime, type Tone } from '@/components/shared/format';
import AgentHarnesses from './AgentHarnesses.vue';
import {
  capacityOf,
  formatCount,
  heartbeatOf,
  ofCap,
  throttleOf,
  type HeartbeatState,
} from './pools';

// Everything here is what the agent and its owner reported: rendered as text only. The
// contract's Agent carries no join token, agent token or lease credential, and none is shown.
const props = defineProps<{
  agent: Schemas['Agent'];
  pool?: Schemas['Pool'];
  now: Date;
}>();

const name = computed(() => props.agent.metadata.name);
const headingId = computed(() => `agent-${name.value}`);
const status = computed(() => props.agent.status);
const lending = computed(() => props.agent.spec.lending);
const labels = computed(() =>
  Object.entries(props.agent.metadata.labels ?? {}).map(
    ([k, v]) => `${k}=${v}`,
  ),
);
const capabilityLabels = computed(
  () => props.agent.spec.capabilities.labels ?? [],
);

const HEARTBEAT_TONES: Record<HeartbeatState, Tone> = {
  'on time': 'success',
  stale: 'danger',
  never: 'neutral',
};
const heartbeat = computed(() =>
  heartbeatOf(props.agent, props.pool, props.now),
);
const throttle = computed(() => throttleOf(props.agent, props.now));
const running = computed(() => status.value?.running ?? []);
const capacity = computed(() => capacityOf(props.agent, props.pool));

const lendingRows = computed(() => {
  const l = lending.value;
  if (!l || l.unlimited) return [];
  const list = (v: string[] | undefined) => (v?.length ? v.join(', ') : '');
  return [
    ['Queues', list(l.queues) || 'none'],
    ['RuleSets', list(l.ruleSets) || 'any'],
    ['Harnesses', list(l.harnesses) || 'any'],
    ['Max concurrent', l.maxConcurrent?.toString() ?? ''],
    [
      'Runs a day',
      l.maxRunsPerDay === undefined ? '' : formatCount(l.maxRunsPerDay),
    ],
    [
      'Tokens a day',
      l.maxTokensPerDay === undefined ? '' : formatCount(l.maxTokensPerDay),
    ],
    ['Hours', l.hours ?? 'any time'],
  ].filter(([, v]) => v !== '');
});
// Daily caps apply only to limited lending.
const caps = computed(() =>
  lending.value?.unlimited ? undefined : lending.value,
);
</script>

<template>
  <article
    :aria-labelledby="headingId"
    class="rounded-lg border border-ccf-200 p-4 dark:border-ccf-800"
    data-testid="agent"
  >
    <div class="mb-2 flex flex-wrap items-center gap-2">
      <h4 :id="headingId" class="font-mono text-sm font-semibold break-all">
        {{ name }}
      </h4>
      <StatusPill
        v-if="status"
        :label="status.phase"
        data-testid="agent-phase"
      />
      <StatusPill
        v-else
        label="no status"
        tone="neutral"
        data-testid="agent-phase"
      />
    </div>

    <dl class="kv-grid gap-x-4 gap-y-1 text-sm">
      <dt class="text-ccf-600 dark:text-ccf-400">Owner</dt>
      <dd class="break-all" data-testid="agent-owner">
        {{ agent.spec.owner }}
      </dd>
      <template v-if="labels.length">
        <dt class="text-ccf-600 dark:text-ccf-400">Labels</dt>
        <dd class="flex flex-wrap gap-1" data-testid="agent-labels">
          <StatusPill
            v-for="label in labels"
            :key="label"
            :label="label"
            tone="neutral"
          />
        </dd>
      </template>
      <template v-if="capabilityLabels.length">
        <dt class="text-ccf-600 dark:text-ccf-400">Capabilities</dt>
        <dd class="flex flex-wrap gap-1" data-testid="agent-capabilities">
          <StatusPill
            v-for="label in capabilityLabels"
            :key="label"
            :label="label"
            tone="neutral"
          />
        </dd>
      </template>
      <dt class="text-ccf-600 dark:text-ccf-400">Heartbeat</dt>
      <dd
        class="flex flex-wrap items-center gap-2"
        data-testid="agent-heartbeat"
      >
        <StatusPill
          :label="heartbeat.state"
          :tone="HEARTBEAT_TONES[heartbeat.state]"
        />
        <time
          v-if="status?.lastHeartbeat"
          :datetime="status.lastHeartbeat"
          :title="formatTime(status.lastHeartbeat)"
          class="text-xs text-ccf-600 dark:text-ccf-400"
          >{{ heartbeat.ago }} ago</time
        >
      </dd>
      <template v-if="throttle.state !== 'none'">
        <dt class="text-ccf-600 dark:text-ccf-400">Throttled</dt>
        <dd data-testid="agent-throttle">
          until
          <time :datetime="throttle.until">{{
            formatTime(throttle.until)
          }}</time>
          <span
            v-if="throttle.state === 'throttled'"
            class="text-xs text-ccf-600 dark:text-ccf-400"
          >
            (in {{ throttle.in }})</span
          >
          <span v-else class="text-xs text-ccf-600 dark:text-ccf-400">
            (passed)</span
          >
        </dd>
      </template>
      <dt class="text-ccf-600 dark:text-ccf-400">Leases</dt>
      <dd data-testid="agent-running">
        {{ running.length }} of {{ capacity }}
        <ul v-if="running.length" role="list">
          <li v-for="task in running" :key="task">
            <code class="text-xs break-all">{{ task }}</code>
          </li>
        </ul>
      </dd>
      <dt class="text-ccf-600 dark:text-ccf-400">Today</dt>
      <dd data-testid="agent-today">
        {{ ofCap(status?.today?.runs, caps?.maxRunsPerDay) }} runs ·
        {{ ofCap(status?.today?.tokens, caps?.maxTokensPerDay) }} tokens
      </dd>
    </dl>

    <h5 class="mt-3 mb-1 text-sm font-semibold">Lending</h5>
    <div data-testid="agent-lending" class="text-sm">
      <p v-if="!lending">Lends nothing: no lending is set.</p>
      <p v-else-if="lending.unlimited">
        Unlimited: any queue of the pool, any RuleSet and harness, at any time.
      </p>
      <dl v-else class="kv-grid gap-x-4 gap-y-1">
        <template v-for="[label, value] in lendingRows" :key="label">
          <dt class="text-ccf-600 dark:text-ccf-400">{{ label }}</dt>
          <dd>{{ value }}</dd>
        </template>
      </dl>
    </div>

    <h5 class="mt-3 mb-1 text-sm font-semibold">Harnesses</h5>
    <AgentHarnesses :offers="agent.spec.capabilities.harnesses ?? []" />
  </article>
</template>
