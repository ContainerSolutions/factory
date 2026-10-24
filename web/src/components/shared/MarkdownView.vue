<script setup lang="ts">
import { ref, watch } from 'vue';
import { renderMarkdown } from './markdown';

/** Markdown rendered through markdown-it (raw HTML escaped) and DOMPurify. */
const props = defineProps<{ source: string; label: string }>();

const html = ref<string | null>(null);
const error = ref<string | null>(null);

watch(
  () => props.source,
  async (source) => {
    error.value = null;
    try {
      const out = await renderMarkdown(source);
      if (source === props.source) html.value = out;
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e);
    }
  },
  { immediate: true },
);
</script>

<template>
  <p v-if="error" class="text-sm text-red-700 dark:text-red-300" role="alert">
    Couldn't render the Markdown: {{ error }}
  </p>
  <p
    v-else-if="html === null"
    class="text-sm text-ccf-600 dark:text-ccf-400"
    role="status"
  >
    Rendering…
  </p>
  <!--
    The only v-html in the app: `html` comes from renderMarkdown, which escapes raw HTML
    (markdown-it html: false) and sanitises the result with DOMPurify. Never bind anything else.
  -->
  <!-- eslint-disable-next-line vue/no-v-html -->
  <div
    v-else
    role="region"
    :aria-label="label"
    tabindex="0"
    class="markdown rounded-md border border-ccf-200 p-4 text-sm leading-6 dark:border-ccf-700 [&_a]:text-sky-800 [&_a]:underline dark:[&_a]:text-sky-300 [&_code]:rounded [&_code]:bg-ccf-100 [&_code]:px-1 [&_code]:font-mono [&_code]:text-xs dark:[&_code]:bg-ccf-800 [&_h1]:mt-4 [&_h1]:mb-2 [&_h1]:text-lg [&_h1]:font-semibold [&_h2]:mt-4 [&_h2]:mb-2 [&_h2]:font-semibold [&_h3]:mt-3 [&_h3]:mb-1 [&_h3]:font-semibold [&_li]:my-0.5 [&_ol]:list-decimal [&_ol]:ps-6 [&_p]:my-2 [&_pre]:my-2 [&_pre]:overflow-x-auto [&_pre]:rounded-md [&_pre]:bg-ccf-100 [&_pre]:p-3 dark:[&_pre]:bg-ccf-900 [&_pre_code]:bg-transparent [&_pre_code]:p-0 [&_ul]:list-disc [&_ul]:ps-6"
    data-testid="markdown"
    v-html="html"
  />
</template>
