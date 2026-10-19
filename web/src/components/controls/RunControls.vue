<script setup lang="ts">
import { computed, ref, watch } from 'vue';
import type { Schemas } from '@/api/client';
import { useSessionStore } from '@/stores/session';
import InputText from '@/volt/InputText.vue';
import Message from '@/volt/Message.vue';
import CommandButton from './CommandButton.vue';
import { cancelCommand, redispatchCommand } from './commands';

/**
 * A run's controls: cancel while it runs, re-dispatch (optionally on another harness or model)
 * once it ended. Each says what happened only after the server accepted it.
 */
const props = defineProps<{ run: Schemas['Run']; running: boolean }>();

const session = useSessionStore();

const task = computed(() => props.run.spec.task);
const cancel = computed(() => cancelCommand(task.value));

const harness = ref('');
const model = ref('');
const redispatch = computed(() =>
  redispatchCommand(task.value, { name: harness.value, model: model.value }),
);
// A model needs a harness to belong to.
const invalid = computed(() => !!model.value.trim() && !harness.value.trim());

const cancelSent = ref(false);
const notice = ref<string | null>(null);

watch(task, () => {
  cancelSent.value = false;
  notice.value = null;
  harness.value = '';
  model.value = '';
});

function cancelled(accepted: Schemas['CommandAccepted']): void {
  cancelSent.value = true;
  notice.value = `Cancel accepted${accepted.event ? ` (event ${accepted.event})` : ''}: the agent stops the run at its next heartbeat.`;
}

function redispatched(accepted: Schemas['CommandAccepted']): void {
  notice.value = `Re-dispatch accepted${accepted.event ? ` (event ${accepted.event})` : ''}: a copy of the task is queued as a new Pending task.`;
}
</script>

<template>
  <section
    v-if="session.canAct"
    aria-label="Run controls"
    class="flex flex-col gap-2"
    data-testid="run-controls"
  >
    <div class="flex flex-wrap items-center gap-2">
      <CommandButton
        v-if="running && !cancelSent"
        label="Cancel run"
        title="Cancel this run?"
        :command="cancel"
        @accepted="cancelled"
      >
        <p>
          Moves task <code class="text-xs">{{ task }}</code> to Cancelling. The
          agent stops the run at its next heartbeat; effects already applied
          stay.
        </p>
      </CommandButton>
      <CommandButton
        v-if="!running"
        label="Re-dispatch"
        title="Re-dispatch this task?"
        :command="redispatch"
        :invalid="invalid"
        @accepted="redispatched"
      >
        <p>
          Queues a copy of task <code class="text-xs">{{ task }}</code> as a new
          Pending task. Leave the harness empty to keep the task's.
        </p>
        <label class="flex flex-col gap-1">
          <span class="text-xs font-medium">Harness (optional)</span>
          <InputText
            v-model="harness"
            placeholder="claude-code"
            class="font-mono text-xs"
            data-testid="redispatch-harness"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-xs font-medium">Model (optional)</span>
          <InputText
            v-model="model"
            placeholder="claude-sonnet-5-*"
            class="font-mono text-xs"
            data-testid="redispatch-model"
          />
        </label>
        <p v-if="invalid" class="text-xs text-red-700 dark:text-red-300">
          A model needs a harness.
        </p>
      </CommandButton>
    </div>
    <Message
      v-if="notice"
      severity="success"
      role="status"
      data-testid="run-control-notice"
    >
      {{ notice }}
    </Message>
  </section>
</template>
