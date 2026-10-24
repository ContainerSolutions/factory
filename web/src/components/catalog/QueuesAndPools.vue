<script setup lang="ts">
import { RouterLink } from 'vue-router';
import type { Schemas } from '@/api/client';
import { budgetText } from '@/components/pools/pools';

/** The catalog's queues and pools as configured; the pools view has their live state. */
defineProps<{ queues: Schemas['Queue'][]; pools: Schemas['Pool'][] }>();
</script>

<template>
  <div class="grid gap-6 lg:grid-cols-2">
    <div>
      <h3 class="mb-2 text-sm font-semibold">Queues</h3>
      <div class="max-w-full overflow-x-auto">
        <table class="w-full text-left text-sm">
          <thead class="text-ccf-600 dark:text-ccf-400">
            <tr>
              <th scope="col" class="py-1 pr-4 font-medium">Queue</th>
              <th scope="col" class="py-1 pr-4 font-medium">Pool</th>
              <th scope="col" class="py-1 pr-4 font-medium">Concurrency</th>
              <th scope="col" class="py-1 pr-4 font-medium">Priority</th>
              <th scope="col" class="py-1 font-medium">Budget</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="q in queues"
              :key="q.metadata.name"
              class="border-t border-ccf-200 dark:border-ccf-800"
              data-testid="catalog-queue"
            >
              <th scope="row" class="py-1 pr-4 font-mono text-xs font-medium">
                {{ q.metadata.name }}
              </th>
              <td class="py-1 pr-4 font-mono text-xs">{{ q.spec.pool }}</td>
              <td class="py-1 pr-4">{{ q.spec.concurrency ?? '—' }}</td>
              <td class="py-1 pr-4">{{ q.spec.priority ?? '—' }}</td>
              <td class="py-1">{{ budgetText(q.spec.budget) || '—' }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
    <div>
      <h3 class="mb-2 text-sm font-semibold">Pools</h3>
      <div class="max-w-full overflow-x-auto">
        <table class="w-full text-left text-sm">
          <thead class="text-ccf-600 dark:text-ccf-400">
            <tr>
              <th scope="col" class="py-1 pr-4 font-medium">Pool</th>
              <th scope="col" class="py-1 pr-4 font-medium">Mode</th>
              <th scope="col" class="py-1 font-medium">Scaling</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="p in pools"
              :key="p.metadata.name"
              class="border-t border-ccf-200 dark:border-ccf-800"
              data-testid="catalog-pool"
            >
              <th scope="row" class="py-1 pr-4 font-mono text-xs font-medium">
                {{ p.metadata.name }}
              </th>
              <td class="py-1 pr-4">{{ p.spec.mode }}</td>
              <td class="py-1">
                {{
                  p.spec.scaling
                    ? `${p.spec.scaling.min}–${p.spec.scaling.max} agents`
                    : '—'
                }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <p class="mt-2 text-sm">
        <RouterLink
          :to="{ name: 'pools' }"
          class="text-ccf-600 underline dark:text-ccf-400"
          >Live queues, pools and agents →</RouterLink
        >
      </p>
    </div>
  </div>
</template>
