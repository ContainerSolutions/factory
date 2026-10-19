<script setup lang="ts">
import type { Schemas } from '@/api/client';
import StatusPill from '@/components/shared/StatusPill.vue';
import { formatTime, safeURL } from '@/components/shared/format';

defineProps<{ effects: Schemas['Effect'][] }>();
</script>

<template>
  <section aria-labelledby="effects-heading">
    <h2 id="effects-heading" class="mb-2 text-sm font-semibold">Effects</h2>
    <ul v-if="effects.length" class="flex flex-col gap-2 text-sm">
      <li
        v-for="effect in effects"
        :key="effect.metadata.name"
        class="rounded-md border border-ccf-200 p-2 dark:border-ccf-800"
        data-testid="effect"
      >
        <div class="flex flex-wrap items-center gap-2">
          <span class="font-mono text-xs">{{ effect.spec.op }}</span>
          on
          <span class="break-all">{{ effect.spec.target.ref }}</span>
          <StatusPill
            v-if="effect.status?.result?.error"
            label="failed"
            tone="danger"
          />
        </div>
        <p
          v-if="effect.spec.identity || effect.status?.at"
          class="mt-1 text-xs text-ccf-600 dark:text-ccf-400"
        >
          <template v-if="effect.spec.identity"
            >as {{ effect.spec.identity }}</template
          >
          <template v-if="effect.status?.at">
            ·
            <time :datetime="effect.status.at">{{
              formatTime(effect.status.at)
            }}</time></template
          >
        </p>
        <p
          v-if="effect.status?.result?.error"
          class="mt-1 text-red-700 dark:text-red-300"
        >
          {{ effect.status.result.error }}
        </p>
        <p v-else-if="effect.status?.result?.url" class="mt-1 text-xs">
          <a
            v-if="safeURL(effect.status.result.url)"
            :href="safeURL(effect.status.result.url)"
            target="_blank"
            rel="noopener noreferrer"
            class="underline break-all"
            >{{ effect.status.result.url }}</a
          >
          <span v-else class="break-all">{{ effect.status.result.url }}</span>
        </p>
      </li>
    </ul>
    <p v-else class="text-sm text-ccf-500">No effects.</p>
  </section>
</template>
