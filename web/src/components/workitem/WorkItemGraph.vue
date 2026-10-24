<script setup lang="ts">
import { computed } from 'vue';
import type { Schemas } from '@/api/client';
import { formatTime, toneOf, type Tone } from '@/components/shared/format';
import { graphOf, type GraphAction } from './graph';

/**
 * The work item as a hand-laid graph, left to right in time: a column per event (a circle), the
 * actions its fired decisions took below it (rectangles; dotted when deterministic, solid for a
 * dispatch), each arrow labelled with its rule, and links to the events the actions caused
 * (dashed when inferred). Every node is a button: an action opens its decision's "why", an
 * event its row in the timeline.
 */
const props = defineProps<{ detail: Schemas['WorkItemDetail'] }>();
const emit = defineEmits<{
  why: [index: number];
  event: [key: string];
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

const graph = computed(() => graphOf(props.detail));

function colX(column: number): number {
  return PAD + column * COL_W;
}

interface Placed {
  action: GraphAction;
  x: number;
  y: number;
  cx: number;
}

const placed = computed(() => {
  const out = new Map<string, Placed>();
  for (const e of graph.value.events) {
    const x0 = colX(e.column);
    e.actions.forEach((action, row) => {
      out.set(action.key, {
        action,
        x: x0 + ACT_X,
        y: ACT_Y + row * ROW_H,
        cx: x0 + PAD + R,
      });
    });
  }
  return out;
});

const width = computed(
  () => PAD * 2 + Math.max(1, graph.value.events.length) * COL_W,
);
const height = computed(() => {
  const rows = Math.max(0, ...graph.value.events.map((e) => e.actions.length));
  return ACT_Y + rows * ROW_H + PAD;
});

const eventX = (column: number) => colX(column) + PAD + R;

const links = computed(() =>
  graph.value.links.flatMap((l) => {
    const from = placed.value.get(l.from);
    const to = graph.value.events.find((e) => e.key === l.to);
    if (!from || !to) return [];
    const x1 = from.x + ACT_W;
    const y1 = from.y + ACT_H / 2;
    const x2 = eventX(to.column) - R - 2;
    const y2 = EV_Y;
    const bend = Math.max(40, Math.abs(x2 - x1) / 3);
    return [
      {
        key: `${l.from}->${l.to}`,
        d: `M ${x1} ${y1} C ${x1 + bend} ${y1}, ${x2 - bend} ${y2}, ${x2} ${y2}`,
        inferred: l.inferred,
      },
    ];
  }),
);

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

function clip(text: string, n: number): string {
  return text.length > n ? `${text.slice(0, n - 1)}…` : text;
}

function actionTitle(a: GraphAction): string {
  return a.subject ? `${a.kind} · ${a.subject}` : a.kind;
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
          aria-label="Events, the actions their rules took, and what those caused"
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

          <path
            v-for="l in links"
            :key="l.key"
            :d="l.d"
            fill="none"
            class="stroke-ccf-500"
            stroke-width="1.5"
            :stroke-dasharray="l.inferred ? '6 4' : undefined"
            marker-end="url(#wi-arrow)"
            :data-inferred="l.inferred"
            data-testid="graph-link"
          />

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
                :d="`M ${placed.get(a.key)!.cx} ${EV_Y + R} V ${placed.get(a.key)!.y + ACT_H / 2} H ${placed.get(a.key)!.x - 2}`"
                fill="none"
                class="stroke-ccf-400"
                stroke-width="1.5"
                marker-end="url(#wi-arrow)"
              />
              <text
                :x="placed.get(a.key)!.x"
                :y="placed.get(a.key)!.y - 5"
                class="fill-ccf-500 text-[10px]"
                data-testid="graph-rule"
              >
                {{ clip(a.rule, 34) }}
              </text>
              <rect
                :x="placed.get(a.key)!.x"
                :y="placed.get(a.key)!.y"
                :width="ACT_W"
                :height="ACT_H"
                rx="4"
                class="fill-white dark:fill-ccf-900"
                :class="STROKE[toneFor(a.state)]"
                stroke-width="2"
                :stroke-dasharray="a.deterministic ? '2 3' : undefined"
              />
              <text
                :x="placed.get(a.key)!.x + 8"
                :y="placed.get(a.key)!.y + 19"
                class="fill-current text-xs font-medium"
              >
                {{ clip(actionTitle(a), 28) }}
              </text>
              <text
                :x="placed.get(a.key)!.x + 8"
                :y="placed.get(a.key)!.y + 37"
                class="text-xs"
                :class="TEXT[toneFor(a.state)]"
              >
                {{ a.state
                }}{{ a.runs.length > 1 ? ` · ${a.runs.length} attempts` : '' }}
              </text>
            </g>
          </g>
        </svg>
      </div>
      <ul
        class="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-ccf-600 dark:text-ccf-400"
        aria-label="Legend"
      >
        <li>○ an event</li>
        <li>▭ solid: a dispatch (an agent task)</li>
        <li>┈ dotted: a deterministic action</li>
        <li>
          → caused: a run's completion (task provenance, run id), or the object
          an effect created (the event names the effect)
        </li>
        <li>
          ⇢ dashed: inferred, where no effect is named (same object, later, by
          the factory)
        </li>
      </ul>
    </template>
  </section>
</template>
