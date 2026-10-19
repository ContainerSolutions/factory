import {
  inject,
  onScopeDispose,
  readonly,
  ref,
  toValue,
  watch,
  type InjectionKey,
  type MaybeRefOrGetter,
} from 'vue';
import {
  isTopic,
  STREAM_EVENTS,
  streamURL,
  type StreamEventMap,
  type StreamEventName,
} from '@/api/stream';

/** What `useStream` needs of an `EventSource`, so specs can inject a fake one. */
export interface EventSourceLike {
  readonly readyState: number;
  addEventListener(type: string, listener: (event: Event) => void): void;
  close(): void;
}

export type EventSourceFactory = (url: string) => EventSourceLike;

/**
 * The `EventSource` constructor `useStream` uses. Node (and so Vitest's jsdom) has none, so a
 * spec that exercises the stream provides a fake with `provide: { [eventSourceKey]: … }`.
 */
export const eventSourceKey: InjectionKey<EventSourceFactory> =
  Symbol('eventSource');

const CLOSED = 2;

function browserEventSource(): EventSourceFactory | undefined {
  if (typeof EventSource === 'undefined') return undefined;
  // Same origin, so the session cookie goes with the request.
  return (url) => new EventSource(url);
}

/**
 * `connecting` until the first open; `open`; `reconnecting` while the browser retries (it resumes
 * with `Last-Event-ID` itself) or while a re-created stream waits out its backoff; `closed` with
 * no topic; `unsupported` without an EventSource.
 */
export type StreamStatus =
  | 'connecting'
  | 'open'
  | 'reconnecting'
  | 'closed'
  | 'unsupported';

/** One handler per event name, given the parsed `data` and the event's `id:`. */
export type StreamHandlers = {
  [E in StreamEventName]?: (data: StreamEventMap[E], id: string) => void;
};

export interface StreamOptions {
  /**
   * Called when a stream that had to be re-created is open again. A new EventSource can't send
   * `Last-Event-ID`, so events may have been missed: reload what the stream keeps current.
   */
  onResync?: () => void;
  /** The first re-create delay; it doubles per failed attempt up to `maxDelayMs`. */
  initialDelayMs?: number;
  maxDelayMs?: number;
}

/**
 * Subscribes to `GET /stream` for the topics while the calling scope lives, reopening when they
 * change; topics the contract doesn't accept are dropped. A transient failure is retried by the
 * browser, which sends `Last-Event-ID` and gets the missed events backfilled. When the browser
 * gives up (an error status, a dropped proxy), the stream is re-created with exponential backoff
 * and `onResync` runs once it is open again.
 */
export function useStream(
  topics: MaybeRefOrGetter<readonly string[]>,
  handlers: StreamHandlers,
  options: StreamOptions = {},
) {
  const factory = inject(eventSourceKey, undefined) ?? browserEventSource();
  const initialDelay = options.initialDelayMs ?? 1000;
  const maxDelay = options.maxDelayMs ?? 30_000;

  const status = ref<StreamStatus>('closed');
  const lastEventId = ref<string>();

  let source: EventSourceLike | undefined;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let attempt = 0;
  let resync = false;
  // Bumped on every (re)connect and stop, so listeners and timers of an older source are inert.
  let generation = 0;

  function stop(): void {
    generation++;
    clearTimeout(timer);
    timer = undefined;
    source?.close();
    source = undefined;
  }

  function dispatch(name: StreamEventName, event: Event): void {
    const { data, lastEventId: id } = event as MessageEvent<unknown>;
    if (id) lastEventId.value = id;
    if (typeof data !== 'string') return;
    let parsed: unknown;
    try {
      parsed = JSON.parse(data);
    } catch {
      return;
    }
    if (parsed === null || typeof parsed !== 'object') return;
    const handle = handlers[name] as
      | ((data: unknown, id: string) => void)
      | undefined;
    handle?.(parsed, id);
  }

  function connect(make: EventSourceFactory, url: string): void {
    const gen = ++generation;
    const es = make(url);
    source = es;
    es.addEventListener('open', () => {
      if (gen !== generation) return;
      status.value = 'open';
      attempt = 0;
      if (resync) {
        resync = false;
        options.onResync?.();
      }
    });
    es.addEventListener('error', () => {
      if (gen !== generation) return;
      status.value = 'reconnecting';
      if (es.readyState !== CLOSED) return; // The browser retries on its own.
      es.close();
      source = undefined;
      resync = true;
      const delay = Math.min(maxDelay, initialDelay * 2 ** attempt);
      attempt++;
      timer = setTimeout(() => {
        timer = undefined;
        if (gen === generation) connect(make, url);
      }, delay);
    });
    for (const name of STREAM_EVENTS) {
      if (!handlers[name]) continue;
      es.addEventListener(name, (event) => {
        if (gen === generation) dispatch(name, event);
      });
    }
  }

  watch(
    () => toValue(topics).filter(isTopic).join(','),
    (key) => {
      stop();
      attempt = 0;
      resync = false;
      lastEventId.value = undefined;
      if (!key) {
        status.value = 'closed';
      } else if (!factory) {
        status.value = 'unsupported';
      } else {
        status.value = 'connecting';
        connect(factory, streamURL(key.split(',')));
      }
    },
    { immediate: true },
  );

  onScopeDispose(() => {
    stop();
    status.value = 'closed';
  });

  return {
    status: readonly(status),
    lastEventId: readonly(lastEventId),
  };
}
