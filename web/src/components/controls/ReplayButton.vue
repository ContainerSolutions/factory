<script setup lang="ts">
import { computed, ref, watch } from 'vue';
import { API_BASE, type Schemas } from '@/api/client';
import StatusPill from '@/components/shared/StatusPill.vue';
import { useSessionStore } from '@/stores/session';
import { useWorkItemsStore } from '@/stores/workitems';
import SecondaryButton from '@/volt/SecondaryButton.vue';
import ConfirmRequest from './ConfirmRequest.vue';

/**
 * Replays one stored event through the rules (`POST /events/{id}/replay`), behind a confirmation,
 * and lists the decisions the replay recorded. Only members of the maintainers Group (`isAdmin`)
 * see it; the server refuses anyone else anyway.
 */
const props = defineProps<{ event: string }>();

const emit = defineEmits<{ replayed: [Schemas['ReplayResult']] }>();

const session = useSessionStore();
const store = useWorkItemsStore();

const open = ref(false);
const result = ref<Schemas['ReplayResult'] | null>(null);

watch(
  () => props.event,
  () => {
    result.value = null;
  },
);

const request = computed(
  () => `POST ${API_BASE}/events/${encodeURIComponent(props.event)}/replay`,
);

function send() {
  return store.replayEvent(props.event);
}

function done(data: unknown): void {
  const r = data as Schemas['ReplayResult'];
  result.value = r;
  emit('replayed', r);
}
</script>

<template>
  <div v-if="session.canAct" class="flex flex-col gap-2">
    <SecondaryButton
      size="small"
      class="self-start"
      label="Replay"
      :aria-label="`Replay event ${event}`"
      aria-haspopup="dialog"
      data-testid="replay"
      @click="open = true"
    />
    <ConfirmRequest
      v-model:visible="open"
      title="Replay this event?"
      confirm-label="Replay"
      :request="request"
      :send="send"
      @done="done"
    >
      <p>
        Evaluates event <code class="text-xs break-all">{{ event }}</code>
        again against the live catalog and the work item as it is now. A rule
        that already acted on it doesn't act again; only the rules that act now
        and didn't then do anything, and their decisions are recorded as a
        replay.
      </p>
    </ConfirmRequest>
    <section
      v-if="result"
      aria-label="Replay result"
      class="rounded border border-sky-300 bg-sky-50 p-2 text-xs dark:border-sky-800 dark:bg-sky-950"
      role="status"
      data-testid="replay-result"
    >
      <p v-if="!result.decisions.length">
        The replay changed nothing: no rule acts on it now that didn't then.
      </p>
      <template v-else>
        <p class="mb-1">
          The replay recorded {{ result.decisions.length }}
          {{ result.decisions.length === 1 ? 'decision' : 'decisions' }}:
        </p>
        <ul class="flex flex-col gap-1">
          <li
            v-for="(d, i) in result.decisions"
            :key="i"
            class="flex flex-wrap items-center gap-2"
            data-testid="replay-decision"
          >
            <span class="font-medium">{{ d.ruleSet }} / {{ d.rule }}</span>
            <StatusPill
              :label="d.fired ? 'fired' : 'did not fire'"
              :tone="d.fired ? 'success' : 'neutral'"
            />
            <StatusPill v-if="d.replay" label="replay" tone="info" />
            <span v-if="d.actions?.length" class="font-mono">{{
              d.actions.join(', ')
            }}</span>
            <span v-else-if="d.reason">{{ d.reason }}</span>
          </li>
        </ul>
      </template>
    </section>
  </div>
</template>
