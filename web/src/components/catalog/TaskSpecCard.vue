<script setup lang="ts">
import { computed } from 'vue';
import { RouterLink } from 'vue-router';
import type { Schemas } from '@/api/client';
import CodeBlock from '@/components/shared/CodeBlock.vue';
import ObjectYaml from '@/components/shared/ObjectYaml.vue';
import StatusPill from '@/components/shared/StatusPill.vue';
import { tokenizeValue, yamlTokens } from '@/components/shared/highlight';
import {
  constraintText,
  harnessText,
  inputRows,
  limitsText,
  skillText,
} from './catalog';

/**
 * One TaskSpec, readable: inputs, instructions (a link to the prompt), skills, harness,
 * workspace, identity and permissions, effects, limits and outputs (a link to the schema).
 */
const props = defineProps<{ taskSpec: Schemas['TaskSpec'] }>();

const spec = computed(() => props.taskSpec.spec);
const name = computed(() => props.taskSpec.metadata.name);
const headingId = computed(() => `taskspec-${name.value}`);
const inputs = computed(() => inputRows(spec.value.inputs));
const permissions = computed(() =>
  Object.entries(spec.value.permissions ?? {})
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([k, v]) => `${k}: ${v}`),
);
const environment = computed(() => spec.value.workspace?.environment);

function fileLink(tab: 'prompts' | 'schemas', file: string) {
  return { name: 'catalog', query: { tab, file } };
}
</script>

<template>
  <section
    :aria-labelledby="headingId"
    class="rounded-lg border border-ccf-200 p-4 dark:border-ccf-800"
    data-testid="taskspec"
  >
    <h3 :id="headingId" class="font-mono font-semibold">{{ name }}</h3>
    <p
      v-if="spec.description"
      class="mt-1 text-sm text-ccf-600 dark:text-ccf-400"
    >
      {{ spec.description }}
    </p>

    <dl class="mt-3 kv-grid gap-x-4 gap-y-2 text-sm">
      <dt class="text-ccf-600 dark:text-ccf-400">Inputs</dt>
      <dd>
        <div v-if="inputs.length" class="max-w-full overflow-x-auto">
          <table class="text-left text-xs" data-testid="taskspec-inputs">
            <thead class="text-ccf-600 dark:text-ccf-400">
              <tr>
                <th scope="col" class="pr-3 font-medium">Name</th>
                <th scope="col" class="pr-3 font-medium">Type</th>
                <th scope="col" class="pr-3 font-medium">Values</th>
                <th scope="col" class="font-medium">Default</th>
              </tr>
            </thead>
            <tbody class="font-mono">
              <tr v-for="i in inputs" :key="i.name">
                <th scope="row" class="pr-3 font-medium">
                  {{ i.name }}<span v-if="i.required" title="required">*</span>
                </th>
                <td class="pr-3">{{ i.type }}</td>
                <td class="pr-3">{{ i.values || 'any' }}</td>
                <td>{{ i.default || '—' }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <span v-else>none</span>
      </dd>

      <dt class="text-ccf-600 dark:text-ccf-400">Prompt</dt>
      <dd data-testid="taskspec-prompt">
        <template v-if="spec.instructions">
          <RouterLink
            :to="fileLink('prompts', spec.instructions.template)"
            class="font-mono text-xs text-sky-800 underline dark:text-sky-300"
            >{{ spec.instructions.template }}</RouterLink
          >
          <StatusPill
            v-if="spec.instructions.headless"
            class="ms-2"
            label="headless"
            tone="neutral"
          />
        </template>
        <span v-else>none</span>
      </dd>

      <template v-if="spec.skills?.length">
        <dt class="text-ccf-600 dark:text-ccf-400">Skills</dt>
        <dd class="font-mono text-xs leading-5" data-testid="taskspec-skills">
          <div v-for="s in spec.skills" :key="skillText(s)">
            {{ skillText(s) }}
          </div>
        </dd>
      </template>

      <dt class="text-ccf-600 dark:text-ccf-400">Harness</dt>
      <dd data-testid="taskspec-harness">
        {{ harnessText(spec.harness?.default) || 'the agent picks' }}
        <ul
          v-if="spec.harness?.allowed?.length"
          class="mt-1 text-xs text-ccf-600 dark:text-ccf-400"
        >
          <li v-for="c in spec.harness.allowed" :key="c.name">
            allowed {{ constraintText(c) }}
          </li>
        </ul>
      </dd>

      <template v-if="spec.requires?.labels?.length">
        <dt class="text-ccf-600 dark:text-ccf-400">Requires</dt>
        <dd class="font-mono text-xs leading-5">
          {{ spec.requires.labels.join(', ') }}
        </dd>
      </template>

      <template v-if="spec.workspace">
        <dt class="text-ccf-600 dark:text-ccf-400">Workspace</dt>
        <dd data-testid="taskspec-workspace">
          <ul class="flex flex-col gap-1">
            <li
              v-for="r in spec.workspace.repos ?? []"
              :key="r.ref"
              class="flex flex-wrap items-center gap-2"
            >
              <CodeBlock :tokens="tokenizeValue(r.ref)" label="Repo" inline />
              <StatusPill :label="r.mode ?? 'ro'" tone="neutral" />
            </li>
            <li v-if="spec.workspace.checkout" class="text-xs">
              checkout <code>{{ spec.workspace.checkout }}</code>
            </li>
            <li v-if="spec.workspace.publicReads" class="text-xs">
              public reads
            </li>
            <li v-if="spec.workspace.hosts?.length" class="text-xs">
              hosts
              <code>{{ spec.workspace.hosts.join(', ') }}</code>
            </li>
          </ul>
          <div v-if="environment" class="mt-2">
            <span class="text-xs text-ccf-600 dark:text-ccf-400"
              >Environment</span
            >
            <CodeBlock
              :tokens="yamlTokens(environment)"
              :label="`Environment of ${name}`"
            />
          </div>
        </dd>
      </template>

      <template v-if="spec.identity">
        <dt class="text-ccf-600 dark:text-ccf-400">Identity</dt>
        <dd>{{ spec.identity }}</dd>
      </template>

      <template v-if="permissions.length">
        <dt class="text-ccf-600 dark:text-ccf-400">Permissions</dt>
        <dd
          class="font-mono text-xs leading-5"
          data-testid="taskspec-permissions"
        >
          {{ permissions.join(', ') }}
        </dd>
      </template>

      <dt class="text-ccf-600 dark:text-ccf-400">Effects</dt>
      <dd class="font-mono text-xs leading-5" data-testid="taskspec-effects">
        {{ (spec.effects?.allow ?? []).join(', ') || 'none' }}
        <div
          v-if="spec.effects?.labels?.length"
          class="text-ccf-600 dark:text-ccf-400"
        >
          labels {{ spec.effects.labels.join(', ') }}
        </div>
      </dd>

      <dt class="text-ccf-600 dark:text-ccf-400">Limits</dt>
      <dd data-testid="taskspec-limits">
        {{ limitsText(spec.limits) || 'none' }}
      </dd>

      <dt class="text-ccf-600 dark:text-ccf-400">Outputs</dt>
      <dd data-testid="taskspec-output">
        <RouterLink
          v-if="spec.output?.schema"
          :to="fileLink('schemas', spec.output.schema)"
          class="font-mono text-xs text-sky-800 underline dark:text-sky-300"
          >{{ spec.output.schema }}</RouterLink
        >
        <span v-else class="text-xs">the base Result</span>
        <div
          v-if="spec.output?.outcomes?.length"
          class="mt-1 flex flex-wrap gap-1"
        >
          <StatusPill
            v-for="o in spec.output.outcomes"
            :key="o"
            :label="o"
            tone="neutral"
          />
        </div>
      </dd>
    </dl>

    <ObjectYaml :value="taskSpec" :label="`YAML of TaskSpec ${name}`" />
  </section>
</template>
