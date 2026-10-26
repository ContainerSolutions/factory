<script setup lang="ts">
import { ref, watch } from 'vue';
import JsonViewer from '@/components/shared/JsonViewer.vue';
import type { AdminOutcome } from '@/stores/workitems';
import Button from '@/volt/Button.vue';
import Dialog from '@/volt/Dialog.vue';
import Message from '@/volt/Message.vue';
import SecondaryButton from '@/volt/SecondaryButton.vue';

/**
 * Asks before an admin request that isn't a command (replay, close, delete): says what it does,
 * shows the exact request it sends, and sends it only when confirmed. It closes and emits `done`
 * only when the request succeeded; a refusal stays in the dialog with the server's reason, and
 * is emitted as `refused` so the caller can offer another way (a forced delete).
 */
const props = defineProps<{
  title: string;
  /** The request, as shown: `POST /api/v1/events/…/replay`. */
  request: string;
  /** The request's JSON body, if it has one. */
  body?: unknown;
  confirmLabel: string;
  invalid?: boolean;
  send: () => Promise<AdminOutcome<unknown>>;
}>();

const emit = defineEmits<{
  done: [data: unknown];
  refused: [outcome: Extract<AdminOutcome<unknown>, { ok: false }>];
}>();

const visible = defineModel<boolean>('visible', { required: true });

const sending = ref(false);
const error = ref<string | null>(null);

watch(visible, (open) => {
  if (open) error.value = null;
});

async function confirm(): Promise<void> {
  if (sending.value || props.invalid) return;
  sending.value = true;
  error.value = null;
  try {
    const outcome = await props.send();
    if (outcome.ok) {
      visible.value = false;
      emit('done', outcome.data);
    } else {
      error.value = outcome.message;
      emit('refused', outcome);
    }
  } finally {
    sending.value = false;
  }
}
</script>

<template>
  <Dialog
    v-model:visible="visible"
    :header="title"
    modal
    :draggable="false"
    :closable="!sending"
    :theme="{ root: 'w-[calc(100%-2rem)] sm:w-[32rem]' }"
  >
    <div class="flex flex-col gap-4 text-sm" data-testid="confirm-request">
      <slot />
      <section aria-label="Request">
        <h3 class="mb-2 font-semibold">Sends</h3>
        <code
          class="block font-mono text-xs break-all"
          data-testid="confirm-request-line"
          >{{ request }}</code
        >
        <JsonViewer
          v-if="body !== undefined"
          class="mt-2"
          :value="body"
          label="Request JSON"
        />
      </section>
      <Message
        v-if="error"
        severity="error"
        role="alert"
        data-testid="request-error"
      >
        {{ error }}
      </Message>
    </div>
    <template #footer>
      <SecondaryButton
        label="Go back"
        :disabled="sending"
        @click="visible = false"
      />
      <Button
        :label="confirmLabel"
        :loading="sending"
        :disabled="invalid"
        data-testid="confirm"
        @click="confirm"
      />
    </template>
  </Dialog>
</template>
