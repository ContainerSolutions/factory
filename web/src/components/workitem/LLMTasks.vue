<script setup lang="ts">
import { computed } from 'vue';
import { RouterLink } from 'vue-router';
import type { Schemas } from '@/api/client';
import { runStateOf } from '@/components/run/run';
import StatusPill from '@/components/shared/StatusPill.vue';
import { formatTime } from '@/components/shared/format';
import { llmTasksOf } from './agentTasks';

/**
 * Every agent task of the work item, in order: TaskSpec, inputs, harness, attempts, phase and
 * pending reason, outcome and summary, links to each run, its transcript and what it produced.
 * The tasks still in flight are highlighted.
 */
const props = defineProps<{ detail: Schemas['WorkItemDetail'] }>();

const tasks = computed(() => llmTasksOf(props.detail));
</script>

<template>
  <section aria-label="LLM tasks">
    <ol v-if="tasks.length" class="flex flex-col gap-3">
      <li
        v-for="(t, i) in tasks"
        :key="t.task.metadata.id"
        class="rounded-lg border p-4 text-sm"
        :class="
          t.inFlight
            ? 'border-sky-500 bg-sky-50 ring-2 ring-sky-300 dark:border-sky-400 dark:bg-sky-950 dark:ring-sky-800'
            : 'border-ccf-200 dark:border-ccf-800'
        "
        :data-in-flight="t.inFlight"
        data-testid="llm-task"
      >
        <div class="mb-2 flex flex-wrap items-center gap-2">
          <span class="text-xs text-ccf-500">{{ i + 1 }}.</span>
          <h3 class="font-mono font-semibold">
            {{ t.task.spec.taskSpec.name }}
          </h3>
          <StatusPill :label="t.task.status?.phase ?? 'Pending'" />
          <StatusPill v-if="t.inFlight" label="in flight" tone="info" />
          <StatusPill
            v-if="t.outcome"
            :label="`outcome: ${t.outcome}`"
            tone="neutral"
          />
          <time
            v-if="t.task.metadata.createdAt"
            :datetime="t.task.metadata.createdAt"
            class="ms-auto text-xs text-ccf-600 dark:text-ccf-400"
            >{{ formatTime(t.task.metadata.createdAt) }}</time
          >
        </div>
        <p
          v-if="t.task.status?.reason"
          class="mb-2 text-amber-800 dark:text-amber-300"
          data-testid="llm-task-reason"
        >
          {{ t.task.status.phase === 'Pending' ? 'Waiting: ' : ''
          }}{{ t.task.status.reason }}
        </p>
        <p v-if="t.summary" class="mb-2" data-testid="llm-task-summary">
          {{ t.summary }}
        </p>
        <dl class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1">
          <dt class="text-ccf-600 dark:text-ccf-400">Inputs</dt>
          <dd class="font-mono text-xs leading-5" data-testid="llm-task-inputs">
            <span v-if="!t.inputs.length">none</span>
            <span v-for="[k, v] in t.inputs" :key="k" class="me-3 break-all"
              >{{ k }}: {{ v }}</span
            >
          </dd>
          <dt class="text-ccf-600 dark:text-ccf-400">Harness</dt>
          <dd data-testid="llm-task-harness">{{ t.harness }}</dd>
          <dt class="text-ccf-600 dark:text-ccf-400">Attempts</dt>
          <dd data-testid="llm-task-attempts">
            {{ t.task.status?.attempts ?? t.runs.length }}
          </dd>
          <template v-if="t.runs.length">
            <dt class="text-ccf-600 dark:text-ccf-400">Runs</dt>
            <dd>
              <ul class="flex flex-col gap-1" data-testid="llm-task-runs">
                <li
                  v-for="r in t.runs"
                  :key="r.metadata.name"
                  class="flex flex-wrap items-center gap-2"
                >
                  <RouterLink
                    :to="{ name: 'run', params: { id: r.metadata.name } }"
                    class="font-mono text-xs underline"
                    >attempt {{ r.spec.attempt }}</RouterLink
                  >
                  <StatusPill :label="runStateOf(r)" />
                  <RouterLink
                    v-if="r.status?.transcriptRef"
                    :to="{
                      name: 'run',
                      params: { id: r.metadata.name },
                      hash: '#transcript-heading',
                    }"
                    class="text-xs underline"
                    >transcript</RouterLink
                  >
                </li>
              </ul>
            </dd>
          </template>
          <template v-if="t.produced.length">
            <dt class="text-ccf-600 dark:text-ccf-400">Produced</dt>
            <dd>
              <ul class="flex flex-col gap-1" data-testid="llm-task-produced">
                <li v-for="p in t.produced" :key="p.url ?? p.label">
                  <a
                    v-if="p.url"
                    :href="p.url"
                    target="_blank"
                    rel="noopener noreferrer"
                    class="text-xs break-all underline"
                    >{{ p.label }}</a
                  >
                  <span v-else class="font-mono text-xs">{{ p.label }}</span>
                </li>
              </ul>
            </dd>
          </template>
        </dl>
      </li>
    </ol>
    <p v-else class="text-sm text-ccf-500 dark:text-ccf-400">
      No agent tasks yet.
    </p>
  </section>
</template>
