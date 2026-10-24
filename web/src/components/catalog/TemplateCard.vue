<script setup lang="ts">
import { computed } from 'vue';
import type { Schemas } from '@/api/client';
import CodeBlock from '@/components/shared/CodeBlock.vue';
import ObjectYaml from '@/components/shared/ObjectYaml.vue';
import { tokenizeTemplate } from '@/components/shared/highlight';

/** One notify Template: each provider's body, an inline Go template highlighted or a Block Kit file. */
const props = defineProps<{ template: Schemas['Template'] }>();

const name = computed(() => props.template.metadata.name);
const headingId = computed(() => `template-${name.value}`);
const bodies = computed(() =>
  Object.entries(props.template.spec.bodies)
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([via, body]) =>
      typeof body === 'string'
        ? { via, tokens: tokenizeTemplate(body), file: '' }
        : { via, tokens: [], file: body.blocks },
    ),
);
</script>

<template>
  <section
    :aria-labelledby="headingId"
    class="rounded-lg border border-ccf-200 p-4 dark:border-ccf-800"
    data-testid="template"
  >
    <h3 :id="headingId" class="mb-2 font-mono font-semibold">{{ name }}</h3>
    <div
      v-for="b in bodies"
      :key="b.via"
      class="mb-2"
      data-testid="template-body"
    >
      <h4 class="mb-1 text-xs font-semibold">{{ b.via }}</h4>
      <p v-if="b.file" class="text-sm">
        Block Kit file <code class="text-xs">{{ b.file }}</code>
      </p>
      <CodeBlock
        v-else
        :tokens="b.tokens"
        :label="`${b.via} body of ${name}`"
      />
    </div>
    <ObjectYaml :value="template" :label="`YAML of Template ${name}`" />
  </section>
</template>
