<script setup lang="ts">
import type { Schemas } from '@/api/client';
import PauseControls from '@/components/controls/PauseControls.vue';
import StatusPill from '@/components/shared/StatusPill.vue';
import { formatTime, type Tone } from '@/components/shared/format';

/**
 * The providers and their health, with pause and resume per provider. Only names, types, labels
 * and scope are shown: a provider's spec also names its secrets, which never reach the page.
 */
defineProps<{ providers: Schemas['ProviderView'][] }>();

const HEALTH: Record<Schemas['ProviderHealth']['status'], Tone> = {
  ok: 'success',
  degraded: 'warn',
  down: 'danger',
  unknown: 'neutral',
};

function scopeOf(p: Schemas['Provider']): string {
  const s = p.spec.scope;
  return (
    [
      ...(s?.orgs ?? []).map((o) => `org ${o}`),
      ...(s?.channels ?? []).map((c) => `#${c}`),
    ].join(', ') || 'everything it can see'
  );
}

function labelsOf(p: Schemas['Provider']): string {
  return Object.entries(p.metadata.labels ?? {})
    .map(([k, v]) => `${k}=${v}`)
    .join(', ');
}
</script>

<template>
  <div class="overflow-x-auto">
    <table class="w-full text-left text-sm">
      <thead class="text-ccf-600 dark:text-ccf-400">
        <tr>
          <th scope="col" class="py-1 pr-4 font-medium">Provider</th>
          <th scope="col" class="py-1 pr-4 font-medium">Type</th>
          <th scope="col" class="py-1 pr-4 font-medium">Labels</th>
          <th scope="col" class="py-1 pr-4 font-medium">Scope</th>
          <th scope="col" class="py-1 pr-4 font-medium">Health</th>
          <th scope="col" class="py-1 font-medium">
            <span class="sr-only">Controls</span>
          </th>
        </tr>
      </thead>
      <tbody>
        <tr
          v-for="view in providers"
          :key="view.provider.metadata.name"
          class="border-t border-ccf-200 align-top dark:border-ccf-800"
          data-testid="provider"
        >
          <th scope="row" class="py-2 pr-4 font-mono text-xs font-medium">
            {{ view.provider.metadata.name }}
          </th>
          <td class="py-2 pr-4">{{ view.provider.spec.type }}</td>
          <td class="py-2 pr-4 font-mono text-xs">
            {{ labelsOf(view.provider) }}
          </td>
          <td class="py-2 pr-4">{{ scopeOf(view.provider) }}</td>
          <td class="py-2 pr-4" data-testid="provider-health">
            <StatusPill
              :label="view.health.status"
              :tone="HEALTH[view.health.status] ?? 'neutral'"
            />
            <!-- Provider text: rendered as text. -->
            <p
              v-if="view.health.message"
              class="mt-1 text-xs text-ccf-600 dark:text-ccf-400"
            >
              {{ view.health.message }}
            </p>
            <p
              v-if="view.health.checkedAt"
              class="mt-1 text-xs text-ccf-500 dark:text-ccf-400"
            >
              checked
              <time :datetime="view.health.checkedAt">{{
                formatTime(view.health.checkedAt)
              }}</time>
            </p>
          </td>
          <td class="py-2">
            <PauseControls
              scope="provider"
              :name="view.provider.metadata.name"
            />
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
