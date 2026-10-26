<script setup lang="ts">
import { computed, ref, watch } from 'vue';
import { API_BASE, type Schemas } from '@/api/client';
import { useSessionStore } from '@/stores/session';
import { useWorkItemsStore } from '@/stores/workitems';
import SecondaryButton from '@/volt/SecondaryButton.vue';
import Textarea from '@/volt/Textarea.vue';
import ConfirmRequest from './ConfirmRequest.vue';

type Reason = Schemas['CloseWorkItemRequest']['reason'];

/**
 * A work item's Close (with a reason and an optional note) and Delete, each behind a
 * confirmation. A delete refused because a task is leased (`409 task-leased`) offers to force
 * it, which cancels the active tasks first. Only members of the maintainers Group (`isAdmin`) see
 * them; the server refuses anyone else anyway.
 */
const props = defineProps<{ item: Schemas['WorkItem'] }>();

const emit = defineEmits<{
  closed: [Schemas['WorkItem']];
  deleted: [Schemas['WorkItemDeleted']];
}>();

const session = useSessionStore();
const store = useWorkItemsStore();

const REASONS: readonly { value: Reason; label: string }[] = [
  { value: 'done', label: 'Done' },
  { value: 'wont-do', label: "Won't do" },
  { value: 'duplicate', label: 'Duplicate' },
];

const name = computed(() => props.item.metadata.name);
const path = computed(
  () => `${API_BASE}/workitems/${encodeURIComponent(name.value)}`,
);

const closeOpen = ref(false);
const reason = ref<Reason>('done');
const note = ref('');
const closeBody = computed<Schemas['CloseWorkItemRequest']>(() => {
  const n = note.value.trim();
  return n ? { reason: reason.value, note: n } : { reason: reason.value };
});

const deleteOpen = ref(false);
const force = ref(false);

watch(name, () => {
  reason.value = 'done';
  note.value = '';
  force.value = false;
});

watch(deleteOpen, (open) => {
  if (open) force.value = false;
});

function sendClose() {
  return store.closeItem(name.value, closeBody.value);
}

function sendDelete() {
  return store.deleteItem(name.value, force.value);
}

function deleteRefused(outcome: { status?: number; code?: string }): void {
  if (outcome.status === 409 && outcome.code === 'task-leased') {
    force.value = true;
  }
}
</script>

<template>
  <div
    v-if="session.canAct"
    class="flex flex-wrap items-center gap-2"
    role="group"
    aria-label="Work item admin"
    data-testid="workitem-admin"
  >
    <SecondaryButton
      size="small"
      :label="item.status?.closed ? 'Change close reason' : 'Close'"
      aria-haspopup="dialog"
      data-testid="close-workitem"
      @click="closeOpen = true"
    />
    <SecondaryButton
      size="small"
      label="Delete"
      aria-haspopup="dialog"
      data-testid="delete-workitem"
      @click="deleteOpen = true"
    />

    <ConfirmRequest
      v-model:visible="closeOpen"
      title="Close this work item?"
      confirm-label="Close"
      :request="`POST ${path}/close`"
      :body="closeBody"
      :send="sendClose"
      @done="emit('closed', $event as Schemas['WorkItem'])"
    >
      <p>
        Cancels the work item's active tasks and stops the rules: its events are
        still stored, but no rule acts on them. The board shows it as done.
      </p>
      <fieldset class="flex flex-col gap-1">
        <legend class="mb-1 text-xs font-medium">Reason</legend>
        <label
          v-for="r in REASONS"
          :key="r.value"
          class="flex items-center gap-2"
        >
          <input
            v-model="reason"
            type="radio"
            name="close-reason"
            :value="r.value"
            :data-testid="`close-reason-${r.value}`"
          />
          {{ r.label }}
        </label>
      </fieldset>
      <label class="flex flex-col gap-1">
        <span class="text-xs font-medium">Note (optional)</span>
        <Textarea
          v-model="note"
          rows="2"
          class="text-sm"
          data-testid="close-note"
        />
      </label>
    </ConfirmRequest>

    <ConfirmRequest
      v-model:visible="deleteOpen"
      title="Delete this work item?"
      :confirm-label="force ? 'Cancel tasks and delete' : 'Delete'"
      :request="`DELETE ${path}${force ? '?force=true' : ''}`"
      :send="sendDelete"
      @done="emit('deleted', $event as Schemas['WorkItemDeleted'])"
      @refused="deleteRefused"
    >
      <p>
        Removes work item <code class="text-xs break-all">{{ name }}</code> and
        its tasks, runs, transcripts, effects and events from the store. This
        can't be undone; what it already did on GitHub stays.
      </p>
      <p
        v-if="force"
        class="text-amber-800 dark:text-amber-300"
        data-testid="delete-force"
      >
        One of its tasks is leased to an agent. Deleting it anyway cancels its
        active tasks first.
      </p>
    </ConfirmRequest>
  </div>
</template>
