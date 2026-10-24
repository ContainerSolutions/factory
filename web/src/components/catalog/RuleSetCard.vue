<script setup lang="ts">
import { computed } from 'vue';
import type { Schemas } from '@/api/client';
import CodeBlock from '@/components/shared/CodeBlock.vue';
import JsonViewer from '@/components/shared/JsonViewer.vue';
import ObjectYaml from '@/components/shared/ObjectYaml.vue';
import StatusPill from '@/components/shared/StatusPill.vue';
import { tokenizeCEL } from '@/components/shared/highlight';
import { budgetText } from '@/components/pools/pools';
import {
  actionKinds,
  formatSelector,
  matchesSelector,
  providerLabels,
  selectRules,
} from './catalog';

/** One RuleSet: where it applies, how it selects rules, and each selected rule with its params. */
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
</script>

<template>
  <section
    :aria-labelledby="headingId"
    class="rounded-lg border border-ccf-200 p-4 dark:border-ccf-800"
    data-testid="ruleset"
  >
    <h3 :id="headingId" class="mb-2 font-mono font-semibold">{{ name }}</h3>
    <dl class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
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
    <ul v-if="selection.rules.length" class="flex flex-col gap-3">
      <li
        v-for="rule in selection.rules"
        :key="rule.name"
        class="rounded-md border border-ccf-200 p-3 dark:border-ccf-800"
        data-testid="selected-rule"
      >
        <div class="mb-1 flex flex-wrap items-center gap-2">
          <span class="font-mono text-sm font-medium" data-testid="rule-name">{{
            rule.name
          }}</span>
          <StatusPill
            v-for="via in rule.via"
            :key="via"
            :label="`by ${via}`"
            tone="neutral"
          />
          <StatusPill v-if="rule.spec.final" label="final" tone="warn" />
        </div>
        <dl class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
          <dt class="text-ccf-600 dark:text-ccf-400">On</dt>
          <dd class="font-mono text-xs leading-5">
            {{ rule.spec.on.join(', ') }}
          </dd>
          <template v-if="rule.spec.where">
            <dt class="text-ccf-600 dark:text-ccf-400">Where</dt>
            <dd>
              <CodeBlock
                :tokens="tokenizeCEL(rule.spec.where)"
                :label="`Where of ${rule.name}`"
                inline
              />
            </dd>
          </template>
          <template v-if="rule.spec.priority !== undefined">
            <dt class="text-ccf-600 dark:text-ccf-400">Priority</dt>
            <dd>{{ rule.spec.priority }}</dd>
          </template>
          <dt class="text-ccf-600 dark:text-ccf-400">Actions</dt>
          <dd data-testid="rule-actions">
            {{ actionKinds(rule.spec.actions).join(', ') || 'none' }}
          </dd>
        </dl>
        <template v-if="Object.keys(rule.params).length">
          <h5 class="mt-2 mb-1 text-xs font-semibold">
            Params after defaults and overrides
          </h5>
          <JsonViewer :value="rule.params" :label="`Params of ${rule.name}`" />
        </template>
        <p
          v-if="rule.unknownOverrides.length"
          class="mt-1 text-xs text-red-700 dark:text-red-300"
        >
          Overrides undeclared params: {{ rule.unknownOverrides.join(', ') }}
        </p>
      </li>
    </ul>
    <p v-else class="text-sm text-ccf-500 dark:text-ccf-400">
      Selects no rules.
    </p>
    <ObjectYaml :value="ruleSet" :label="`YAML of RuleSet ${name}`" />
  </section>
</template>
