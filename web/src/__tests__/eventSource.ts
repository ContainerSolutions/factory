import type {
  EventSourceFactory,
  EventSourceLike,
} from '@/composables/useStream';

/** A scripted EventSource: the spec opens it, emits frames and fails it. */
export class FakeEventSource implements EventSourceLike {
  readyState = 0;
  private readonly listeners = new Map<string, ((event: Event) => void)[]>();

  constructor(readonly url: string) {}

  /** The `topics` the source was opened for. */
  get topics(): string[] {
    return (new URL(this.url).searchParams.get('topics') ?? '').split(',');
  }

  get closed(): boolean {
    return this.readyState === 2;
  }

  addEventListener(type: string, listener: (event: Event) => void): void {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
  }

  close(): void {
    this.readyState = 2;
  }

  private dispatch(type: string, event: Event): void {
    for (const listener of this.listeners.get(type) ?? []) listener(event);
  }

  open(): void {
    this.readyState = 1;
    this.dispatch('open', new Event('open'));
  }

  /** Sends one frame; `data` is JSON-encoded unless it is already a string. */
  emit(event: string, data: unknown, id = ''): void {
    this.dispatch(
      event,
      new MessageEvent(event, {
        data: typeof data === 'string' ? data : JSON.stringify(data),
        lastEventId: id,
      }),
    );
  }

  /** A failure the browser retries (`retry`), or one it gives up on. */
  fail(mode: 'retry' | 'give-up'): void {
    this.readyState = mode === 'retry' ? 0 : 2;
    this.dispatch('error', new Event('error'));
  }
}

/** A factory to inject as `eventSourceKey`, and every source it made, oldest first. */
export function fakeEventSources() {
  const sources: FakeEventSource[] = [];
  const factory: EventSourceFactory = (url) => {
    const source = new FakeEventSource(url);
    sources.push(source);
    return source;
  };
  return {
    factory,
    sources,
    /** The newest source. */
    get last(): FakeEventSource | undefined {
      return sources[sources.length - 1];
    },
  };
}
