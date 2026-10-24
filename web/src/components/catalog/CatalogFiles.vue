<script setup lang="ts">
import { computed, ref } from 'vue';
import type { Schemas } from '@/api/client';
import CodeBlock from '@/components/shared/CodeBlock.vue';
import ListDetail from '@/components/shared/ListDetail.vue';
import MarkdownView from '@/components/shared/MarkdownView.vue';
import type { ListItem } from '@/components/shared/list';
import { tokenizeJSON, tokenizeTemplate } from '@/components/shared/highlight';

/**
 * The catalog's files of one kind: a list to pick from and the picked file's content, with the
 * TaskSpecs using it. A prompt shows rendered as Markdown (its template actions as text) or, as
 * its source, the raw template highlighted; a schema as its JSON highlighted.
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

/** How a prompt is shown: rendered Markdown (the default) or its highlighted source. */
const mode = ref<'rendered' | 'source'>('rendered');
const MODES = [
  { id: 'rendered', label: 'Rendered' },
  { id: 'source', label: 'Source' },
] as const;

const items = computed<ListItem[]>(() =>
  props.files.map((f) => ({
    id: f.path,
    label: f.path,
    secondary: props.usedBy[f.path]?.length
      ? `used by ${props.usedBy[f.path]!.join(', ')}`
      : 'unused',
  })),
);

const tokens = computed(() => {
  const f = props.file;
  if (!f) return [];
  return f.kind === 'schema'
    ? tokenizeJSON(f.content)
    : tokenizeTemplate(f.content);
});
</script>

<template>
  <ListDetail
    :items="items"
    :selected="selected"
    :label="label"
    @select="emit('select', $event)"
  >
    <div data-testid="catalog-file-view">
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
      <template v-else-if="file">
        <div
          v-if="file.kind === 'prompt'"
          role="group"
          aria-label="Show the prompt"
          class="mb-2 inline-flex rounded-md border border-ccf-200 text-xs dark:border-ccf-700"
        >
          <button
            v-for="m in MODES"
            :key="m.id"
            type="button"
            class="px-2 py-1 first:rounded-s-md last:rounded-e-md"
            :class="
              mode === m.id
                ? 'bg-ccf-900 text-white dark:bg-ccf-100 dark:text-ccf-900'
                : 'text-ccf-700 dark:text-ccf-300'
            "
            :aria-pressed="mode === m.id"
            :data-mode="m.id"
            @click="mode = m.id"
          >
            {{ m.label }}
          </button>
        </div>
        <MarkdownView
          v-if="file.kind === 'prompt' && mode === 'rendered'"
          :source="file.content"
          :label="selected"
        />
        <CodeBlock v-else :tokens="tokens" :label="selected" />
      </template>
      <p v-else class="text-sm text-ccf-600 dark:text-ccf-400" role="status">
        Loading…
      </p>
    </div>
  </ListDetail>
</template>
