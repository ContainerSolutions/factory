<script setup lang="ts">
import { computed } from 'vue';
import type { Schemas } from '@/api/client';
import CodeBlock from '@/components/shared/CodeBlock.vue';
import ObjectYaml from '@/components/shared/ObjectYaml.vue';
import StatusPill from '@/components/shared/StatusPill.vue';
import { tokenizeCEL, yamlTokens } from '@/components/shared/highlight';
import { formatSelector } from './catalog';

/** One standalone Rule as written: on, where (CEL), params and each action. */
const props = defineProps<{ rule: Schemas['Rule'] }>();

const spec = computed(() => props.rule.spec);
const name = computed(() => props.rule.metadata.name);
const headingId = computed(() => `rule-${name.value}`);
const labels = computed(() =>
  Object.entries(props.rule.metadata.labels ?? {}).map(([k, v]) => `${k}=${v}`),
);
const actions = computed(() =>
  spec.value.actions.map((a) => {
    const [kind, body] = Object.entries(a)[0] ?? ['unknown', undefined];
    return { kind, tokens: yamlTokens(body ?? {}) };
  }),
);
</script>

<template>
  <section
    :aria-labelledby="headingId"
    class="rounded-lg border border-ccf-200 p-4 dark:border-ccf-800"
    data-testid="rule"
  >
    <div class="mb-2 flex flex-wrap items-center gap-2">
      <h3 :id="headingId" class="font-mono font-semibold">{{ name }}</h3>
      <StatusPill v-for="l in labels" :key="l" :label="l" tone="neutral" />
      <StatusPill v-if="spec.final" label="final" tone="warn" />
    </div>
    <dl class="kv-grid gap-x-4 gap-y-2 text-sm">
      <template v-if="spec.ruleSet">
        <dt class="text-ccf-600 dark:text-ccf-400">RuleSet</dt>
        <dd class="font-mono text-xs">{{ spec.ruleSet }}</dd>
      </template>
      <dt class="text-ccf-600 dark:text-ccf-400">Providers</dt>
      <dd>
        <code class="text-xs">{{ formatSelector(spec.providerSelector) }}</code>
      </dd>
      <dt class="text-ccf-600 dark:text-ccf-400">On</dt>
      <dd class="font-mono text-xs leading-5" data-testid="rule-on">
        {{ spec.on.join(', ') }}
      </dd>
      <dt class="text-ccf-600 dark:text-ccf-400">Where</dt>
      <dd data-testid="rule-where">
        <CodeBlock
          v-if="spec.where"
          :tokens="tokenizeCEL(spec.where)"
          :label="`Where of ${name}`"
        />
        <span v-else>always</span>
      </dd>
      <template v-if="spec.params && Object.keys(spec.params).length">
        <dt class="text-ccf-600 dark:text-ccf-400">Params</dt>
        <dd data-testid="rule-params">
          <CodeBlock
            :tokens="yamlTokens(spec.params)"
            :label="`Params of ${name}`"
          />
        </dd>
      </template>
      <template v-if="spec.priority !== undefined">
        <dt class="text-ccf-600 dark:text-ccf-400">Priority</dt>
        <dd>{{ spec.priority }}</dd>
      </template>
      <template v-if="spec.debounce">
        <dt class="text-ccf-600 dark:text-ccf-400">Debounce</dt>
        <dd class="font-mono text-xs">
          {{ spec.debounce.window
          }}{{ spec.debounce.key ? ` by ${spec.debounce.key}` : '' }}
        </dd>
      </template>
      <template v-if="spec.schedule">
        <dt class="text-ccf-600 dark:text-ccf-400">Schedule</dt>
        <dd class="font-mono text-xs">{{ spec.schedule }}</dd>
      </template>
      <dt class="text-ccf-600 dark:text-ccf-400">Actions</dt>
      <dd>
        <ol class="flex flex-col gap-2" data-testid="rule-actions">
          <li v-for="(a, i) in actions" :key="i">
            <StatusPill :label="a.kind" tone="info" />
            <CodeBlock
              class="mt-1"
              :tokens="a.tokens"
              :label="`Action ${i + 1} of ${name}: ${a.kind}`"
            />
          </li>
        </ol>
      </dd>
    </dl>
    <ObjectYaml :value="rule" :label="`YAML of Rule ${name}`" />
  </section>
</template>
