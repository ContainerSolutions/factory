<script setup lang="ts">
import { computed, ref, watch } from 'vue';
import type { Schemas } from '@/api/client';
import ListDetail from '@/components/shared/ListDetail.vue';
import ObjectYaml from '@/components/shared/ObjectYaml.vue';
import StatusPill from '@/components/shared/StatusPill.vue';
import { pickItem, type ListItem } from '@/components/shared/list';
import { budgetText } from '@/components/pools/pools';
import RuleCard from './RuleCard.vue';
import {
  formatSelector,
  matchesSelector,
  providerLabels,
  selectRules,
} from './catalog';

/**
 * One RuleSet: where it applies, how it selects rules, and its selected rules as a list; the
 * picked one (the first by default) shows as its rule, with its params after the RuleSet's
 * defaults and overrides.
 */
const props = defineProps<{
  ruleSet: Schemas['RuleSet'];
  rules: Schemas['Rule'][];
  providers: Schemas['Provider'][];
}>();

const spec = computed(() => props.ruleSet.spec);
const name = computed(() => props.ruleSet.metadata.name);
const headingId = computed(() => `ruleset-${name.value}`);
const selection = computed(() => selectRules(props.ruleSet, props.rules));
const matched = computed(() =>
  props.providers
    .filter((p) =>
      matchesSelector(spec.value.providerSelector, providerLabels(p)),
    )
    .map((p) => p.metadata.name),
);
const scope = computed(() => {
  const s = spec.value.scope;
  if (!s) return [];
  return [
    ['Repos', s.repos],
    ['Excluding', s.exclude],
    ['Channels', s.channels],
    ['Branches', s.branches],
  ].filter((e): e is [string, string[]] => !!e[1]?.length);
});
const selectionText = computed(() => {
  const sel = spec.value.rules ?? {};
  return [
    sel.refs?.length ? `refs ${sel.refs.join(', ')}` : '',
    sel.selector ? `selector ${formatSelector(sel.selector)}` : '',
    spec.value.inlineRules?.length
      ? `${spec.value.inlineRules.length} inline`
      : '',
    sel.exclude?.length ? `minus ${sel.exclude.join(', ')}` : '',
  ]
    .filter(Boolean)
    .join('; ');
});
const budget = computed(() => budgetText(spec.value.budget));

const ruleItems = computed<ListItem[]>(() =>
  selection.value.rules.map((r) => ({
    id: r.name,
    label: r.name,
    secondary: `on ${r.spec.on.join(', ')}`,
  })),
);
const picked = ref('');
watch(name, () => (picked.value = ''));
const ruleShown = computed(() => pickItem(ruleItems.value, picked.value));
const shownRule = computed(() =>
  selection.value.rules.find((r) => r.name === ruleShown.value),
);
</script>

<template>
  <section
    :aria-labelledby="headingId"
    class="rounded-lg border border-ccf-200 p-4 dark:border-ccf-800"
    data-testid="ruleset"
  >
    <h3 :id="headingId" class="mb-2 font-mono font-semibold">{{ name }}</h3>
    <dl class="kv-grid gap-x-4 gap-y-1 text-sm">
      <dt class="text-ccf-600 dark:text-ccf-400">Providers</dt>
      <dd data-testid="ruleset-providers">
        <code class="text-xs">{{ formatSelector(spec.providerSelector) }}</code>
        → {{ matched.length ? matched.join(', ') : 'none' }}
      </dd>
      <template v-for="[label, values] in scope" :key="label">
        <dt class="text-ccf-600 dark:text-ccf-400">{{ label }}</dt>
        <dd class="font-mono text-xs leading-5 break-all">
          {{ values.join(', ') }}
        </dd>
      </template>
      <dt class="text-ccf-600 dark:text-ccf-400">Selects</dt>
      <dd data-testid="ruleset-selection">{{ selectionText || 'nothing' }}</dd>
      <template v-if="budget">
        <dt class="text-ccf-600 dark:text-ccf-400">Budget</dt>
        <dd>{{ budget }}</dd>
      </template>
      <template v-if="spec.stateKeys?.length">
        <dt class="text-ccf-600 dark:text-ccf-400">State keys</dt>
        <dd class="font-mono text-xs leading-5">
          {{ spec.stateKeys.join(', ') }}
        </dd>
      </template>
    </dl>

    <p
      v-if="selection.missing.length"
      class="mt-2 text-sm text-red-700 dark:text-red-300"
      data-testid="ruleset-missing"
    >
      Names no loaded rule: {{ selection.missing.join(', ') }}
    </p>

    <h4 class="mt-4 mb-2 text-sm font-semibold">
      Selected rules ({{ selection.rules.length }})
    </h4>
    <ListDetail
      v-if="selection.rules.length"
      :items="ruleItems"
      :selected="ruleShown"
      :label="`Rules of RuleSet ${name}`"
      @select="picked = $event"
    >
      <div v-if="shownRule" data-testid="selected-rule">
        <div class="mb-2 flex flex-wrap items-center gap-2 text-xs">
          <span class="text-ccf-600 dark:text-ccf-400">Selected</span>
          <StatusPill
            v-for="via in shownRule.via"
            :key="via"
            :label="`by ${via}`"
            tone="neutral"
          />
        </div>
        <p
          v-if="shownRule.unknownOverrides.length"
          class="mb-2 text-xs text-red-700 dark:text-red-300"
        >
          Overrides undeclared params:
          {{ shownRule.unknownOverrides.join(', ') }}
        </p>
        <RuleCard
          :rule="{
            kind: 'Rule',
            metadata: { name: shownRule.name, labels: shownRule.labels },
            spec: shownRule.spec,
          }"
          :params="shownRule.params"
        />
      </div>
    </ListDetail>
    <p v-else class="text-sm text-ccf-500 dark:text-ccf-400">
      Selects no rules.
    </p>
    <ObjectYaml :value="ruleSet" :label="`YAML of RuleSet ${name}`" />
  </section>
</template>
