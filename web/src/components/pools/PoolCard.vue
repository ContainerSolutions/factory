<script setup lang="ts">
import { computed } from 'vue';
import StatusPill from '@/components/shared/StatusPill.vue';
import AgentCard from './AgentCard.vue';
import type { PoolGroup } from './pools';

const props = defineProps<{ group: PoolGroup; now: Date }>();

const spec = computed(() => props.group.pool?.spec);
const headingId = computed(() => `pool-${props.group.name}`);
const lease = computed(() => {
  const l = spec.value?.lease;
  if (!l) return '';
  return [
    l.ttl && `ttl ${l.ttl}`,
    l.heartbeat && `heartbeat every ${l.heartbeat}`,
    l.maxTasksPerAgent !== undefined && `${l.maxTasksPerAgent} per agent`,
  ]
    .filter(Boolean)
    .join(' · ');
});
const backend = computed(() => {
  const b = spec.value?.backend;
  if (!b) return '';
  const gha = b.githubActions;
  return gha ? `${b.type}: ${gha.repo} ${gha.workflow}` : b.type;
});
</script>

<template>
  <section
    :aria-labelledby="headingId"
    class="rounded-lg border border-ccf-200 bg-ccf-100/50 p-4 dark:border-ccf-800 dark:bg-ccf-900/50"
    data-testid="pool"
  >
    <div class="mb-2 flex flex-wrap items-center gap-2">
      <h3 :id="headingId" class="font-mono font-semibold">{{ group.name }}</h3>
      <StatusPill
        v-if="spec"
        :label="spec.mode"
        tone="neutral"
        data-testid="pool-mode"
      />
      <StatusPill
        v-else
        label="not listed"
        tone="warn"
        data-testid="pool-mode"
      />
    </div>

    <!-- The admission token and the LLM API key are secret refs: never shown. -->
    <dl
      v-if="spec"
      class="mb-3 grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm"
    >
      <template v-if="spec.admission">
        <dt class="text-ccf-600 dark:text-ccf-400">Owners</dt>
        <dd>members of the Group {{ spec.admission.owners }}</dd>
      </template>
      <template v-if="lease">
        <dt class="text-ccf-600 dark:text-ccf-400">Lease</dt>
        <dd>{{ lease }}</dd>
      </template>
      <template v-if="spec.require?.harnesses?.length">
        <dt class="text-ccf-600 dark:text-ccf-400">Requires</dt>
        <dd>any of {{ spec.require.harnesses.join(', ') }}</dd>
      </template>
      <template v-if="spec.fairness">
        <dt class="text-ccf-600 dark:text-ccf-400">Fairness</dt>
        <dd>{{ spec.fairness }}</dd>
      </template>
      <template v-if="backend">
        <dt class="text-ccf-600 dark:text-ccf-400">Backend</dt>
        <dd>{{ backend }}</dd>
      </template>
      <template v-if="spec.image">
        <dt class="text-ccf-600 dark:text-ccf-400">Image</dt>
        <dd class="font-mono text-xs break-all">{{ spec.image }}</dd>
      </template>
      <template v-if="spec.scaling">
        <dt class="text-ccf-600 dark:text-ccf-400">Scaling</dt>
        <dd>{{ spec.scaling.min }} to {{ spec.scaling.max }} agents</dd>
      </template>
    </dl>
    <p v-else class="mb-3 text-sm text-ccf-600 dark:text-ccf-400">
      Agents name this pool, but it isn't listed.
    </p>

    <div v-if="group.agents.length" class="grid gap-3 xl:grid-cols-2">
      <AgentCard
        v-for="agent in group.agents"
        :key="agent.metadata.name"
        :agent="agent"
        :pool="group.pool"
        :now="now"
      />
    </div>
    <p v-else class="text-sm text-ccf-500 dark:text-ccf-400">No agents.</p>
    <p
      v-if="group.missing.length"
      class="mt-2 text-xs text-ccf-600 dark:text-ccf-400"
      data-testid="pool-missing"
    >
      Listed in the pool but not returned by the agents list:
      <code v-for="n in group.missing" :key="n" class="mr-1">{{ n }}</code>
    </p>
  </section>
</template>
