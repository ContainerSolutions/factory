<script setup lang="ts">
import { ref, watch } from 'vue';
import type { Schemas } from '@/api/client';
import JsonViewer from '@/components/shared/JsonViewer.vue';
import { useControlsStore } from '@/stores/controls';
import Button from '@/volt/Button.vue';
import Dialog from '@/volt/Dialog.vue';
import Message from '@/volt/Message.vue';
import SecondaryButton from '@/volt/SecondaryButton.vue';
import type { Command } from './commands';

/**
 * Asks before sending a command: says what it does, shows the exact JSON it sends, and sends it
 * (`POST /commands`) only when confirmed. It closes and emits `accepted` only on a 2xx; a refusal
 * stays in the dialog with the server's reason.
 */
const props = defineProps<{
  title: string;
  command: Command;
  /** The confirm button's label, e.g. "Pause". */
  confirmLabel: string;
  /** Set while the fields in the slot don't make a valid command. */
  invalid?: boolean;
}>();

const emit = defineEmits<{ accepted: [Schemas['CommandAccepted']] }>();

const visible = defineModel<boolean>('visible', { required: true });

const store = useControlsStore();
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
    const outcome = await store.send(props.command);
    if (outcome.ok) {
      visible.value = false;
      emit('accepted', outcome.accepted);
    } else {
      error.value = outcome.message;
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
    <div class="flex flex-col gap-4 text-sm" data-testid="confirm-command">
      <slot />
      <section aria-label="Command">
        <h3 class="mb-2 font-semibold">Sends</h3>
        <JsonViewer :value="command" label="Command JSON" />
      </section>
      <Message
        v-if="error"
        severity="error"
        role="alert"
        data-testid="command-error"
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
