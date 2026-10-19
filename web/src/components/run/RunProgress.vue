<script setup lang="ts">
import { computed } from 'vue';
import type { Schemas } from '@/api/client';
import StatusPill from '@/components/shared/StatusPill.vue';
import { formatTime, type Tone } from '@/components/shared/format';
import type { StreamStatus } from '@/composables/useStream';
import { turnOf } from './run';

const props = defineProps<{
  events: Schemas['ProgressEvent'][];
  /** Whether the run is still going; a finished run shows no stream status. */
  running: boolean;
  stream?: StreamStatus;
}>();

const STREAM: Partial<Record<StreamStatus, { label: string; tone: Tone }>> = {
  connecting: { label: 'connecting', tone: 'neutral' },
  open: { label: 'live', tone: 'info' },
  reconnecting: { label: 'reconnecting', tone: 'warn' },
};

const streamPill = computed(() =>
  props.running && props.stream ? STREAM[props.stream] : undefined,
);
</script>

<template>
  <section aria-labelledby="progress-heading">
    <div class="mb-3 flex items-center gap-2">
      <h2 id="progress-heading" class="text-lg font-semibold">Progress</h2>
      <StatusPill
        v-if="streamPill"
        :label="streamPill.label"
        :tone="streamPill.tone"
        data-testid="stream-status"
      />
    </div>
    <!-- The agent's messages are untrusted: text only. -->
    <ol
      v-if="events.length"
      class="flex flex-col gap-1 text-sm"
      aria-live="polite"
      aria-relevant="additions"
      data-testid="progress"
    >
      <li
        v-for="(event, i) in events"
        :key="i"
        class="flex flex-wrap items-baseline gap-2"
        data-testid="progress-event"
      >
        <time
          :datetime="event.at"
          class="font-mono text-xs text-ccf-600 dark:text-ccf-400"
          >{{ formatTime(event.at) }}</time
        >
        <StatusPill :label="event.kind" tone="neutral" />
        <span class="break-all">{{ event.message }}</span>
        <span
          v-if="turnOf(event) !== undefined"
          class="text-xs text-ccf-600 dark:text-ccf-400"
          >turn {{ turnOf(event) }}</span
        >
      </li>
    </ol>
    <p v-else class="text-sm text-ccf-500">No progress yet.</p>
  </section>
</template>
