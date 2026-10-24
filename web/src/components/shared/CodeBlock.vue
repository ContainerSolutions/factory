<script setup lang="ts">
import type { Token, TokenKind } from './highlight';

// Each token is rendered as text, never HTML: catalog and provider text can't inject markup.
withDefaults(
  defineProps<{ tokens: readonly Token[]; label: string; inline?: boolean }>(),
  { inline: false },
);

const classes: Record<TokenKind, string> = {
  plain: '',
  key: 'text-sky-800 dark:text-sky-300',
  string: 'text-green-800 dark:text-green-300',
  number: 'text-amber-800 dark:text-amber-300',
  keyword: 'font-semibold text-purple-800 dark:text-purple-300',
  operator: 'text-red-800 dark:text-red-300',
  punct: 'text-ccf-500 dark:text-ccf-400',
  comment: 'italic text-ccf-500 dark:text-ccf-400',
  variable: 'text-teal-800 dark:text-teal-300',
  field: 'text-sky-800 dark:text-sky-300',
  function: 'text-indigo-800 dark:text-indigo-300',
  delim: 'font-semibold text-fuchsia-800 dark:text-fuchsia-300',
  heading: 'font-semibold text-ccf-900 dark:text-ccf-50',
};
</script>

<template>
  <code
    v-if="inline"
    class="font-mono text-xs break-all"
    :aria-label="label"
    data-testid="code"
    ><span
      v-for="(t, i) in tokens"
      :key="i"
      :class="classes[t.kind]"
      :data-kind="t.kind"
      >{{ t.text }}</span
    ></code
  >
  <pre
    v-else
    role="region"
    :aria-label="label"
    tabindex="0"
    class="max-w-full overflow-x-auto rounded-md border border-ccf-200 bg-ccf-50 p-3 font-mono text-xs leading-5 text-ccf-800 dark:border-ccf-700 dark:bg-ccf-900 dark:text-ccf-200"
    data-testid="code"
  ><span
      v-for="(t, i) in tokens"
      :key="i"
      :class="classes[t.kind]"
      :data-kind="t.kind"
      >{{ t.text }}</span
    ></pre>
</template>
