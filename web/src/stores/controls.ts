import { defineStore } from 'pinia';
import { ref } from 'vue';
import { api, type Schemas } from '@/api/client';
import {
  describeCommandFailure,
  pauseKey,
  type Command,
} from '@/components/controls/commands';

export type CommandOutcome =
  | { ok: true; accepted: Schemas['CommandAccepted'] }
  | { ok: false; status?: number; message: string };

/**
 * Operator commands (`POST /commands`) and what the UI knows of them. The contract has no read
 * model of the kill switches, so whether a scope is paused is only known here, from the pause and
 * resume commands this page sent, and only after the server accepted them.
 */
export const useControlsStore = defineStore('controls', () => {
  /** `pauseKey` → the last accepted pause or resume of it. */
  const pauses = ref<Record<string, 'paused' | 'resumed'>>({});

  async function send(command: Command): Promise<CommandOutcome> {
    try {
      const {
        data,
        error: problem,
        response,
      } = await api.POST('/commands', { body: command });
      if (!data) {
        return {
          ok: false,
          status: response.status,
          message: describeCommandFailure(problem, response),
        };
      }
      const scope = command.target?.scope;
      if (scope && (command.type === 'pause' || command.type === 'resume')) {
        pauses.value = {
          ...pauses.value,
          [pauseKey(scope, command.target?.name)]:
            command.type === 'pause' ? 'paused' : 'resumed',
        };
      }
      return { ok: true, accepted: data };
    } catch (e) {
      return { ok: false, message: e instanceof Error ? e.message : String(e) };
    }
  }

  return { pauses, send };
});
