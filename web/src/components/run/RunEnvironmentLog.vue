<script setup lang="ts">
import { onMounted } from 'vue';
import type { TranscriptState } from '@/stores/runs';
import Message from '@/volt/Message.vue';

/**
 * The run's environment log: the TaskSpec environment's scripts' output, as the agent uploaded it
 * (redacted). Loaded when shown; rendered as text, wrapping long lines as the transcript does.
 */
const props = defineProps<{
  state: TranscriptState;
  text: string | null;
  error?: string | null;
}>();

const emit = defineEmits<{ load: [] }>();

onMounted(() => {
  if (props.state === 'idle') emit('load');
});
</script>

<template>
  <section aria-labelledby="environment-log-heading">
    <h2 id="environment-log-heading" class="mb-3 text-lg font-semibold">
      Environment log
    </h2>

    <p
      v-if="state === 'idle' || state === 'loading'"
      class="text-sm text-ccf-600 dark:text-ccf-400"
      role="status"
    >
      Loading…
    </p>

    <p v-else-if="state === 'missing'" class="text-sm text-ccf-500">
      No environment log was uploaded for this run.
    </p>

    <Message v-else-if="state === 'error'" severity="error" role="alert">
      Couldn't load the environment log: {{ error }}
    </Message>

    <!-- Untrusted (the environment's scripts' output, redacted by the agent): text, never HTML. -->
    <pre
      v-else-if="text"
      class="max-h-[70vh] overflow-y-auto rounded-md border border-ccf-200 bg-ccf-50 p-3 font-mono text-xs whitespace-pre-wrap [overflow-wrap:anywhere] dark:border-ccf-800 dark:bg-ccf-950"
      data-testid="environment-log"
      >{{ text }}</pre
    >

    <p v-else class="text-sm text-ccf-500">The environment log is empty.</p>
  </section>
</template>
