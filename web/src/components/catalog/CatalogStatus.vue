<script setup lang="ts">
import { computed } from 'vue';
import type { Schemas } from '@/api/client';
import CommandButton from '@/components/controls/CommandButton.vue';
import { resyncCatalogCommand } from '@/components/controls/commands';
import StatusPill from '@/components/shared/StatusPill.vue';
import { formatTime, safeURL, type Tone } from '@/components/shared/format';
import { shortSHA } from './catalog';

/**
 * The loaded catalog: its commit and load status, its errors and warnings (among them the App
 * installations missing on a repo it touches), what it holds, and a resync control.
 */
const props = defineProps<{ summary: Schemas['CatalogSummary'] }>();

const emit = defineEmits<{ resynced: [Schemas['CommandAccepted']] }>();

const STATUS: Record<Schemas['CatalogSummary']['status'], Tone> = {
  loaded: 'success',
  loading: 'info',
  error: 'danger',
};

const repoURL = computed(() => safeURL(props.summary.repo));
const counts = computed(() => {
  const by = new Map<string, number>();
  for (const o of props.summary.objects) {
    by.set(o.kind, (by.get(o.kind) ?? 0) + 1);
  }
  return [...by.entries()];
});
const command = resyncCatalogCommand();

const WARNING_LABEL: Record<string, string> = {
  'app-not-installed': 'App not installed',
};
</script>

<template>
  <section
    aria-label="Catalog status"
    class="mb-8 rounded-lg border border-ccf-200 p-4 dark:border-ccf-800"
  >
    <div class="flex flex-wrap items-start gap-6">
      <dl class="kv-grid gap-x-4 gap-y-1 text-sm">
        <dt class="text-ccf-600 dark:text-ccf-400">Status</dt>
        <dd data-testid="catalog-status">
          <StatusPill
            :label="summary.status"
            :tone="STATUS[summary.status] ?? 'neutral'"
          />
          <span
            v-if="summary.status === 'error'"
            class="ms-2 text-ccf-600 dark:text-ccf-400"
            >serving the last good catalog</span
          >
        </dd>
        <dt class="text-ccf-600 dark:text-ccf-400">Commit</dt>
        <dd>
          <code
            class="text-xs"
            :title="summary.sha"
            data-testid="catalog-sha"
            >{{ shortSHA(summary.sha) }}</code
          >
          <span class="text-ccf-600 dark:text-ccf-400">
            on {{ summary.ref }}</span
          >
        </dd>
        <dt class="text-ccf-600 dark:text-ccf-400">Source</dt>
        <dd class="break-all">
          <a
            v-if="repoURL"
            :href="repoURL"
            class="text-ccf-700 underline dark:text-ccf-300"
            rel="noopener noreferrer"
            >{{ summary.repo }}</a
          >
          <span v-else>{{ summary.repo || 'local directory' }}</span>
        </dd>
        <template v-if="summary.loadedAt">
          <dt class="text-ccf-600 dark:text-ccf-400">Loaded</dt>
          <dd>
            <time :datetime="summary.loadedAt">{{
              formatTime(summary.loadedAt)
            }}</time>
          </dd>
        </template>
        <dt class="text-ccf-600 dark:text-ccf-400">Objects</dt>
        <dd data-testid="catalog-counts">
          {{ counts.map(([kind, n]) => `${n} ${kind}`).join(', ') || 'none' }}
        </dd>
      </dl>
      <div class="ms-auto">
        <CommandButton
          label="Resync catalog"
          title="Resync the catalog?"
          confirm-label="Resync"
          :command="command"
          @accepted="emit('resynced', $event)"
        >
          <p>
            Re-reads the catalog from {{ summary.repo || 'its directory' }} at
            {{ summary.ref }} now, instead of at the next poll. A catalog that
            fails to load keeps the last good one serving.
          </p>
        </CommandButton>
      </div>
    </div>

    <template v-if="summary.errors?.length">
      <h2 class="mt-4 mb-2 text-sm font-semibold">Load errors</h2>
      <ul class="flex flex-col gap-1 text-sm" data-testid="catalog-errors">
        <li v-for="(e, i) in summary.errors" :key="i">
          <code v-if="e.code" class="me-2 text-xs">{{ e.code }}</code>
          <span v-if="e.file" class="me-2 font-mono text-xs"
            >{{ e.file }}<template v-if="e.line">:{{ e.line }}</template></span
          >
          <span>{{ e.message }}</span>
        </li>
      </ul>
    </template>

    <template v-if="summary.warnings?.length">
      <h2 class="mt-4 mb-2 text-sm font-semibold">Warnings</h2>
      <ul class="flex flex-col gap-1 text-sm" data-testid="catalog-warnings">
        <li
          v-for="(w, i) in summary.warnings"
          :key="i"
          class="flex flex-wrap items-baseline gap-2"
        >
          <StatusPill
            :label="(w.code && WARNING_LABEL[w.code]) || w.code || 'warning'"
            tone="warn"
          />
          <span v-if="w.file" class="font-mono text-xs"
            >{{ w.file }}<template v-if="w.line">:{{ w.line }}</template></span
          >
          <span class="[overflow-wrap:anywhere]">{{ w.message }}</span>
        </li>
      </ul>
    </template>
  </section>
</template>
