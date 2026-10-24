<script setup lang="ts">
import type { Schemas } from '@/api/client';

defineProps<{ offers: Schemas['HarnessOffer'][] }>();
</script>

<template>
  <div v-if="offers.length" class="max-w-full overflow-x-auto">
    <table class="w-full text-left text-sm">
      <caption class="sr-only">
        Harness offers
      </caption>
      <thead class="text-xs text-ccf-600 dark:text-ccf-400">
        <tr>
          <th scope="col" class="pr-3 font-medium">Harness</th>
          <th scope="col" class="pr-3 font-medium">Auth</th>
          <th scope="col" class="pr-3 font-medium">Models</th>
          <th scope="col" class="font-medium">Reasoning effort</th>
        </tr>
      </thead>
      <tbody>
        <tr
          v-for="offer in offers"
          :key="offer.name"
          class="align-top"
          data-testid="harness-offer"
        >
          <td class="pr-3">
            {{ offer.name }}
            <span
              v-if="offer.version"
              class="text-xs text-ccf-600 dark:text-ccf-400"
              >{{ offer.version }}</span
            >
          </td>
          <td class="pr-3">{{ offer.auth ?? '' }}</td>
          <td class="pr-3">
            <ul v-if="offer.models?.length" role="list">
              <li
                v-for="model in offer.models"
                :key="model"
                class="font-mono text-xs"
              >
                {{ model
                }}<span
                  v-if="model === offer.defaultModel"
                  class="ml-1 font-sans text-ccf-600 dark:text-ccf-400"
                  data-testid="default-model"
                  >(default)</span
                >
              </li>
            </ul>
            <span v-else class="text-ccf-500">none reported</span>
            <p
              v-if="
                offer.defaultModel &&
                !offer.models?.includes(offer.defaultModel)
              "
              class="text-xs"
              data-testid="default-model"
            >
              default: <span class="font-mono">{{ offer.defaultModel }}</span>
            </p>
          </td>
          <td>{{ offer.reasoningEffort?.join(', ') ?? '' }}</td>
        </tr>
      </tbody>
    </table>
  </div>
  <p v-else class="text-sm text-ccf-500 dark:text-ccf-400">
    No harness offers.
  </p>
</template>
