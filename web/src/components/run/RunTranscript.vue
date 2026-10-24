<script setup lang="ts">
import { computed, reactive } from 'vue';
import StatusPill from '@/components/shared/StatusPill.vue';
import type { TranscriptState } from '@/stores/runs';
import Message from '@/volt/Message.vue';
import SecondaryButton from '@/volt/SecondaryButton.vue';
import { turnsOf, type TranscriptEntry } from './transcript';

const props = defineProps<{
  /** Whether the run has a transcript (`status.transcriptRef`). */
  available: boolean;
  state: TranscriptState;
  entries: TranscriptEntry[] | null;
  error?: string | null;
}>();

const emit = defineEmits<{ load: [] }>();

const turns = computed(() => turnsOf(props.entries ?? []));

// A turn's entries are rendered only while it is open, so a long transcript stays cheap.
const open = reactive(new Set<string>());

function toggle(key: string): void {
  if (open.has(key)) open.delete(key);
  else open.add(key);
}

function panelId(key: string): string {
  return `transcript-${key.replace(/[^a-z0-9]/gi, '-')}`;
}

function detailText(entry: TranscriptEntry): string {
  return JSON.stringify(entry.detail, null, 2);
}
</script>

<template>
  <section aria-labelledby="transcript-heading">
    <h2 id="transcript-heading" class="mb-3 text-lg font-semibold">
      Transcript
    </h2>

    <p v-if="!available" class="text-sm text-ccf-500">
      No transcript uploaded yet.
    </p>

    <template v-else-if="state === 'idle' || state === 'loading'">
      <SecondaryButton
        size="small"
        label="Load transcript"
        :loading="state === 'loading'"
        @click="emit('load')"
      />
    </template>

    <p v-else-if="state === 'missing'" class="text-sm text-ccf-500">
      No transcript was uploaded for this run.
    </p>

    <Message v-else-if="state === 'error'" severity="error" role="alert">
      Couldn't load the transcript: {{ error }}
    </Message>

    <!-- Transcripts are untrusted (redacted by the agent): rendered as text, never HTML. -->
    <ol
      v-else-if="turns.length"
      class="flex flex-col gap-2"
      data-testid="transcript"
    >
      <li
        v-for="turn in turns"
        :key="turn.key"
        class="rounded-md border border-ccf-200 dark:border-ccf-800"
        data-testid="transcript-turn"
      >
        <button
          type="button"
          class="flex w-full items-center gap-2 px-3 py-2 text-left text-sm font-medium hover:bg-ccf-100 dark:hover:bg-ccf-800"
          :aria-expanded="open.has(turn.key)"
          :aria-controls="panelId(turn.key)"
          @click="toggle(turn.key)"
        >
          <span aria-hidden="true">{{ open.has(turn.key) ? '▾' : '▸' }}</span>
          {{ turn.label }}
          <span class="text-xs font-normal text-ccf-600 dark:text-ccf-400"
            >{{ turn.entries.length }}
            {{ turn.entries.length === 1 ? 'entry' : 'entries' }}</span
          >
        </button>
        <ul
          v-if="open.has(turn.key)"
          :id="panelId(turn.key)"
          class="flex flex-col gap-2 border-t border-ccf-200 p-3 text-sm dark:border-ccf-800"
        >
          <li
            v-for="entry in turn.entries"
            :key="entry.index"
            data-testid="transcript-entry"
          >
            <div class="mb-1 flex flex-wrap items-center gap-2">
              <StatusPill :label="entry.type" tone="neutral" />
              <span v-if="entry.role" class="font-medium">{{
                entry.role
              }}</span>
              <span v-if="entry.name" class="font-mono text-xs">{{
                entry.name
              }}</span>
            </div>
            <pre
              v-if="entry.text"
              class="font-sans whitespace-pre-wrap [overflow-wrap:anywhere]"
              >{{ entry.text }}</pre
            >
            <pre
              v-if="entry.detail"
              class="max-w-full overflow-x-auto rounded bg-ccf-100 p-2 font-mono text-xs dark:bg-ccf-900"
              >{{ detailText(entry) }}</pre
            >
          </li>
        </ul>
      </li>
    </ol>

    <p v-else class="text-sm text-ccf-500">The transcript is empty.</p>
  </section>
</template>
