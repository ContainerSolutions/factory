<script setup lang="ts">
import type { Schemas } from '@/api/client';

/** The TaskSpecs: what each does, its default and allowed harnesses, its effects and limits. */
defineProps<{ taskSpecs: Schemas['TaskSpec'][] }>();

function model(m: Schemas['StringOrList'] | undefined): string {
  return Array.isArray(m) ? m.join(' | ') : (m ?? '');
}

function harnessOf(t: Schemas['TaskSpec']): string {
  const d = t.spec.harness?.default;
  if (!d) return 'the agent picks';
  const m = model(d.config?.model);
  return m ? `${d.name} (${m})` : d.name;
}

function allowedOf(t: Schemas['TaskSpec']): string {
  return (t.spec.harness?.allowed ?? []).map((h) => h.name).join(', ');
}

function limitsOf(t: Schemas['TaskSpec']): string {
  const l = t.spec.limits;
  if (!l) return '';
  return [
    l.maxTurns !== undefined ? `${l.maxTurns} turns` : '',
    l.maxWallClock ? `${l.maxWallClock}` : '',
    l.maxCostUSD !== undefined ? `$${l.maxCostUSD.toFixed(2)}` : '',
  ]
    .filter(Boolean)
    .join(', ');
}
</script>

<template>
  <div class="overflow-x-auto">
    <table class="w-full text-left text-sm">
      <thead class="text-ccf-600 dark:text-ccf-400">
        <tr>
          <th scope="col" class="py-1 pr-4 font-medium">TaskSpec</th>
          <th scope="col" class="py-1 pr-4 font-medium">Harness</th>
          <th scope="col" class="py-1 pr-4 font-medium">Effects</th>
          <th scope="col" class="py-1 pr-4 font-medium">Limits</th>
          <th scope="col" class="py-1 font-medium">Outcomes</th>
        </tr>
      </thead>
      <tbody>
        <tr
          v-for="t in taskSpecs"
          :key="t.metadata.name"
          class="border-t border-ccf-200 align-top dark:border-ccf-800"
          data-testid="taskspec"
        >
          <th scope="row" class="py-2 pr-4 font-medium">
            <span class="font-mono text-xs">{{ t.metadata.name }}</span>
            <p
              v-if="t.spec.description"
              class="mt-1 font-normal text-ccf-600 dark:text-ccf-400"
            >
              {{ t.spec.description }}
            </p>
          </th>
          <td class="py-2 pr-4" data-testid="taskspec-harness">
            {{ harnessOf(t) }}
            <p
              v-if="allowedOf(t)"
              class="mt-1 text-xs text-ccf-600 dark:text-ccf-400"
            >
              allowed: {{ allowedOf(t) }}
            </p>
          </td>
          <td class="py-2 pr-4 font-mono text-xs leading-5">
            {{ (t.spec.effects?.allow ?? []).join(', ') || 'none' }}
          </td>
          <td class="py-2 pr-4">{{ limitsOf(t) || 'none' }}</td>
          <td class="py-2 font-mono text-xs leading-5">
            {{ (t.spec.output?.outcomes ?? []).join(', ') }}
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
