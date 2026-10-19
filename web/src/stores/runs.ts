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

/** The open run (`GET /runs/{id}`), the progress streamed for it, and its transcript. */
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
  };
});
