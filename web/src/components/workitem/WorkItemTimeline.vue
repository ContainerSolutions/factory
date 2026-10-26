<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue';
import { RouterLink } from 'vue-router';
import type { Schemas } from '@/api/client';
import ReplayButton from '@/components/controls/ReplayButton.vue';
import { runStateOf } from '@/components/run/run';
import StatusPill from '@/components/shared/StatusPill.vue';
import {
  formatTime,
  formatValue,
  railVerdictLabel,
  safeURL,
  toneOf,
} from '@/components/shared/format';
import SecondaryButton from '@/volt/SecondaryButton.vue';
import { asRecord } from './board';
import { graphOf, type GraphAction } from './graph';
import { eventRows, unattached, type RowDecision } from './timeline';

/**
 * The work item's events as collapsible rows, oldest first. Open, a row shows the decisions
 * that fired, with their actions and rails, and keeps the others behind "show N not fired",
 * each with why it didn't: its reason, the inputs `where` saw, and the rails. A decision an admin's
 * replay recorded is marked as one; an admin can replay each event.
 */
const props = defineProps<{
  detail: Schemas['WorkItemDetail'];
  /** An event row to open and scroll to (from the graph). */
  focus?: string;
}>();

const emit = defineEmits<{
  /** Open the "why" panel for `workItem.status.decisions[index]`. */
  why: [index: number];
  /** An event was replayed: the work item has new decisions. */
  replayed: [Schemas['ReplayResult']];
}>();

const graph = computed(() => graphOf(props.detail));
const rows = computed(() => eventRows(props.detail, graph.value));
const others = computed(() => unattached(props.detail, graph.value));

/** The open rows (the newest is open at first), and the rows showing what didn't fire. */
const open = ref(new Set<string>());
const showNotFired = ref(new Set<string>());

function toggle(set: Set<string>, key: string): void {
  if (set.has(key)) set.delete(key);
  else set.add(key);
}

watch(
  () => rows.value[rows.value.length - 1]?.key,
  (key) => {
    if (key && !open.value.size) open.value.add(key);
  },
  { immediate: true },
);

watch(
  () => props.focus,
  async (key) => {
    if (!key) return;
    open.value.add(key);
    await nextTick();
    document.getElementById(rowId(key))?.scrollIntoView?.({ block: 'nearest' });
  },
  { immediate: true },
);

function rowId(key: string): string {
  return `row-${key.replace(/[^A-Za-z0-9_-]/g, '-')}`;
}

/** The provider's title for an event, shown as text: it is untrusted. */
function eventTitle(event: Schemas['Event'] | undefined): string | undefined {
  const title = asRecord(event?.data.facts).title;
  return typeof title === 'string' ? title : undefined;
}

function heldRails(decision: Schemas['Decision']): Schemas['RailVerdict'][] {
  return (decision.rails ?? []).filter((r) => r.verdict !== 'allowed');
}

const STATE_TONE: Record<string, 'success' | 'neutral'> = {
  applied: 'success',
  done: 'neutral',
};

function stateTone(state: string) {
  return STATE_TONE[state] ?? toneOf(state);
}

function whyNot(d: RowDecision): string {
  return d.decision.reason ?? 'did not fire';
}

function actionLabel(a: GraphAction): string {
  return a.subject ? `${a.kind} · ${a.subject}` : a.kind;
}

const KIND_LABEL = { task: 'Task', run: 'Run', effect: 'Effect' } as const;
</script>

<template>
  <section aria-label="Timeline">
    <ol v-if="rows.length" class="flex flex-col gap-2">
      <li
        v-for="row in rows"
        :id="rowId(row.key)"
        :key="row.key"
        class="rounded-md border border-ccf-200 text-sm dark:border-ccf-800"
        :class="focus === row.key ? 'ring-2 ring-sky-400' : ''"
        data-testid="timeline-event"
      >
        <button
          type="button"
          class="flex w-full flex-wrap items-center gap-2 p-3 text-left"
          :aria-expanded="open.has(row.key)"
          :aria-controls="`${rowId(row.key)}-body`"
          data-testid="timeline-event-toggle"
          @click="toggle(open, row.key)"
        >
          <span aria-hidden="true" class="w-3 text-ccf-500">{{
            open.has(row.key) ? '▾' : '▸'
          }}</span>
          <span class="font-mono text-xs font-semibold">{{
            row.node.type
          }}</span>
          <span v-if="row.node.event" class="text-ccf-700 dark:text-ccf-300">
            on {{ row.node.event.source
            }}<template v-if="row.node.event.subject">
              · {{ row.node.event.subject }}</template
            >
          </span>
          <span v-else class="font-mono text-xs text-ccf-500">{{
            row.node.id
          }}</span>
          <span class="text-xs text-ccf-600 dark:text-ccf-400"
            >{{ row.fired.length }} fired<template v-if="row.notFired.length">
              · {{ row.notFired.length }} not fired</template
            ></span
          >
          <time
            v-if="row.node.at"
            :datetime="row.node.at"
            class="ms-auto text-xs text-ccf-600 dark:text-ccf-400"
            >{{ formatTime(row.node.at) }}</time
          >
        </button>

        <div
          v-if="open.has(row.key)"
          :id="`${rowId(row.key)}-body`"
          class="border-t border-ccf-200 p-3 dark:border-ccf-800"
          data-testid="timeline-event-body"
        >
          <p v-if="eventTitle(row.node.event)" class="mb-1">
            {{ eventTitle(row.node.event) }}
          </p>
          <p
            v-if="row.node.event?.data.actor"
            class="mb-2 text-xs text-ccf-600 dark:text-ccf-400"
          >
            by {{ row.node.event.data.actor.id }}
          </p>
          <p v-if="!row.node.event" class="mb-2 text-xs text-ccf-600">
            The work item no longer holds this event; its decisions name it.
          </p>
          <ReplayButton
            :event="row.node.id"
            class="mb-2"
            @replayed="emit('replayed', $event)"
          />

          <ul v-if="row.fired.length" class="flex flex-col gap-2">
            <li
              v-for="d in row.fired"
              :key="d.index"
              class="rounded border border-ccf-200 p-2 dark:border-ccf-800"
              data-testid="timeline-decision"
            >
              <div class="flex flex-wrap items-center gap-2">
                <span class="font-medium"
                  >{{ d.decision.ruleSet }} / {{ d.decision.rule }}</span
                >
                <StatusPill label="fired" tone="success" />
                <StatusPill
                  v-if="d.decision.replay"
                  label="replay"
                  tone="info"
                  data-testid="decision-replay"
                />
                <StatusPill
                  v-for="rail in heldRails(d.decision)"
                  :key="rail.rail"
                  :label="`${rail.rail}: ${railVerdictLabel(rail.verdict)}`"
                  :tone="rail.verdict === 'deferred' ? 'warn' : 'danger'"
                />
                <SecondaryButton
                  size="small"
                  class="ms-auto"
                  label="Why?"
                  :aria-label="`Why: ${d.decision.ruleSet} / ${d.decision.rule}`"
                  aria-haspopup="dialog"
                  @click="emit('why', d.index)"
                />
              </div>
              <ul class="mt-1 flex flex-col gap-1 ps-2">
                <li
                  v-for="a in d.actions"
                  :key="a.key"
                  data-testid="timeline-action"
                >
                  <div class="flex flex-wrap items-center gap-2">
                    <span class="font-mono text-xs">{{ actionLabel(a) }}</span>
                    <StatusPill :label="a.state" :tone="stateTone(a.state)" />
                  </div>
                  <ul
                    v-if="a.runs.length"
                    class="mt-1 flex flex-col gap-1 ps-4 text-xs"
                  >
                    <li
                      v-for="r in a.runs"
                      :key="r.metadata.name"
                      class="flex flex-wrap items-center gap-2"
                    >
                      <RouterLink
                        :to="{ name: 'run', params: { id: r.metadata.name } }"
                        class="font-mono underline"
                        >{{ r.metadata.name }}</RouterLink
                      >
                      <span class="text-ccf-600 dark:text-ccf-400"
                        >attempt {{ r.spec.attempt }} on
                        {{ r.spec.agent }}</span
                      >
                      <StatusPill :label="runStateOf(r)" />
                      <StatusPill
                        v-if="r.status?.result?.outcome"
                        :label="`outcome: ${r.status.result.outcome}`"
                        tone="neutral"
                      />
                      <span
                        v-if="r.status?.result?.summary"
                        class="basis-full text-ccf-700 dark:text-ccf-300"
                        >{{ r.status.result.summary }}</span
                      >
                    </li>
                  </ul>
                  <ul
                    v-for="f in [a.effect, ...a.taskEffects].filter(
                      (x) => x !== undefined,
                    )"
                    :key="f.metadata.name"
                    class="mt-1 ps-4 text-xs"
                  >
                    <li>
                      <span class="font-mono">{{ f.spec.op }}</span> on
                      <span class="break-all">{{ f.spec.target.ref }}</span>
                      <span
                        v-if="f.status?.result?.error"
                        class="ms-1 text-red-700 dark:text-red-300"
                        >failed: {{ f.status.result.error }}</span
                      >
                      <a
                        v-else-if="safeURL(f.status?.result?.url ?? undefined)"
                        :href="safeURL(f.status?.result?.url ?? undefined)"
                        target="_blank"
                        rel="noopener noreferrer"
                        class="ms-1 break-all underline"
                        >{{ f.status?.result?.url }}</a
                      >
                    </li>
                  </ul>
                </li>
              </ul>
            </li>
          </ul>
          <p v-else class="text-xs text-ccf-600 dark:text-ccf-400">
            No rule fired.
          </p>

          <template v-if="row.notFired.length">
            <button
              type="button"
              class="mt-2 text-xs font-medium text-ccf-600 underline dark:text-ccf-400"
              :aria-expanded="showNotFired.has(row.key)"
              data-testid="not-fired-toggle"
              @click="toggle(showNotFired, row.key)"
            >
              {{ showNotFired.has(row.key) ? 'Hide' : 'Show' }}
              {{ row.notFired.length }} not fired
            </button>
            <ul
              v-if="showNotFired.has(row.key)"
              class="mt-1 flex flex-col gap-1"
            >
              <li
                v-for="d in row.notFired"
                :key="d.index"
                class="rounded border border-dashed border-ccf-300 p-2 dark:border-ccf-700"
                data-testid="timeline-not-fired"
              >
                <div class="flex flex-wrap items-center gap-2">
                  <span class="font-medium"
                    >{{ d.decision.ruleSet }} / {{ d.decision.rule }}</span
                  >
                  <StatusPill label="did not fire" tone="neutral" />
                  <StatusPill
                    v-if="d.decision.replay"
                    label="replay"
                    tone="info"
                    data-testid="decision-replay"
                  />
                  <span class="text-xs" data-testid="not-fired-why">{{
                    whyNot(d)
                  }}</span>
                  <SecondaryButton
                    size="small"
                    class="ms-auto"
                    label="Why?"
                    :aria-label="`Why: ${d.decision.ruleSet} / ${d.decision.rule}`"
                    aria-haspopup="dialog"
                    @click="emit('why', d.index)"
                  />
                </div>
                <p
                  v-if="
                    d.decision.inputs && Object.keys(d.decision.inputs).length
                  "
                  class="mt-1 font-mono text-xs break-all text-ccf-600 dark:text-ccf-400"
                  data-testid="not-fired-inputs"
                >
                  <span
                    v-for="(v, k) in d.decision.inputs"
                    :key="k"
                    class="me-3"
                    >{{ k }} = {{ formatValue(v) }}</span
                  >
                </p>
                <p
                  v-for="rail in heldRails(d.decision)"
                  :key="rail.rail"
                  class="mt-1 text-xs"
                >
                  {{ rail.rail }}: {{ railVerdictLabel(rail.verdict)
                  }}{{ rail.reason ? ` (${rail.reason})` : '' }}
                </p>
              </li>
            </ul>
          </template>
        </div>
      </li>
    </ol>
    <p v-else class="text-sm text-ccf-500">Nothing has happened yet.</p>

    <section v-if="others.length" aria-label="Other activity" class="mt-4">
      <h3 class="mb-2 text-sm font-semibold">Other activity</h3>
      <ul class="flex flex-col gap-1 text-sm">
        <li
          v-for="e in others"
          :key="e.key"
          class="flex flex-wrap items-center gap-2"
          :data-kind="e.kind"
          data-testid="timeline-other"
        >
          <StatusPill
            :label="KIND_LABEL[e.kind as keyof typeof KIND_LABEL]"
            tone="neutral"
          />
          <template v-if="e.kind === 'task'">
            <span class="font-medium">{{ e.task.spec.taskSpec.name }}</span>
            <StatusPill v-if="e.task.status" :label="e.task.status.phase" />
          </template>
          <template v-else-if="e.kind === 'run'">
            <RouterLink
              :to="{ name: 'run', params: { id: e.run.metadata.name } }"
              class="font-mono text-xs underline"
              >{{ e.run.metadata.name }}</RouterLink
            >
            <StatusPill :label="runStateOf(e.run)" />
          </template>
          <template v-else-if="e.kind === 'effect'">
            <span class="font-mono text-xs">{{ e.effect.spec.op }}</span> on
            <span class="break-all">{{ e.effect.spec.target.ref }}</span>
          </template>
          <time
            v-if="e.at"
            :datetime="e.at"
            class="text-xs text-ccf-600 dark:text-ccf-400"
            >{{ formatTime(e.at) }}</time
          >
        </li>
      </ul>
    </section>
  </section>
</template>
