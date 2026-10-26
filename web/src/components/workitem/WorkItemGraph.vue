<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import type { Schemas } from '@/api/client';
import { formatTime, toneOf, type Tone } from '@/components/shared/format';
import {
  graphOf,
  withoutIdle,
  type GraphAction,
  type GraphTask,
} from './graph';
import { ACT_H, ACT_W, R, layoutGraph } from './graphLayout';
import {
  ZOOM_STEP,
  fitWidth,
  panBy,
  wheelZoom,
  zoomAt,
  type View,
} from './panZoom';

/**
 * The work item as a hand-laid graph, left to right in time: a column per event (a circle), the
 * actions its fired decisions took on the rows below it (rectangles; dotted when deterministic, solid for a
 * dispatch), each arrow labelled with its rule, each dispatch's task beside it (coloured by its
 * phase), and links to what they caused (dashed when inferred). Each event sits on the row of
 * what caused it, so that arrow is a straight line (graphLayout.ts lays it out). Every node is a
 * button: an action opens its decision's "why", a task its
 * entry in the LLM tasks tab, an event its row in the timeline (where an admin can replay it). A
 * replay's decisions say so on their rule. Events no decision acted on and
 * nothing points to are left out (the Timeline lists them).
 *
 * The graph sits in a viewport you can pan (drag, wheel, arrow keys) and zoom (⌘/Ctrl+wheel,
 * pinch, + and -, the corner buttons); 0 and "Fit" fit its width, never above 1×. A press that
 * moves less than a few pixels is still a click on the node under it.
 */
const props = defineProps<{ detail: Schemas['WorkItemDetail'] }>();
const emit = defineEmits<{
  why: [index: number];
  event: [key: string];
  task: [id: string];
  timeline: [];
}>();

const visible = computed(() => withoutIdle(graphOf(props.detail)));
const graph = computed(() => visible.value.graph);
const hidden = computed(() => visible.value.hidden);
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

/** How far a press must move before it pans rather than clicks. */
const DRAG_SLOP = 4;
const KEY_PAN = 40;

const viewport = ref<HTMLElement | null>(null);
const viewWidth = ref(0);
const view = ref<View>({ x: 0, y: 0, k: 1 });
const panning = ref(false);

const fitted = computed(() => fitWidth(width.value, viewWidth.value));
/** The viewport's height: the fitted content's, up to 70% of the window. */
const viewportHeight = computed(
  () => `min(${Math.ceil(height.value * fitted.value.k)}px, 70vh)`,
);
const transform = computed(
  () => `translate(${view.value.x} ${view.value.y}) scale(${view.value.k})`,
);

function fit(): void {
  view.value = { ...fitted.value };
}
function actualSize(): void {
  view.value = { x: 0, y: 0, k: 1 };
}
function zoomCentre(factor: number): void {
  const el = viewport.value;
  view.value = zoomAt(
    view.value,
    factor,
    (el?.clientWidth ?? 0) / 2,
    (el?.clientHeight ?? 0) / 2,
  );
}

let observer: ResizeObserver | undefined;
function measure(): void {
  viewWidth.value = viewport.value?.clientWidth ?? 0;
}
onMounted(() => {
  measure();
  fit();
  if (typeof ResizeObserver !== 'undefined' && viewport.value) {
    observer = new ResizeObserver(measure);
    observer.observe(viewport.value);
  }
});
onBeforeUnmount(() => observer?.disconnect());
// A new graph (another work item, or an update) starts fitted again.
watch(width, () => fit());

let press: { id: number; x: number; y: number; view: View } | undefined;
let dragged = false;

function onPointerDown(e: PointerEvent): void {
  if (e.button !== 0) return;
  press = { id: e.pointerId, x: e.clientX, y: e.clientY, view: view.value };
  dragged = false;
}
function onPointerMove(e: PointerEvent): void {
  if (!press || e.pointerId !== press.id) return;
  const dx = e.clientX - press.x;
  const dy = e.clientY - press.y;
  if (!panning.value) {
    if (Math.hypot(dx, dy) < DRAG_SLOP) return;
    // Only now: capturing on the press would retarget the click away from the node.
    panning.value = true;
    dragged = true;
    viewport.value?.setPointerCapture?.(e.pointerId);
  }
  view.value = panBy(press.view, dx, dy);
}
function onPointerUp(e: PointerEvent): void {
  if (!press || e.pointerId !== press.id) return;
  if (panning.value) viewport.value?.releasePointerCapture?.(e.pointerId);
  press = undefined;
  panning.value = false;
}
/** A drag that panned is not a click on the node it started on. */
function onClickCapture(e: MouseEvent): void {
  if (!dragged) return;
  dragged = false;
  e.stopPropagation();
  e.preventDefault();
}

function onWheel(e: WheelEvent): void {
  e.preventDefault();
  if (e.ctrlKey || e.metaKey) {
    const r = viewport.value!.getBoundingClientRect();
    view.value = zoomAt(
      view.value,
      wheelZoom(e.deltaY),
      e.clientX - r.left,
      e.clientY - r.top,
    );
    return;
  }
  const dx = e.shiftKey && !e.deltaX ? e.deltaY : e.deltaX;
  const dy = e.shiftKey && !e.deltaX ? 0 : e.deltaY;
  view.value = panBy(view.value, -dx, -dy);
}

function onKeydown(e: KeyboardEvent): void {
  if (e.target !== e.currentTarget) return;
  const keys: Record<string, () => void> = {
    '+': () => zoomCentre(ZOOM_STEP),
    '=': () => zoomCentre(ZOOM_STEP),
    '-': () => zoomCentre(1 / ZOOM_STEP),
    '0': fit,
    ArrowLeft: () => (view.value = panBy(view.value, KEY_PAN, 0)),
    ArrowRight: () => (view.value = panBy(view.value, -KEY_PAN, 0)),
    ArrowUp: () => (view.value = panBy(view.value, 0, KEY_PAN)),
    ArrowDown: () => (view.value = panBy(view.value, 0, -KEY_PAN)),
  };
  const fn = keys[e.key];
  if (!fn) return;
  e.preventDefault();
  fn();
}

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

/** The task's tooltip: its label, and the dispatcher's sentence for why it waits. */
function taskTitle(t: GraphTask): string {
  const detail = t.reason ? t.task.status?.reasonDetail : undefined;
  return detail ? `${taskLabel(t)}\n${detail}` : taskLabel(t);
}

/** The rule above an action, marked when an admin's replay recorded its decision. */
function ruleText(a: GraphAction): string {
  return a.decision.replay ? `${a.rule} (replay)` : a.rule;
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
        ref="viewport"
        class="relative touch-none overflow-hidden rounded-md border border-ccf-200 select-none focus:outline-none focus-visible:ring-2 focus-visible:ring-sky-400 dark:border-ccf-800"
        :class="panning ? 'cursor-grabbing' : 'cursor-grab'"
        :style="{ height: viewportHeight }"
        tabindex="0"
        role="region"
        aria-label="Graph viewport: drag or scroll to pan, ⌘ or Ctrl and scroll to zoom; + and - zoom, the arrow keys pan, 0 fits"
        data-testid="graph-viewport"
        @pointerdown="onPointerDown"
        @pointermove="onPointerMove"
        @pointerup="onPointerUp"
        @pointercancel="onPointerUp"
        @click.capture="onClickCapture"
        @wheel="onWheel"
        @keydown="onKeydown"
      >
        <div
          class="absolute top-2 right-2 z-10 flex gap-1"
          data-testid="graph-controls"
        >
          <button
            v-for="c in [
              { label: 'Zoom in', text: '+', fn: () => zoomCentre(ZOOM_STEP) },
              {
                label: 'Zoom out',
                text: '−',
                fn: () => zoomCentre(1 / ZOOM_STEP),
              },
              { label: 'Fit the graph to the view', text: 'Fit', fn: fit },
              { label: 'Actual size', text: '100%', fn: actualSize },
            ]"
            :key="c.label"
            type="button"
            class="min-w-7 cursor-pointer rounded border border-ccf-300 bg-white px-2 py-0.5 text-xs text-ccf-700 hover:bg-ccf-100 dark:border-ccf-700 dark:bg-ccf-900 dark:text-ccf-200 dark:hover:bg-ccf-800"
            :aria-label="c.label"
            :title="c.label"
            @pointerdown.stop
            @click="c.fn"
          >
            {{ c.text }}
          </button>
        </div>
        <svg
          width="100%"
          height="100%"
          role="group"
          aria-label="Events, the actions their rules took, their tasks, and what those caused"
          class="text-ccf-700 dark:text-ccf-300"
          :data-zoom="view.k"
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
          <g :transform="transform" data-testid="graph-content">
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
                :aria-label="`${actionTitle(a)}: ${a.state}, by ${ruleText(a)}. Why?`"
                :data-kind="a.kind"
                :data-state="a.state"
                data-testid="graph-action"
                @click="emit('why', a.decisionIndex)"
                @keydown="activate($event, () => emit('why', a.decisionIndex))"
              >
                <title>{{ actionTitle(a) }} ({{ ruleText(a) }})</title>
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
                  {{ clip(ruleText(a), 34) }}
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
                  }}{{
                    a.runs.length > 1 ? ` · ${a.runs.length} attempts` : ''
                  }}
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
              <title>{{ taskTitle(t.node) }}</title>
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
          </g>
        </svg>
      </div>
      <p
        v-if="hidden"
        class="mt-2 text-xs text-ccf-600 dark:text-ccf-400"
        data-testid="graph-hidden"
      >
        {{ hidden }} {{ hidden === 1 ? 'event' : 'events' }} with no decisions
        hidden;
        <button
          type="button"
          class="cursor-pointer underline"
          @click="emit('timeline')"
        >
          see the Timeline
        </button>
      </p>
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
