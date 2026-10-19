<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import type { Schemas } from '@/api/client';
import { titleOf } from '@/components/workitem/board';
import { formatTime } from '@/components/shared/format';
import { useCatalogStore } from '@/stores/catalog';
import Button from '@/volt/Button.vue';
import InputText from '@/volt/InputText.vue';
import Message from '@/volt/Message.vue';
import Select from '@/volt/Select.vue';
import Textarea from '@/volt/Textarea.vue';
import EvaluationView from './EvaluationView.vue';
import { eventLabel } from './catalog';

/**
 * Evaluates an event against the loaded catalog (`POST /catalog/dry-run`): a pasted event, or one
 * recorded on a work item, optionally against that work item's state. Nothing is stored and no
 * action is applied, so readers may use it too.
 */
const props = defineProps<{ catalogSha?: string }>();

const store = useCatalogStore();

const source = ref('');
const workItem = ref('');
const running = ref(false);
const parseError = ref<string | null>(null);
const failure = ref<{
  message: string;
  fields: Schemas['FieldError'][];
} | null>(null);
const evaluation = ref<Schemas['Evaluation'] | null>(null);

const workItems = ref<Schemas['WorkItem'][]>([]);
const workItemsError = ref<string | null>(null);
const picked = ref('');
const events = ref<Schemas['Event'][]>([]);
const eventsError = ref<string | null>(null);
const pickedEvent = ref('');

const workItemOptions = computed(() =>
  workItems.value.map((w) => ({
    label: titleOf(w),
    value: w.metadata.name,
  })),
);
const eventOptions = computed(() =>
  events.value.map((e) => ({
    label: eventLabel(e, formatTime(e.time)),
    value: e.id,
  })),
);

onMounted(async () => {
  const result = await store.recentWorkItems();
  if ('items' in result) workItems.value = result.items;
  else workItemsError.value = result.error;
});

async function pickWorkItem(value: unknown): Promise<void> {
  picked.value = typeof value === 'string' ? value : '';
  pickedEvent.value = '';
  events.value = [];
  eventsError.value = null;
  if (!picked.value) return;
  const result = await store.recordedEvents(picked.value);
  if ('events' in result) events.value = result.events;
  else eventsError.value = result.error;
}

function pickEvent(value: unknown): void {
  pickedEvent.value = typeof value === 'string' ? value : '';
  const event = events.value.find((e) => e.id === pickedEvent.value);
  if (!event) return;
  source.value = JSON.stringify(event, null, 2);
  workItem.value = picked.value;
}

/** The pasted event, or why it isn't one. */
function parse(): Schemas['Event'] | string {
  if (!source.value.trim()) return 'Paste an event, or pick a recorded one.';
  let value: unknown;
  try {
    value = JSON.parse(source.value);
  } catch (e) {
    return `Not JSON: ${e instanceof Error ? e.message : String(e)}`;
  }
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    return 'The event must be a JSON object.';
  }
  // The server validates the rest and says which field is wrong.
  return value as Schemas['Event'];
}

async function run(): Promise<void> {
  failure.value = null;
  const event = parse();
  if (typeof event === 'string') {
    parseError.value = event;
    return;
  }
  parseError.value = null;
  running.value = true;
  try {
    const request: Schemas['DryRunRequest'] = { event };
    const item = workItem.value.trim();
    if (item) request.workItem = item;
    const outcome = await store.dryRun(request);
    if (outcome.ok) {
      evaluation.value = outcome.evaluation;
    } else {
      evaluation.value = null;
      failure.value = { message: outcome.message, fields: outcome.fields };
    }
  } finally {
    running.value = false;
  }
}
</script>

<template>
  <section aria-labelledby="dry-run-heading" data-testid="dry-run">
    <h2 id="dry-run-heading" class="mb-1 text-lg font-semibold">Dry run</h2>
    <p class="mb-4 text-sm text-ccf-600 dark:text-ccf-400">
      What the rules would do with an event, against the loaded catalog. Nothing
      is stored and no action is applied.
    </p>

    <div class="mb-4 flex flex-wrap items-end gap-4">
      <div class="flex flex-col gap-1">
        <span
          id="dry-run-workitem-pick"
          class="text-xs font-medium text-ccf-600 dark:text-ccf-400"
          >Recorded on work item</span
        >
        <Select
          :model-value="picked"
          :options="workItemOptions"
          option-label="label"
          option-value="value"
          placeholder="Pick a work item"
          aria-labelledby="dry-run-workitem-pick"
          data-testid="pick-workitem"
          class="min-w-64"
          @update:model-value="pickWorkItem"
        />
      </div>
      <div class="flex flex-col gap-1">
        <span
          id="dry-run-event-pick"
          class="text-xs font-medium text-ccf-600 dark:text-ccf-400"
          >Event</span
        >
        <Select
          :model-value="pickedEvent"
          :options="eventOptions"
          option-label="label"
          option-value="value"
          placeholder="Pick an event"
          :disabled="!events.length"
          aria-labelledby="dry-run-event-pick"
          data-testid="pick-event"
          class="min-w-80"
          @update:model-value="pickEvent"
        />
      </div>
    </div>
    <Message v-if="workItemsError" severity="warn" class="mb-4" role="status">
      Couldn't list work items to pick an event from: {{ workItemsError }}
    </Message>
    <Message v-if="eventsError" severity="error" class="mb-4" role="alert">
      Couldn't load the work item's events: {{ eventsError }}
    </Message>

    <form class="flex flex-col gap-3" @submit.prevent="run">
      <label class="flex flex-col gap-1 text-sm">
        <span class="text-xs font-medium text-ccf-600 dark:text-ccf-400"
          >Event JSON</span
        >
        <Textarea
          v-model="source"
          rows="12"
          spellcheck="false"
          class="font-mono text-xs"
          data-testid="dry-run-event"
        />
      </label>
      <label class="flex flex-col gap-1 text-sm">
        <span class="text-xs font-medium text-ccf-600 dark:text-ccf-400"
          >Against work item (optional; an empty one otherwise)</span
        >
        <InputText
          v-model="workItem"
          placeholder="wi_…"
          class="max-w-md font-mono text-xs"
          data-testid="dry-run-workitem"
        />
      </label>
      <div>
        <Button
          type="submit"
          label="Evaluate"
          :loading="running"
          data-testid="dry-run-submit"
        />
      </div>
    </form>

    <Message
      v-if="parseError"
      severity="error"
      class="mt-4"
      role="alert"
      data-testid="dry-run-error"
    >
      {{ parseError }}
    </Message>
    <Message
      v-if="failure"
      severity="error"
      class="mt-4"
      role="alert"
      data-testid="dry-run-error"
    >
      Couldn't evaluate the event: {{ failure.message }}
      <ul v-if="failure.fields.length" class="mt-1 list-disc ps-5">
        <li v-for="f in failure.fields" :key="f.field">
          <code class="text-xs">{{ f.field }}</code
          >: {{ f.message }}
        </li>
      </ul>
    </Message>

    <EvaluationView
      v-if="evaluation"
      class="mt-6"
      :evaluation="evaluation"
      :catalog-sha="props.catalogSha"
    />
  </section>
</template>
