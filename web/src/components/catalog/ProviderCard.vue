<script setup lang="ts">
import { computed } from 'vue';
import type { Schemas } from '@/api/client';
import PauseControls from '@/components/controls/PauseControls.vue';
import ObjectYaml from '@/components/shared/ObjectYaml.vue';
import StatusPill from '@/components/shared/StatusPill.vue';
import { formatTime } from '@/components/shared/format';
import {
  providerHealthTone,
  providerLabelsText,
  providerScopeText,
} from './catalog';

/**
 * One provider and its health, with pause and resume. Only its name, type, labels and scope are
 * shown: a provider's spec also names its secrets, which never reach the page.
 */
const props = defineProps<{ view: Schemas['ProviderView'] }>();

const provider = computed(() => props.view.provider);
const name = computed(() => provider.value.metadata.name);
const headingId = computed(() => `provider-${name.value}`);
</script>

<template>
  <section
    :aria-labelledby="headingId"
    class="rounded-lg border border-ccf-200 p-4 dark:border-ccf-800"
    data-testid="provider"
  >
    <div class="mb-2 flex flex-wrap items-center gap-2">
      <h3 :id="headingId" class="font-mono font-semibold">{{ name }}</h3>
      <PauseControls scope="provider" :name="name" class="ms-auto" />
    </div>
    <dl class="kv-grid gap-x-4 gap-y-1 text-sm">
      <dt class="text-ccf-600 dark:text-ccf-400">Type</dt>
      <dd data-testid="provider-type">{{ provider.spec.type }}</dd>
      <dt class="text-ccf-600 dark:text-ccf-400">Labels</dt>
      <dd class="font-mono text-xs leading-5" data-testid="provider-labels">
        {{ providerLabelsText(provider) || 'none' }}
      </dd>
      <dt class="text-ccf-600 dark:text-ccf-400">Scope</dt>
      <dd data-testid="provider-scope">{{ providerScopeText(provider) }}</dd>
      <dt class="text-ccf-600 dark:text-ccf-400">Health</dt>
      <dd data-testid="provider-health">
        <StatusPill
          :label="view.health.status"
          :tone="providerHealthTone(view.health.status)"
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
      </dd>
    </dl>
    <ObjectYaml :value="provider" :label="`YAML of Provider ${name}`" />
  </section>
</template>
