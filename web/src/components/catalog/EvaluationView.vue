<script setup lang="ts">
import { computed, ref } from 'vue';
import type { Schemas } from '@/api/client';
import DecisionWhy from '@/components/shared/DecisionWhy.vue';
import JsonViewer from '@/components/shared/JsonViewer.vue';
import StatusPill from '@/components/shared/StatusPill.vue';
import SecondaryButton from '@/volt/SecondaryButton.vue';
import { decisionOf, matchTone } from './catalog';

/**
 * What the rule engine would do with an event: every candidate rule and its result, the ordered
 * actions with their rail verdicts (each with a why drawer), and the state they would leave.
 */
const props = defineProps<{
  evaluation: Schemas['Evaluation'];
  /** The catalog commit the dry run ran against; the Evaluation doesn't carry it. */
  catalogSha?: string;
}>();

const whyIndex = ref<number | null>(null);
const whyVisible = computed({
  get: () => whyIndex.value !== null,
  set: (v) => {
    if (!v) whyIndex.value = null;
  },
});
const why = computed(() => {
  const d =
    whyIndex.value === null
      ? undefined
      : props.evaluation.decisions[whyIndex.value];
  return d ? decisionOf(props.evaluation, d) : null;
});

function actionOf(action: Schemas['ActionIntent']): Record<string, unknown> {
  return Object.fromEntries(
    Object.entries(action).filter(([key]) => key !== 'type'),
  );
}
</script>

<template>
  <section aria-label="Evaluation" data-testid="evaluation">
    <p class="mb-3 text-sm text-ccf-600 dark:text-ccf-400">
      Event <code class="text-xs">{{ evaluation.event }}</code>
      <template v-if="evaluation.workItem">
        against work item
        <code class="text-xs">{{ evaluation.workItem }}</code></template
      >
      <template v-else> against an empty work item</template>. Nothing was
      stored or applied.
    </p>

    <h3 class="mb-2 text-sm font-semibold">Candidate rules</h3>
    <div v-if="evaluation.matches.length" class="max-w-full overflow-x-auto">
      <table class="mb-4 w-full text-left text-sm">
        <thead class="text-ccf-600 dark:text-ccf-400">
          <tr>
            <th scope="col" class="py-1 pr-4 font-medium">RuleSet</th>
            <th scope="col" class="py-1 pr-4 font-medium">Rule</th>
            <th scope="col" class="py-1 pr-4 font-medium">Result</th>
            <th scope="col" class="py-1 font-medium">Error</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="(m, i) in evaluation.matches"
            :key="i"
            class="border-t border-ccf-200 dark:border-ccf-800"
            data-testid="match"
          >
            <td class="py-1 pr-4 font-mono text-xs">{{ m.ruleSet }}</td>
            <td class="py-1 pr-4 font-mono text-xs">{{ m.rule }}</td>
            <td class="py-1 pr-4">
              <StatusPill :label="m.result" :tone="matchTone(m.result)" />
            </td>
            <td class="py-1 text-xs">
              {{ m.error ?? ''
              }}<template v-if="m.actionIndex !== undefined">
                (action {{ m.actionIndex }})</template
              >
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-else class="mb-4 text-sm text-ccf-500">No rule is a candidate.</p>

    <h3 class="mb-2 text-sm font-semibold">Actions</h3>
    <ol v-if="evaluation.decisions.length" class="mb-4 flex flex-col gap-3">
      <li
        v-for="(d, i) in evaluation.decisions"
        :key="i"
        class="rounded-md border border-ccf-200 p-3 dark:border-ccf-800"
        data-testid="decision"
      >
        <div class="mb-2 flex flex-wrap items-center gap-2 text-sm">
          <span class="font-medium" data-testid="decision-action">{{
            d.action.type
          }}</span>
          <span class="font-mono text-xs text-ccf-600 dark:text-ccf-400"
            >{{ d.ruleSet }} / {{ d.rule }}</span
          >
          <StatusPill :label="d.rail.verdict" data-testid="decision-verdict" />
          <span v-if="d.rail.reason" class="text-xs">{{ d.rail.reason }}</span>
          <SecondaryButton
            size="small"
            class="ms-auto"
            label="Why?"
            :aria-label="`Why: ${d.ruleSet} / ${d.rule} (action ${i + 1})`"
            aria-haspopup="dialog"
            @click="whyIndex = i"
          />
        </div>
        <JsonViewer
          :value="actionOf(d.action)"
          :label="`Action ${i + 1}: ${d.action.type}`"
        />
      </li>
    </ol>
    <p v-else class="mb-4 text-sm text-ccf-500">No action would be taken.</p>

    <template v-if="evaluation.stateAfter">
      <h3 class="mb-2 text-sm font-semibold">State after</h3>
      <JsonViewer :value="evaluation.stateAfter" label="State after" />
    </template>

    <DecisionWhy
      v-model:visible="whyVisible"
      :decision="why"
      :catalog-sha="catalogSha"
    />
  </section>
</template>
