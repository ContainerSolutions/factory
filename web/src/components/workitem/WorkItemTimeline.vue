<script setup lang="ts">
import { computed } from 'vue';
import type { Schemas } from '@/api/client';
import StatusPill from '@/components/shared/StatusPill.vue';
import { formatTime, railVerdictLabel } from '@/components/shared/format';
import SecondaryButton from '@/volt/SecondaryButton.vue';
import { asRecord } from './board';
import { timelineOf } from './timeline';

const props = defineProps<{ detail: Schemas['WorkItemDetail'] }>();

const emit = defineEmits<{
  /** Open the "why" panel for `workItem.status.decisions[index]`. */
  why: [index: number];
}>();

const entries = computed(() => timelineOf(props.detail));

const KIND_LABEL = {
  event: 'Event',
  decision: 'Decision',
  task: 'Task',
  run: 'Run',
  effect: 'Effect',
} as const;

/** The provider's title for an event, shown as text: it is untrusted. */
function eventTitle(event: Schemas['Event']): string | undefined {
  const title = asRecord(event.data.facts).title;
  return typeof title === 'string' ? title : undefined;
}

/** A link for an https URL only; anything else is shown as text. */
function safeURL(url: string | undefined): string | undefined {
  return url && /^https:\/\//.test(url) ? url : undefined;
}

function blockingRails(
  decision: Schemas['Decision'],
): Schemas['RailVerdict'][] {
  return (decision.rails ?? []).filter((r) => r.verdict !== 'allowed');
}
</script>

<template>
  <section aria-labelledby="timeline-heading">
    <h2 id="timeline-heading" class="mb-3 text-lg font-semibold">Timeline</h2>
    <ol
      v-if="entries.length"
      class="relative flex flex-col gap-3 border-s-2 border-ccf-200 ps-4 dark:border-ccf-800"
    >
      <li
        v-for="entry in entries"
        :key="entry.key"
        class="rounded-md border border-ccf-200 p-3 text-sm dark:border-ccf-800"
        :data-kind="entry.kind"
        data-testid="timeline-entry"
      >
        <div class="mb-1 flex flex-wrap items-center gap-2">
          <StatusPill :label="KIND_LABEL[entry.kind]" tone="neutral" />
          <time
            v-if="entry.at"
            :datetime="entry.at"
            class="text-xs text-ccf-600 dark:text-ccf-400"
            >{{ formatTime(entry.at) }}</time
          >
        </div>

        <template v-if="entry.kind === 'event'">
          <p>
            <span class="font-mono text-xs">{{ entry.event.type }}</span>
            on
            <span class="font-medium">{{ entry.event.source }}</span
            ><template v-if="entry.event.subject">
              · {{ entry.event.subject }}</template
            >
          </p>
          <p
            v-if="eventTitle(entry.event)"
            class="mt-1 text-ccf-700 dark:text-ccf-300"
          >
            {{ eventTitle(entry.event) }}
          </p>
          <p
            v-if="entry.event.data.actor"
            class="mt-1 text-xs text-ccf-600 dark:text-ccf-400"
          >
            by {{ entry.event.data.actor.id }}
          </p>
        </template>

        <template v-else-if="entry.kind === 'decision'">
          <div class="flex flex-wrap items-center gap-2">
            <span class="font-medium"
              >{{ entry.decision.ruleSet }} / {{ entry.decision.rule }}</span
            >
            <StatusPill
              :label="entry.decision.fired ? 'fired' : 'did not fire'"
              :tone="entry.decision.fired ? 'success' : 'neutral'"
            />
            <StatusPill
              v-for="rail in blockingRails(entry.decision)"
              :key="rail.rail"
              :label="`${rail.rail}: ${railVerdictLabel(rail.verdict)}`"
              :tone="rail.verdict === 'deferred' ? 'warn' : 'danger'"
            />
            <SecondaryButton
              size="small"
              class="ms-auto"
              label="Why?"
              :aria-label="`Why: ${entry.decision.ruleSet} / ${entry.decision.rule}`"
              aria-haspopup="dialog"
              @click="emit('why', entry.index)"
            />
          </div>
          <p
            v-if="entry.decision.actions?.length"
            class="mt-1 text-xs text-ccf-600 dark:text-ccf-400"
          >
            actions: {{ entry.decision.actions.join(', ') }}
          </p>
        </template>

        <template v-else-if="entry.kind === 'task'">
          <div class="flex flex-wrap items-center gap-2">
            <span class="font-medium">{{ entry.task.spec.taskSpec.name }}</span>
            <span
              v-if="entry.task.spec.queue"
              class="text-xs text-ccf-600 dark:text-ccf-400"
              >queue {{ entry.task.spec.queue }}</span
            >
            <StatusPill
              v-if="entry.task.status"
              :label="entry.task.status.phase"
            />
          </div>
          <p class="mt-1 font-mono text-xs text-ccf-600 dark:text-ccf-400">
            {{ entry.task.metadata.id }}
          </p>
        </template>

        <template v-else-if="entry.kind === 'run'">
          <div class="flex flex-wrap items-center gap-2">
            <span class="font-mono text-xs">{{ entry.run.metadata.name }}</span>
            <span class="text-xs text-ccf-600 dark:text-ccf-400"
              >attempt {{ entry.run.spec.attempt }} on
              {{ entry.run.spec.agent }}</span
            >
            <StatusPill :label="entry.run.status?.end ?? 'running'" />
            <StatusPill
              v-if="entry.run.status?.result?.outcome"
              :label="`outcome: ${entry.run.status.result.outcome}`"
              tone="neutral"
            />
          </div>
          <p
            v-if="entry.run.status?.result?.summary"
            class="mt-1 text-ccf-700 dark:text-ccf-300"
          >
            {{ entry.run.status.result.summary }}
          </p>
        </template>

        <template v-else-if="entry.kind === 'effect'">
          <div class="flex flex-wrap items-center gap-2">
            <span class="font-mono text-xs">{{ entry.effect.spec.op }}</span>
            on
            <span class="break-all">{{ entry.effect.spec.target.ref }}</span>
            <StatusPill
              v-if="entry.effect.status?.result?.error"
              label="failed"
              tone="danger"
            />
          </div>
          <p
            v-if="entry.effect.status?.result?.error"
            class="mt-1 text-red-700 dark:text-red-300"
          >
            {{ entry.effect.status.result.error }}
          </p>
          <p v-else-if="entry.effect.status?.result?.url" class="mt-1 text-xs">
            <a
              v-if="safeURL(entry.effect.status.result.url)"
              :href="safeURL(entry.effect.status.result.url)"
              target="_blank"
              rel="noopener noreferrer"
              class="underline break-all"
              >{{ entry.effect.status.result.url }}</a
            >
            <span v-else class="break-all">{{
              entry.effect.status.result.url
            }}</span>
          </p>
        </template>
      </li>
    </ol>
    <p v-else class="text-sm text-ccf-500">Nothing has happened yet.</p>
  </section>
</template>
