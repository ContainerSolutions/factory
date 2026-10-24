<script setup lang="ts">
import { computed } from 'vue';
import type { Schemas } from '@/api/client';
import { formatTime, toneOf, type Tone } from '@/components/shared/format';
import { graphOf, type GraphAction, type GraphTask } from './graph';

/**
 * The work item as a hand-laid graph, left to right in time: a column per event (a circle), the
 * actions its fired decisions took below it (rectangles; dotted when deterministic, solid for a
 * dispatch), each arrow labelled with its rule, each dispatch's task beside it (coloured by its
 * phase), and links to what they caused (dashed when inferred). A link to an event leaves its
 * node to the right, climbs in the gutter after it and runs in the lane above the actions, so
 * it crosses no box. Every node is a button: an action opens its decision's "why", a task its
 * entry in the LLM tasks tab, an event its row in the timeline.
 */
const props = defineProps<{ detail: Schemas['WorkItemDetail'] }>();
const emit = defineEmits<{
  why: [index: number];
  event: [key: string];
  task: [id: string];
}>();

const COL_W = 248;
const PAD = 16;
const EV_Y = 40;
const R = 14;
const ACT_X = 52;
const ACT_W = 180;
const ACT_H = 50;
const ACT_Y = 96;
const ROW_H = 74;
const TASK_GAP = 32;
const TASK_W = 176;
const TASK_H = 58;
/** Room after a task for its link's gutter, before the next column. */
const TASK_TRAIL = 24;
/** The lanes links to events run in, between the event labels and the first rule label. */
const LANE_Y = 62;
const LANES = 6;
const LANE_STEP = 3.5;
const GUTTER = 10;
const CORNER = 5;

const graph = computed(() => graphOf(props.detail));

/** Each column's left edge: a column with a task is wider by the task's sub-column. */
const columns = computed(() => {
  const xs: number[] = [];
  let x = PAD;
  for (const e of graph.value.events) {
    xs.push(x);
    x +=
      COL_W +
      (e.actions.some((a) => a.taskNode) ? TASK_GAP + TASK_W + TASK_TRAIL : 0);
  }
  return { xs, width: x + PAD };
});

function colX(column: number): number {
  return columns.value.xs[column] ?? PAD;
}

interface Box {
  x: number;
  y: number;
  w: number;
  h: number;
}

interface Placed extends Box {
  action: GraphAction;
  cx: number;
}

interface PlacedTask extends Box {
  node: GraphTask;
}

const placed = computed(() => {
  const actions = new Map<string, Placed>();
  const tasks = new Map<string, PlacedTask>();
  for (const e of graph.value.events) {
    const x0 = colX(e.column);
    e.actions.forEach((action, row) => {
      const a: Placed = {
        action,
        x: x0 + ACT_X,
        y: ACT_Y + row * ROW_H,
        w: ACT_W,
        h: ACT_H,
        cx: x0 + PAD + R,
      };
      actions.set(action.key, a);
      if (action.taskNode) {
        tasks.set(action.taskNode.key, {
          node: action.taskNode,
          x: a.x + ACT_W + TASK_GAP,
          y: a.y - (TASK_H - ACT_H) / 2,
          w: TASK_W,
          h: TASK_H,
        });
      }
    });
  }
  return { actions, tasks };
});

const tasks = computed(() => [...placed.value.tasks.values()]);

const width = computed(() => columns.value.width);
const height = computed(() => {
  const rows = Math.max(0, ...graph.value.events.map((e) => e.actions.length));
  return ACT_Y + rows * ROW_H + PAD;
});

const eventX = (column: number) => colX(column) + PAD + R;

function box(key: string): Box | undefined {
  return placed.value.actions.get(key) ?? placed.value.tasks.get(key);
}

/** From a node's right edge, up the gutter after it, along a lane, and into the event's left. */
function toEvent(from: Box, column: number, lane: number): string {
  const x1 = from.x + from.w;
  const y1 = from.y + from.h / 2;
  const ex = eventX(column) - R - 2;
  const gx = x1 + GUTTER;
  const ly = LANE_Y + (lane % LANES) * LANE_STEP;
  if (ex < gx + 24) {
    const bend = Math.max(40, Math.abs(ex - x1) / 3);
    return `M ${x1} ${y1} C ${x1 + bend} ${y1}, ${ex - bend} ${EV_Y}, ${ex} ${EV_Y}`;
  }
  const c = CORNER;
  return [
    `M ${x1} ${y1}`,
    `H ${gx - c}`,
    `Q ${gx} ${y1}, ${gx} ${y1 - c}`,
    `V ${ly + c}`,
    `Q ${gx} ${ly}, ${gx + c} ${ly}`,
    `H ${ex - 18}`,
    `C ${ex - 6} ${ly}, ${ex - 10} ${EV_Y}, ${ex} ${EV_Y}`,
  ].join(' ');
}

const links = computed(() => {
  let lane = 0;
  return graph.value.links.flatMap((l) => {
    const from = box(l.from);
    if (!from) return [];
    const task = placed.value.tasks.get(l.to);
    let d: string;
    if (task) {
      const y = from.y + from.h / 2;
      d = `M ${from.x + from.w} ${y} H ${task.x - 2}`;
    } else {
      const to = graph.value.events.find((e) => e.key === l.to);
      if (!to) return [];
      d = toEvent(from, to.column, lane++);
    }
    return [
      {
        key: `${l.from}->${l.to}`,
        d,
        inferred: l.inferred,
        cause: l.cause,
      },
    ];
  });
});

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
                :cx="eventX(e.column)"
                :cy="EV_Y"
                :r="R"
                class="fill-white stroke-ccf-700 dark:fill-ccf-900 dark:stroke-ccf-300"
                stroke-width="2"
              />
              <text
                :x="eventX(e.column) + R + 6"
                :y="EV_Y - 2"
                class="fill-current text-xs font-semibold"
              >
                {{ clip(e.type, 26) }}
              </text>
              <text
                v-if="e.at"
                :x="eventX(e.column) + R + 6"
                :y="EV_Y + 12"
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
                :d="`M ${placed.actions.get(a.key)!.cx} ${EV_Y + R} V ${placed.actions.get(a.key)!.y + ACT_H / 2} H ${placed.actions.get(a.key)!.x - 2}`"
                fill="none"
                class="stroke-ccf-400"
                stroke-width="1.5"
                marker-end="url(#wi-arrow)"
              />
              <text
                :x="placed.actions.get(a.key)!.x"
                :y="placed.actions.get(a.key)!.y - 5"
                class="fill-ccf-500 text-[10px]"
                data-testid="graph-rule"
              >
                {{ clip(a.rule, 34) }}
              </text>
              <rect
                :x="placed.actions.get(a.key)!.x"
                :y="placed.actions.get(a.key)!.y"
                :width="ACT_W"
                :height="ACT_H"
                rx="4"
                class="fill-white dark:fill-ccf-900"
                :class="STROKE[toneFor(a.state)]"
                stroke-width="2"
                :stroke-dasharray="a.deterministic ? '2 3' : undefined"
              />
              <text
                :x="placed.actions.get(a.key)!.x + 8"
                :y="placed.actions.get(a.key)!.y + 19"
                class="fill-current text-xs font-medium"
              >
                {{ clip(actionTitle(a), 28) }}
              </text>
              <text
                :x="placed.actions.get(a.key)!.x + 8"
                :y="placed.actions.get(a.key)!.y + 37"
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
