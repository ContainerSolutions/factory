<script setup lang="ts">
import { computed } from 'vue';
import type { Schemas } from '@/api/client';
import { formatTime, toneOf, type Tone } from '@/components/shared/format';
import { graphOf, type GraphAction, type GraphTask } from './graph';
import { ACT_H, ACT_W, R, layoutGraph } from './graphLayout';

/**
 * The work item as a hand-laid graph, left to right in time: a column per event (a circle), the
 * actions its fired decisions took on the rows below it (rectangles; dotted when deterministic, solid for a
 * dispatch), each arrow labelled with its rule, each dispatch's task beside it (coloured by its
 * phase), and links to what they caused (dashed when inferred). Each event sits on the row of
 * what caused it, so that arrow is a straight line (graphLayout.ts lays it out). Every node is a
 * button: an action opens its decision's "why", a task its
 * entry in the LLM tasks tab, an event its row in the timeline.
 */
const props = defineProps<{ detail: Schemas['WorkItemDetail'] }>();
const emit = defineEmits<{
  why: [index: number];
  event: [key: string];
  task: [id: string];
}>();

const graph = computed(() => graphOf(props.detail));
const layout = computed(() => layoutGraph(graph.value));

const width = computed(() => layout.value.width);
const height = computed(() => layout.value.height);
const tasks = computed(() =>
  graph.value.events.flatMap((e) =>
    e.actions.flatMap((a) =>
      a.taskNode
        ? [{ node: a.taskNode, ...layout.value.tasks.get(a.taskNode.key)! }]
        : [],
    ),
  ),
);
const links = computed(() => layout.value.links);

function ev(key: string) {
  return layout.value.events.get(key)!;
}
function act(key: string) {
  return layout.value.actions.get(key)!;
}

const STATE_TONE: Record<string, Tone> = {
  applied: 'success',
  done: 'neutral',
  'no task': 'neutral',
  'not recorded': 'neutral',
};

function toneFor(state: string): Tone {
  return STATE_TONE[state] ?? toneOf(state);
}

const STROKE: Record<Tone, string> = {
  neutral: 'stroke-ccf-400 dark:stroke-ccf-500',
  info: 'stroke-sky-600 dark:stroke-sky-400',
  success: 'stroke-green-600 dark:stroke-green-400',
  warn: 'stroke-amber-600 dark:stroke-amber-400',
  danger: 'stroke-red-600 dark:stroke-red-400',
};
const TEXT: Record<Tone, string> = {
  neutral: 'fill-ccf-600 dark:fill-ccf-300',
  info: 'fill-sky-800 dark:fill-sky-300',
  success: 'fill-green-800 dark:fill-green-300',
  warn: 'fill-amber-800 dark:fill-amber-300',
  danger: 'fill-red-800 dark:fill-red-300',
};
/** A task's fill: the tint of its phase's pill in the LLM tasks tab. */
const FILL: Record<Tone, string> = {
  neutral: 'fill-ccf-100 dark:fill-ccf-800',
  info: 'fill-sky-50 dark:fill-sky-950',
  success: 'fill-green-50 dark:fill-green-950',
  warn: 'fill-amber-50 dark:fill-amber-950',
  danger: 'fill-red-50 dark:fill-red-950',
};

function clip(text: string, n: number): string {
  return text.length > n ? `${text.slice(0, n - 1)}…` : text;
}

function actionTitle(a: GraphAction): string {
  return a.subject ? `${a.kind} · ${a.subject}` : a.kind;
}

function attemptsText(t: GraphTask): string {
  return `${t.attempts} ${t.attempts === 1 ? 'attempt' : 'attempts'}`;
}

/** The task's third line: the latest run's outcome, else why it waits. */
function taskNote(t: GraphTask): string {
  if (t.outcome) return `outcome: ${t.outcome}`;
  return t.reason ? `waiting: ${t.reason}` : 'no run yet';
}

function taskLabel(t: GraphTask): string {
  return `Task ${t.spec}: ${t.phase}, ${attemptsText(t)}, ${taskNote(t)}. Show it in LLM tasks`;
}

function activate(e: KeyboardEvent, fn: () => void): void {
  if (e.key === 'Enter' || e.key === ' ') {
    e.preventDefault();
    fn();
  }
}
</script>

<template>
  <section aria-label="Graph" data-testid="workitem-graph">
    <p
      v-if="!graph.events.length"
      class="text-sm text-ccf-500 dark:text-ccf-400"
    >
      Nothing has happened yet.
    </p>
    <template v-else>
      <div
        class="overflow-x-auto rounded-md border border-ccf-200 dark:border-ccf-800"
      >
        <svg
          :width="width"
          :height="height"
          :viewBox="`0 0 ${width} ${height}`"
          role="group"
          aria-label="Events, the actions their rules took, their tasks, and what those caused"
          class="text-ccf-700 dark:text-ccf-300"
        >
          <defs>
            <marker
              id="wi-arrow"
              viewBox="0 0 10 10"
              refX="9"
              refY="5"
              markerWidth="6"
              markerHeight="6"
              orient="auto-start-reverse"
            >
              <path d="M 0 0 L 10 5 L 0 10 z" class="fill-ccf-500" />
            </marker>
          </defs>

          <g v-for="e in graph.events" :key="e.key">
            <g
              role="button"
              tabindex="0"
              class="cursor-pointer focus:outline-none [&:focus-visible>circle]:stroke-sky-600"
              :aria-label="`Event ${e.type}${e.at ? ` at ${formatTime(e.at)}` : ''}: show it in the timeline`"
              data-testid="graph-event"
              @click="emit('event', e.key)"
              @keydown="activate($event, () => emit('event', e.key))"
            >
              <title>{{ e.id }}</title>
              <circle
                :cx="ev(e.key).cx"
                :cy="ev(e.key).cy"
                :r="R"
                class="fill-white stroke-ccf-700 dark:fill-ccf-900 dark:stroke-ccf-300"
                stroke-width="2"
              />
              <text
                :x="ev(e.key).cx + R + 6"
                :y="ev(e.key).cy - 2"
                class="fill-current text-xs font-semibold"
              >
                {{ clip(e.type, 26) }}
              </text>
              <text
                v-if="e.at"
                :x="ev(e.key).cx + R + 6"
                :y="ev(e.key).cy + 12"
                class="fill-ccf-500 text-[10px]"
              >
                {{ formatTime(e.at) }}
              </text>
            </g>

            <g
              v-for="a in e.actions"
              :key="a.key"
              role="button"
              tabindex="0"
              class="cursor-pointer focus:outline-none [&:focus-visible>rect]:stroke-[3]"
              :aria-label="`${actionTitle(a)}: ${a.state}, by ${a.rule}. Why?`"
              :data-kind="a.kind"
              :data-state="a.state"
              data-testid="graph-action"
              @click="emit('why', a.decisionIndex)"
              @keydown="activate($event, () => emit('why', a.decisionIndex))"
            >
              <title>{{ actionTitle(a) }} ({{ a.rule }})</title>
              <path
                :d="`M ${act(a.key).spineX} ${ev(e.key).cy + R} V ${act(a.key).y + ACT_H / 2} H ${act(a.key).x - 2}`"
                fill="none"
                class="stroke-ccf-400"
                stroke-width="1.5"
                marker-end="url(#wi-arrow)"
              />
              <text
                :x="act(a.key).x"
                :y="act(a.key).y - 5"
                class="fill-ccf-500 text-[10px]"
                data-testid="graph-rule"
              >
                {{ clip(a.rule, 34) }}
              </text>
              <rect
                :x="act(a.key).x"
                :y="act(a.key).y"
                :width="ACT_W"
                :height="ACT_H"
                rx="4"
                class="fill-white dark:fill-ccf-900"
                :class="STROKE[toneFor(a.state)]"
                stroke-width="2"
                :stroke-dasharray="a.deterministic ? '2 3' : undefined"
              />
              <text
                :x="act(a.key).x + 8"
                :y="act(a.key).y + 19"
                class="fill-current text-xs font-medium"
              >
                {{ clip(actionTitle(a), 28) }}
              </text>
              <text
                :x="act(a.key).x + 8"
                :y="act(a.key).y + 37"
                class="text-xs"
                :class="TEXT[toneFor(a.state)]"
              >
                {{ a.state
                }}{{ a.runs.length > 1 ? ` · ${a.runs.length} attempts` : '' }}
              </text>
            </g>
          </g>

          <g
            v-for="t in tasks"
            :key="t.node.key"
            role="button"
            tabindex="0"
            class="cursor-pointer focus:outline-none [&:focus-visible>rect]:stroke-[3]"
            :aria-label="taskLabel(t.node)"
            :data-phase="t.node.phase"
            data-testid="graph-task"
            @click="emit('task', t.node.task.metadata.id)"
            @keydown="
              activate($event, () => emit('task', t.node.task.metadata.id))
            "
          >
            <title>{{ taskLabel(t.node) }}</title>
            <rect
              :x="t.x"
              :y="t.y"
              :width="t.w"
              :height="t.h"
              rx="10"
              :class="[
                FILL[toneOf(t.node.phase)],
                STROKE[toneOf(t.node.phase)],
              ]"
              stroke-width="2"
            />
            <text
              :x="t.x + 10"
              :y="t.y + 17"
              class="fill-current text-xs font-semibold"
            >
              {{ clip(t.node.spec, 22) }}
            </text>
            <text
              :x="t.x + 10"
              :y="t.y + 33"
              class="text-xs"
              :class="TEXT[toneOf(t.node.phase)]"
            >
              {{ t.node.phase }} · {{ attemptsText(t.node) }}
            </text>
            <text
              :x="t.x + 10"
              :y="t.y + 49"
              class="fill-ccf-600 text-[10px] dark:fill-ccf-400"
            >
              {{ clip(taskNote(t.node), 30) }}
            </text>
          </g>

          <path
            v-for="l in links"
            :key="l.key"
            :d="l.d"
            fill="none"
            class="pointer-events-none stroke-ccf-500"
            stroke-width="1.5"
            :stroke-dasharray="l.inferred ? '6 4' : undefined"
            marker-end="url(#wi-arrow)"
            :data-inferred="l.inferred"
            :data-cause="l.cause"
            data-testid="graph-link"
          />
        </svg>
      </div>
      <ul
        class="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-ccf-600 dark:text-ccf-400"
        aria-label="Legend"
      >
        <li>○ an event</li>
        <li>▭ solid: a dispatch</li>
        <li>┈ dotted: a deterministic action</li>
        <li>
          ▢ rounded, tinted: a dispatch's task, coloured by its phase (opens it
          in LLM tasks)
        </li>
        <li>
          → caused, as recorded: a dispatch's task, a run's completion, or the
          object an effect or a notify created (the event names it)
        </li>
        <li>
          ⇢ dashed: inferred: the tracking comment after a state change, or the
          same object later, by the factory
        </li>
      </ul>
    </template>
  </section>
</template>
