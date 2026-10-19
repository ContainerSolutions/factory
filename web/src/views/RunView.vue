<script setup lang="ts">
import { computed, watch } from 'vue';
import { RouterLink } from 'vue-router';
import { runTopic } from '@/api/stream';
import RunEffects from '@/components/run/RunEffects.vue';
import RunHarness from '@/components/run/RunHarness.vue';
import RunOverview from '@/components/run/RunOverview.vue';
import RunProgress from '@/components/run/RunProgress.vue';
import RunTranscript from '@/components/run/RunTranscript.vue';
import { isRunning } from '@/components/run/run';
import RunControls from '@/components/controls/RunControls.vue';
import JsonViewer from '@/components/shared/JsonViewer.vue';
import PageHeader from '@/components/shared/PageHeader.vue';
import { useStream } from '@/composables/useStream';
import { useRunsStore } from '@/stores/runs';
import Message from '@/volt/Message.vue';

const props = defineProps<{ id: string }>();

const store = useRunsStore();

watch(
  () => props.id,
  (id) => {
    store.loadRun(id);
  },
  { immediate: true },
);

const detail = computed(() => store.current);
const run = computed(() => detail.value?.run);
const running = computed(() => !!run.value && isRunning(run.value));

// Subscribed while the run loads (so nothing between the snapshot and the subscription is lost)
// and while it runs. The `runs/<id>` topic carries progress only, so the run is reloaded when its
// task changes, which is how its end, Result and usage arrive.
const topics = computed(() =>
  store.currentNotFound || store.currentError || (run.value && !running.value)
    ? []
    : [runTopic(props.id), 'tasks'],
);

const stream = useStream(
  topics,
  {
    progress: (progress) => store.addProgress(progress),
    'task.updated': (task) => {
      if (run.value && task.metadata.id === run.value.spec.task) {
        store.loadRun(props.id, { quiet: true });
      }
    },
  },
  // A re-created stream can't resume: reload the run's ring buffer for what was missed.
  { onResync: () => store.loadRun(props.id, { quiet: true }) },
);
</script>

<template>
  <nav aria-label="Breadcrumb" class="mb-2 text-sm">
    <RouterLink
      :to="{ name: 'board' }"
      class="text-ccf-600 hover:underline dark:text-ccf-400"
      >← Board</RouterLink
    >
  </nav>

  <template v-if="detail && run">
    <PageHeader title="Run">
      <code class="text-xs">{{ run.metadata.name }}</code>
      · attempt {{ run.spec.attempt }} of task
      <code class="text-xs">{{ run.spec.task }}</code>
    </PageHeader>
    <RunControls :run="run" :running="running" class="mb-6" />

    <div class="grid gap-6 lg:grid-cols-3">
      <div class="flex flex-col gap-6 lg:col-span-2">
        <RunProgress
          :events="store.progress"
          :running="running"
          :stream="stream.status.value"
        />
        <RunTranscript
          :available="!!run.status?.transcriptRef"
          :state="store.transcriptState"
          :entries="store.transcript"
          :error="store.transcriptError"
          @load="store.loadTranscript()"
        />
      </div>
      <aside
        class="flex flex-col gap-6 rounded-lg border border-ccf-200 p-4 dark:border-ccf-800"
        aria-label="Run details"
      >
        <RunOverview :run="run" />
        <RunHarness :run="run" />
        <RunEffects :effects="detail.effects" />
      </aside>
    </div>

    <div v-if="run.status?.result" class="mt-6">
      <h2 class="mb-3 text-lg font-semibold">Result JSON</h2>
      <JsonViewer :value="run.status.result" label="Result JSON" />
    </div>
  </template>

  <template v-else-if="store.currentNotFound">
    <PageHeader title="Run not found">
      There is no run <code class="text-xs">{{ id }}</code
      >.
    </PageHeader>
  </template>

  <template v-else-if="store.currentError">
    <PageHeader title="Run" />
    <Message severity="error" role="alert">
      Couldn't load the run: {{ store.currentError }}
    </Message>
  </template>

  <template v-else>
    <PageHeader title="Run" />
    <p class="text-sm text-ccf-600 dark:text-ccf-400" role="status">Loading…</p>
  </template>
</template>
