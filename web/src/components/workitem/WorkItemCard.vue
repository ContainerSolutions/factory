<script setup lang="ts">
import { computed } from 'vue';
import { RouterLink } from 'vue-router';
import type { Schemas } from '@/api/client';
import StatusPill from '@/components/shared/StatusPill.vue';
import { formatTime, formatValue, toneOf } from '@/components/shared/format';
import {
  closeReasonText,
  lastActivity,
  primaryLink,
  stateOf,
  titleOf,
} from './board';

const props = defineProps<{ item: Schemas['WorkItem'] }>();

const link = computed(() => primaryLink(props.item));
const closed = computed(() => props.item.status?.closed);
const state = computed(() => Object.entries(stateOf(props.item)));
const at = computed(() => lastActivity(props.item));
const ruleSets = computed(() => [
  ...new Set(
    (props.item.status?.decisions ?? [])
      .map((d) => d.ruleSet)
      .filter((v): v is string => !!v),
  ),
]);
const pendingApprovals = computed(() =>
  (props.item.status?.approvals ?? []).filter((a) => a.phase === 'Pending'),
);
</script>

<template>
  <article
    class="rounded-md border border-ccf-200 bg-white p-3 shadow-xs dark:border-ccf-800 dark:bg-ccf-900"
    data-testid="workitem-card"
  >
    <h3 class="text-sm font-semibold break-all">
      <RouterLink
        :to="{ name: 'workitem', params: { id: item.metadata.name } }"
        class="hover:underline focus-visible:outline-2 focus-visible:outline-offset-2"
      >
        {{ titleOf(item) }}
      </RouterLink>
    </h3>
    <p
      v-if="link"
      class="mt-1 text-xs text-ccf-600 dark:text-ccf-400"
      data-testid="card-provider"
    >
      {{ link.provider
      }}<template v-if="link.kind"> · {{ link.kind }}</template>
    </p>
    <ul
      v-if="closed || state.length || pendingApprovals.length"
      class="mt-2 flex flex-wrap gap-1"
      aria-label="State"
    >
      <li v-if="closed" data-testid="card-closed">
        <StatusPill
          :label="`Closed: ${closeReasonText(closed.reason)}`"
          tone="neutral"
        />
      </li>
      <li v-for="[key, value] in state" :key="key">
        <StatusPill
          :label="`${key}: ${formatValue(value)}`"
          :tone="toneOf(formatValue(value))"
        />
      </li>
      <li v-for="a in pendingApprovals" :key="`approval/${a.name}`">
        <StatusPill :label="`approval ${a.name}: ${a.phase}`" tone="warn" />
      </li>
    </ul>
    <p
      v-if="ruleSets.length || at"
      class="mt-2 text-xs text-ccf-500 dark:text-ccf-400"
    >
      <span v-if="ruleSets.length">{{ ruleSets.join(', ') }}</span>
      <template v-if="ruleSets.length && at"> · </template>
      <time v-if="at" :datetime="at.toISOString()">{{ formatTime(at) }}</time>
    </p>
  </article>
</template>
