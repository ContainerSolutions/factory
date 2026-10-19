<script setup lang="ts">
import { computed } from 'vue';
import type { Schemas } from '@/api/client';
import Drawer from '@/volt/Drawer.vue';
import JsonViewer from './JsonViewer.vue';
import StatusPill from './StatusPill.vue';
import { formatTime } from './format';

/**
 * Why a rule did (or didn't) act: the RuleSet, the rule, the inputs its `where` saw, the rail
 * verdicts and the catalog commit, in a drawer.
 */
const props = defineProps<{
  decision: Schemas['Decision'] | null;
  /** The catalog commit; the contract's Decision doesn't carry it, so the caller finds it. */
  catalogSha?: string;
}>();

const visible = defineModel<boolean>('visible', { required: true });

const title = computed(() =>
  props.decision
    ? `Why: ${props.decision.ruleSet ?? '?'} / ${props.decision.rule ?? '?'}`
    : 'Why',
);
</script>

<template>
  <Drawer
    v-model:visible="visible"
    position="right"
    :header="title"
    role="dialog"
    aria-modal="true"
    :aria-label="title"
    class="w-full! sm:w-[32rem]!"
    :block-scroll="true"
  >
    <div
      v-if="decision"
      class="flex flex-col gap-5 text-sm"
      data-testid="decision-why"
    >
      <dl class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-2">
        <dt class="font-medium text-ccf-600 dark:text-ccf-400">RuleSet</dt>
        <dd data-testid="why-ruleset">{{ decision.ruleSet ?? '—' }}</dd>
        <dt class="font-medium text-ccf-600 dark:text-ccf-400">Rule</dt>
        <dd data-testid="why-rule">{{ decision.rule ?? '—' }}</dd>
        <dt class="font-medium text-ccf-600 dark:text-ccf-400">Result</dt>
        <dd>
          <StatusPill
            :label="decision.fired ? 'fired' : 'did not fire'"
            :tone="decision.fired ? 'success' : 'neutral'"
          />
        </dd>
        <dt class="font-medium text-ccf-600 dark:text-ccf-400">At</dt>
        <dd>
          <time :datetime="decision.at">{{ formatTime(decision.at) }}</time>
        </dd>
        <dt class="font-medium text-ccf-600 dark:text-ccf-400">Event</dt>
        <dd class="font-mono text-xs break-all">{{ decision.event ?? '—' }}</dd>
        <dt class="font-medium text-ccf-600 dark:text-ccf-400">Catalog SHA</dt>
        <dd class="font-mono text-xs break-all" data-testid="why-catalog-sha">
          {{ catalogSha ?? 'not recorded' }}
        </dd>
        <dt class="font-medium text-ccf-600 dark:text-ccf-400">Actions</dt>
        <dd>
          {{ decision.actions?.length ? decision.actions.join(', ') : 'none' }}
        </dd>
        <template v-if="decision.reason">
          <dt class="font-medium text-ccf-600 dark:text-ccf-400">Reason</dt>
          <dd>{{ decision.reason }}</dd>
        </template>
      </dl>

      <section aria-labelledby="why-inputs">
        <h3 id="why-inputs" class="mb-2 font-semibold">
          Inputs <code class="text-xs font-normal">where</code> saw
        </h3>
        <JsonViewer :value="decision.inputs ?? {}" label="where inputs" />
      </section>

      <section aria-labelledby="why-rails">
        <h3 id="why-rails" class="mb-2 font-semibold">Rail verdicts</h3>
        <table v-if="decision.rails?.length" class="w-full text-left">
          <thead class="text-ccf-600 dark:text-ccf-400">
            <tr>
              <th scope="col" class="py-1 pr-3 font-medium">Rail</th>
              <th scope="col" class="py-1 pr-3 font-medium">Verdict</th>
              <th scope="col" class="py-1 font-medium">Reason</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="rail in decision.rails"
              :key="rail.rail"
              class="border-t border-ccf-200 dark:border-ccf-800"
            >
              <td class="py-1 pr-3 font-mono text-xs">{{ rail.rail }}</td>
              <td class="py-1 pr-3"><StatusPill :label="rail.verdict" /></td>
              <td class="py-1">{{ rail.reason ?? '' }}</td>
            </tr>
          </tbody>
        </table>
        <p v-else class="text-ccf-600 dark:text-ccf-400">No rail verdicts.</p>
      </section>
    </div>
  </Drawer>
</template>
