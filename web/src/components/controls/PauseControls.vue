<script setup lang="ts">
import { computed } from 'vue';
import StatusPill from '@/components/shared/StatusPill.vue';
import { useControlsStore } from '@/stores/controls';
import { useSessionStore } from '@/stores/session';
import CommandButton from './CommandButton.vue';
import {
  describeScope,
  pauseCommand,
  pauseKey,
  type PauseScope,
} from './commands';

/**
 * Pause and resume of one scope. The contract can't say whether a scope is paused, so both are
 * offered, and the pill shows only what this page sent and the server accepted.
 */
const props = defineProps<{ scope: PauseScope; name?: string }>();

const session = useSessionStore();
const controls = useControlsStore();

const what = computed(() => describeScope(props.scope, props.name));
const pause = computed(() => pauseCommand('pause', props.scope, props.name));
const resume = computed(() => pauseCommand('resume', props.scope, props.name));
const state = computed(
  () => controls.pauses[pauseKey(props.scope, props.name)],
);
</script>

<template>
  <div
    v-if="session.canAct"
    class="flex flex-wrap items-center gap-2"
    role="group"
    :aria-label="`Pause controls for ${what}`"
    data-testid="pause-controls"
  >
    <StatusPill
      v-if="state"
      :label="state === 'paused' ? 'pause sent' : 'resume sent'"
      :tone="state === 'paused' ? 'warn' : 'success'"
      data-testid="pause-state"
    />
    <CommandButton
      :label="scope === 'global' ? 'Pause all' : 'Pause'"
      confirm-label="Pause"
      :title="`Pause ${what}?`"
      :command="pause"
      :aria-label="`Pause ${what}`"
    >
      <p>
        The kill switch of {{ what }} holds back the actions of every rule it
        covers (the rails suppress them) until it is resumed. Running tasks go
        on.
      </p>
    </CommandButton>
    <CommandButton
      :label="scope === 'global' ? 'Resume all' : 'Resume'"
      confirm-label="Resume"
      :title="`Resume ${what}?`"
      :command="resume"
      :aria-label="`Resume ${what}`"
    >
      <p>
        Clears the kill switch of {{ what }}, so its rules act again. A broader
        pause (the whole factory) still applies.
      </p>
    </CommandButton>
  </div>
</template>
