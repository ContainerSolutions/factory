<script setup lang="ts">
import { computed } from 'vue';
import type { Schemas } from '@/api/client';
import StatusPill from '@/components/shared/StatusPill.vue';
import { formatTime, formatValue, toneOf } from '@/components/shared/format';
import { laneOf, LANES, stateOf } from './board';

const props = defineProps<{ item: Schemas['WorkItem'] }>();

const links = computed(() => props.item.spec?.links ?? []);
const state = computed(() => Object.entries(stateOf(props.item)));
const counters = computed(() =>
  Object.entries(props.item.status?.counters ?? {}),
);
const approvals = computed(() => props.item.status?.approvals ?? []);
const reporter = computed(() => props.item.status?.reporter);
const lane = computed(() => {
  const id = laneOf(props.item);
  return { id, title: LANES.find((l) => l.id === id)!.title };
});
</script>

<template>
  <section
    aria-label="Work item summary"
    class="mb-6 grid gap-6 rounded-lg border border-ccf-200 p-4 md:grid-cols-2 dark:border-ccf-800"
  >
    <div>
      <h2 class="mb-2 text-sm font-semibold">Links</h2>
      <ul
        v-if="links.length"
        class="flex flex-col gap-1 text-sm"
        data-testid="links"
      >
        <li
          v-for="link in links"
          :key="`${link.provider}/${link.ref}`"
          class="flex flex-wrap items-center gap-2"
        >
          <span class="font-medium">{{ link.provider }}</span>
          <span v-if="link.kind" class="text-ccf-600 dark:text-ccf-400">{{
            link.kind
          }}</span>
          <span class="font-mono text-xs break-all">{{ link.ref }}</span>
          <StatusPill v-if="link.primary" label="primary" />
        </li>
      </ul>
      <p v-else class="text-sm text-ccf-500">No links.</p>

      <h2 class="mt-4 mb-2 text-sm font-semibold">Details</h2>
      <dl class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
        <dt class="text-ccf-600 dark:text-ccf-400">Board lane</dt>
        <dd data-testid="lane">
          <StatusPill :label="lane.title" :tone="toneOf(lane.id)" />
        </dd>
        <dt class="text-ccf-600 dark:text-ccf-400">Name</dt>
        <dd class="font-mono text-xs break-all">{{ item.metadata.name }}</dd>
        <template v-if="item.metadata.createdAt">
          <dt class="text-ccf-600 dark:text-ccf-400">Created</dt>
          <dd>
            <time :datetime="item.metadata.createdAt">{{
              formatTime(item.metadata.createdAt)
            }}</time>
          </dd>
        </template>
        <template v-if="reporter">
          <dt class="text-ccf-600 dark:text-ccf-400">Reporter</dt>
          <dd>
            {{ reporter.id
            }}<template v-if="reporter.tier"> ({{ reporter.tier }})</template>
          </dd>
        </template>
      </dl>
    </div>

    <div>
      <h2 class="mb-2 text-sm font-semibold">State keys</h2>
      <dl
        v-if="state.length"
        class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm"
        data-testid="state-keys"
      >
        <template v-for="[key, value] in state" :key="key">
          <dt class="font-mono text-xs leading-6">{{ key }}</dt>
          <dd>
            <StatusPill v-if="typeof value === 'string'" :label="value" />
            <code v-else class="text-xs break-all">{{
              formatValue(value)
            }}</code>
          </dd>
        </template>
      </dl>
      <p v-else class="text-sm text-ccf-500">No state set.</p>

      <template v-if="counters.length">
        <h2 class="mt-4 mb-2 text-sm font-semibold">Runs per TaskSpec</h2>
        <dl
          class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm"
          data-testid="counters"
        >
          <template v-for="[spec, count] in counters" :key="spec">
            <dt class="font-mono text-xs leading-5">{{ spec }}</dt>
            <dd>{{ count }}</dd>
          </template>
        </dl>
      </template>

      <template v-if="approvals.length">
        <h2 class="mt-4 mb-2 text-sm font-semibold">Approvals</h2>
        <ul class="flex flex-col gap-1 text-sm" data-testid="approvals">
          <li
            v-for="a in approvals"
            :key="a.name"
            class="flex items-center gap-2"
          >
            <span class="font-mono text-xs">{{ a.name }}</span>
            <StatusPill :label="a.phase" />
          </li>
        </ul>
      </template>
    </div>
  </section>
</template>
