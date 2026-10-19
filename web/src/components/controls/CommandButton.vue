<script setup lang="ts">
import { ref } from 'vue';
import type { Schemas } from '@/api/client';
import { useSessionStore } from '@/stores/session';
import SecondaryButton from '@/volt/SecondaryButton.vue';
import ConfirmCommand from './ConfirmCommand.vue';
import type { Command } from './commands';

/**
 * A control: a button that opens the confirmation dialog of one command. Only members of the
 * maintainers Group (`isAdmin`) see it; the server refuses anyone else anyway.
 */
defineOptions({ inheritAttrs: false });

defineProps<{
  label: string;
  /** The dialog's title, a question: "Pause queue triage?". */
  title: string;
  command: Command;
  /** The dialog's confirm button; the button's label by default. */
  confirmLabel?: string;
  invalid?: boolean;
}>();

const emit = defineEmits<{ accepted: [Schemas['CommandAccepted']] }>();

const session = useSessionStore();
const open = ref(false);
</script>

<template>
  <template v-if="session.canAct">
    <SecondaryButton
      size="small"
      :label="label"
      aria-haspopup="dialog"
      v-bind="$attrs"
      @click="open = true"
    />
    <ConfirmCommand
      v-model:visible="open"
      :title="title"
      :command="command"
      :confirm-label="confirmLabel ?? label"
      :invalid="invalid"
      @accepted="emit('accepted', $event)"
    >
      <slot />
    </ConfirmCommand>
  </template>
</template>
