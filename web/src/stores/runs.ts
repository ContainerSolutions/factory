import { defineStore } from 'pinia';
import { computed, ref } from 'vue';
import { api, describeProblem, type Schemas } from '@/api/client';
import { mergeProgress } from '@/components/run/run';
import {
  decodeTranscript,
  parseTranscript,
  type TranscriptEntry,
} from '@/components/run/transcript';

/** How many streamed progress events are kept on top of the run's own ring buffer. */
export const MAX_STREAMED = 500;

export type TranscriptState =
  | 'idle'
  | 'loading'
  | 'loaded'
  | 'missing'
  | 'error';

function message(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/**
 * The open run (`GET /runs/{id}`), its task (`GET /tasks/{id}`, for why it waits), the progress
 * streamed for it, its transcript and its environment log.
 */
export const useRunsStore = defineStore('runs', () => {
  const current = ref<Schemas['RunDetail'] | null>(null);
  const currentLoading = ref(false);
  const currentError = ref<string | null>(null);
  const currentNotFound = ref(false);
  const streamed = ref<Schemas['ProgressEvent'][]>([]);
  let requested = '';

  const transcript = ref<TranscriptEntry[] | null>(null);
  const transcriptState = ref<TranscriptState>('idle');
  const transcriptError = ref<string | null>(null);

  const task = ref<Schemas['Task'] | null>(null);

  const environmentLog = ref<string | null>(null);
  const environmentLogState = ref<TranscriptState>('idle');
  const environmentLogError = ref<string | null>(null);

  /**
   * Loads a run. A quiet load (after a reconnect, or when the run's task changed) keeps what is
   * shown and the streamed progress, and a failure leaves the page as it was. A response for an
   * earlier run is dropped.
   */
  async function loadRun(
    id: string,
    { quiet = false }: { quiet?: boolean } = {},
  ): Promise<void> {
    if (!quiet || requested !== id) {
      requested = id;
      current.value = null;
      currentError.value = null;
      currentNotFound.value = false;
      streamed.value = [];
      transcript.value = null;
      transcriptState.value = 'idle';
      transcriptError.value = null;
      task.value = null;
      environmentLog.value = null;
      environmentLogState.value = 'idle';
      environmentLogError.value = null;
      currentLoading.value = true;
    }
    try {
      const {
        data,
        error: problem,
        response,
      } = await api.GET('/runs/{id}', { params: { path: { id } } });
      if (requested !== id) return;
      if (data) {
        current.value = data;
        void loadTask(id, data.run.spec.task);
      } else if (!quiet) {
        if (response.status === 404) currentNotFound.value = true;
        else currentError.value = describeProblem(problem, response);
      }
    } catch (e) {
      if (requested === id && !quiet) currentError.value = message(e);
    } finally {
      if (requested === id) currentLoading.value = false;
    }
  }

  /** Loads the run's task; a failure only leaves it out (the page doesn't need it). */
  async function loadTask(run: string, id: string): Promise<void> {
    try {
      const { data } = await api.GET('/tasks/{id}', {
        params: { path: { id } },
      });
      if (requested === run && data) task.value = data;
    } catch {
      // Only why the task waits is lost.
    }
  }

  /** A streamed update of the open run's task. */
  function setTask(t: Schemas['Task']): void {
    if (current.value?.run.spec.task === t.metadata.id) task.value = t;
  }

  /** Adds a streamed progress event of the open run; events of other runs are ignored. */
  function addProgress(progress: Schemas['StreamProgress']): void {
    if (progress.run !== requested) return;
    streamed.value = [...streamed.value, progress.event].slice(-MAX_STREAMED);
  }

  /** The run's progress and what was streamed since, each event once, oldest first. */
  const progress = computed(() =>
    mergeProgress(current.value?.run.status?.progress ?? [], streamed.value),
  );

  /** Downloads and parses the open run's transcript; `missing` when none was uploaded. */
  async function loadTranscript(): Promise<void> {
    const id = requested;
    if (!id) return;
    transcriptState.value = 'loading';
    transcriptError.value = null;
    try {
      const {
        data,
        error: problem,
        response,
      } = await api.GET('/runs/{id}/transcript', {
        params: { path: { id } },
        parseAs: 'arrayBuffer',
      });
      if (requested !== id) return;
      if (data) {
        const entries = parseTranscript(await decodeTranscript(data));
        if (requested !== id) return;
        transcript.value = entries;
        transcriptState.value = 'loaded';
      } else if (response.status === 404) {
        transcriptState.value = 'missing';
      } else {
        transcriptError.value = describeProblem(problem, response);
        transcriptState.value = 'error';
      }
    } catch (e) {
      if (requested !== id) return;
      transcriptError.value = message(e);
      transcriptState.value = 'error';
    }
  }

  /** Downloads the open run's environment log; `missing` when none was uploaded. */
  async function loadEnvironmentLog(): Promise<void> {
    const id = requested;
    if (!id) return;
    environmentLogState.value = 'loading';
    environmentLogError.value = null;
    try {
      const {
        data,
        error: problem,
        response,
      } = await api.GET('/runs/{id}/environment-log', {
        params: { path: { id } },
        parseAs: 'text',
      });
      if (requested !== id) return;
      if (response.ok) {
        // An empty log comes back with no body.
        environmentLog.value = data ?? '';
        environmentLogState.value = 'loaded';
      } else if (response.status === 404) {
        environmentLogState.value = 'missing';
      } else {
        environmentLogError.value = describeProblem(problem, response);
        environmentLogState.value = 'error';
      }
    } catch (e) {
      if (requested !== id) return;
      environmentLogError.value = message(e);
      environmentLogState.value = 'error';
    }
  }

  return {
    current,
    currentLoading,
    currentError,
    currentNotFound,
    progress,
    loadRun,
    addProgress,
    transcript,
    transcriptState,
    transcriptError,
    loadTranscript,
    task,
    setTask,
    environmentLog,
    environmentLogState,
    environmentLogError,
    loadEnvironmentLog,
  };
});
