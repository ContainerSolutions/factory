<script setup lang="ts">
import { computed } from 'vue';
import type { Schemas } from '@/api/client';
import CodeBlock from '@/components/shared/CodeBlock.vue';
import { tokenizeJSON, tokenizeTemplate } from '@/components/shared/highlight';

/**
 * The catalog's files of one kind: a list to pick from and the picked file's content, a prompt
 * as its raw template highlighted, a schema as its JSON highlighted, with the TaskSpecs using it.
 */
const props = defineProps<{
  files: readonly Schemas['CatalogFileRef'][];
  selected: string;
  file: Schemas['CatalogFile'] | undefined;
  error: string | undefined;
  usedBy: Record<string, string[]>;
  label: string;
}>();
const emit = defineEmits<{ select: [path: string] }>();

const tokens = computed(() => {
  const f = props.file;
  if (!f) return [];
  return f.kind === 'schema'
    ? tokenizeJSON(f.content)
    : tokenizeTemplate(f.content);
});
</script>

<template>
  <div class="grid gap-4 lg:grid-cols-[minmax(12rem,max-content)_1fr]">
    <nav :aria-label="label">
      <ul class="flex flex-col gap-1 text-sm">
        <li v-for="f in files" :key="f.path">
          <button
            type="button"
            class="w-full rounded px-2 py-1 text-left font-mono text-xs hover:bg-ccf-100 dark:hover:bg-ccf-800"
            :class="
              f.path === selected
                ? 'bg-ccf-100 font-semibold dark:bg-ccf-800'
                : ''
            "
            :aria-current="f.path === selected ? 'true' : undefined"
            data-testid="catalog-file"
            @click="emit('select', f.path)"
          >
            {{ f.path }}
          </button>
        </li>
      </ul>
    </nav>
    <div v-if="selected" data-testid="catalog-file-view">
      <h3 class="font-mono text-sm font-semibold">{{ selected }}</h3>
      <p
        class="mb-2 text-xs text-ccf-600 dark:text-ccf-400"
        data-testid="catalog-file-users"
      >
        Used by
        {{
          usedBy[selected]?.length
            ? usedBy[selected]!.join(', ')
            : 'no TaskSpec'
        }}
        <template v-if="file">
          · <code>{{ file.digest }}</code></template
        >
      </p>
      <p
        v-if="error"
        class="text-sm text-red-700 dark:text-red-300"
        role="alert"
      >
        Couldn't load {{ selected }}: {{ error }}
      </p>
      <CodeBlock v-else-if="file" :tokens="tokens" :label="selected" />
      <p v-else class="text-sm text-ccf-600 dark:text-ccf-400" role="status">
        Loading…
      </p>
    </div>
  </div>
</template>
