<script setup lang="ts">
import { computed, onMounted, onScopeDispose, ref } from 'vue';
import PoolCard from '@/components/pools/PoolCard.vue';
import QueueCard from '@/components/pools/QueueCard.vue';
import { poolGroups, sortQueues } from '@/components/pools/pools';
import PageHeader from '@/components/shared/PageHeader.vue';
import StatusPill from '@/components/shared/StatusPill.vue';
import type { Tone } from '@/components/shared/format';
import { useClock } from '@/composables/useClock';
import { useStream, type StreamStatus } from '@/composables/useStream';
import { usePoolsStore } from '@/stores/pools';
import Message from '@/volt/Message.vue';

const store = usePoolsStore();
const clock = useClock();

// Re-read the clock now and then, so "waiting 12 min" and heartbeat ages keep up.
const tick = ref(0);
const timer = setInterval(() => tick.value++, 15_000);
onScopeDispose(() => clearInterval(timer));
const now = computed(() => {
  void tick.value;
  return clock();
});

const queues = computed(() => sortQueues(store.queues));
const groups = computed(() => poolGroups(store.pools, store.agents));

onMounted(() => {
  store.load();
});

// No queue or pool topic exists: agents update themselves, a task change reloads the queues,
// and a re-created stream (which can't resume) reloads everything.
const stream = useStream(
  ['agents', 'tasks'],
  {
    'agent.updated': (agent) => store.upsertAgent(agent),
    'task.updated': () => {
      store.refreshQueues();
    },
  },
  { onResync: () => store.load({ quiet: true }) },
);

const STREAM: Partial<Record<StreamStatus, { label: string; tone: Tone }>> = {
  connecting: { label: 'connecting', tone: 'neutral' },
  open: { label: 'live', tone: 'info' },
  reconnecting: { label: 'reconnecting', tone: 'warn' },
};
const streamPill = computed(() => STREAM[stream.status.value]);
</script>

<template>
  <PageHeader title="Queues and pools">
    What waits in each queue and why, and the agents that take the work.
  </PageHeader>

  <p
    v-if="store.loading"
    class="mb-4 text-sm text-ccf-600 dark:text-ccf-400"
    role="status"
  >
    Loading…
  </p>

  <template v-else>
    <section aria-labelledby="queues-heading" class="mb-8">
      <h2 id="queues-heading" class="mb-3 text-lg font-semibold">Queues</h2>
      <Message
        v-if="store.queuesError"
        severity="error"
        class="mb-4"
        role="alert"
      >
        Couldn't load the queues: {{ store.queuesError }}
      </Message>
      <div
        v-if="queues.length"
        class="grid gap-4 md:grid-cols-2 xl:grid-cols-3"
      >
        <QueueCard
          v-for="view in queues"
          :key="view.queue.metadata.name"
          :view="view"
          :now="now"
        />
      </div>
      <p
        v-else-if="store.queuesLoaded"
        class="text-sm text-ccf-500 dark:text-ccf-400"
      >
        No queues.
      </p>
    </section>

    <section aria-labelledby="pools-heading">
      <div class="mb-3 flex items-center gap-2">
        <h2 id="pools-heading" class="text-lg font-semibold">
          Pools and agents
        </h2>
        <StatusPill
          v-if="streamPill"
          :label="streamPill.label"
          :tone="streamPill.tone"
          data-testid="stream-status"
        />
      </div>
      <Message
        v-if="store.poolsError"
        severity="error"
        class="mb-4"
        role="alert"
      >
        Couldn't load the pools: {{ store.poolsError }}
      </Message>
      <Message
        v-if="store.agentsError"
        severity="error"
        class="mb-4"
        role="alert"
      >
        Couldn't load the agents: {{ store.agentsError }}
      </Message>
      <div v-if="groups.length" class="flex flex-col gap-4">
        <PoolCard
          v-for="group in groups"
          :key="group.name"
          :group="group"
          :now="now"
        />
      </div>
      <p
        v-else-if="store.poolsLoaded"
        class="text-sm text-ccf-500 dark:text-ccf-400"
      >
        No pools.
      </p>
    </section>
  </template>
</template>
