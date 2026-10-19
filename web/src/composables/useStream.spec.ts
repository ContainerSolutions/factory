import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { mount } from '@vue/test-utils';
import { defineComponent, h, nextTick, ref } from 'vue';
import type { Schemas } from '@/api/client';
import { fakeEventSources } from '@/__tests__/eventSource';
import {
  eventSourceKey,
  useStream,
  type EventSourceFactory,
  type StreamHandlers,
  type StreamOptions,
} from './useStream';

const progress: Schemas['StreamProgress'] = {
  run: 'run_1',
  task: 'tsk_1',
  event: { at: '2026-10-19T11:59:00Z', kind: 'phase', message: 'cloning' },
};

function host(
  topics: () => readonly string[],
  handlers: StreamHandlers,
  options?: StreamOptions,
  factory?: EventSourceFactory,
) {
  let stream!: ReturnType<typeof useStream>;
  const wrapper = mount(
    defineComponent({
      setup() {
        stream = useStream(topics, handlers, options);
        return () => h('div');
      },
    }),
    { global: { provide: factory ? { [eventSourceKey]: factory } : {} } },
  );
  return { wrapper, stream };
}

describe('useStream', () => {
  let es: ReturnType<typeof fakeEventSources>;
  beforeEach(() => {
    es = fakeEventSources();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it('subscribes to the topics and hands each event its parsed data and id', () => {
    const onProgress = vi.fn();
    const { stream } = host(
      () => ['runs/run_1', 'tasks'],
      { progress: onProgress },
      {},
      es.factory,
    );
    expect(es.sources).toHaveLength(1);
    expect(es.last!.url).toBe(
      `${window.location.origin}/api/v1/stream?topics=runs%2Frun_1,tasks`,
    );
    expect(es.last!.topics).toEqual(['runs/run_1', 'tasks']);
    expect(stream.status.value).toBe('connecting');

    es.last!.open();
    expect(stream.status.value).toBe('open');
    es.last!.emit('progress', progress, '419');
    expect(onProgress).toHaveBeenCalledWith(progress, '419');
    expect(stream.lastEventId.value).toBe('419');
  });

  it('skips frames that are not JSON objects and events without a handler', () => {
    const onProgress = vi.fn();
    host(() => ['runs/run_1'], { progress: onProgress }, {}, es.factory);
    es.last!.open();
    es.last!.emit('progress', '{not json', '1');
    es.last!.emit('progress', '42', '2');
    es.last!.emit('task.updated', { kind: 'Task' }, '3');
    expect(onProgress).not.toHaveBeenCalled();
  });

  it('drops topics the contract does not accept, and opens nothing without one', () => {
    const { stream } = host(
      () => ['runs/a,b', 'runs/a/b', 'nope'],
      {},
      {},
      es.factory,
    );
    expect(es.sources).toHaveLength(0);
    expect(stream.status.value).toBe('closed');
  });

  it('leaves a transient failure to the browser, which resumes with Last-Event-ID', () => {
    const onResync = vi.fn();
    const { stream } = host(() => ['workitems'], {}, { onResync }, es.factory);
    es.last!.open();
    es.last!.fail('retry');
    expect(stream.status.value).toBe('reconnecting');
    es.last!.open();
    expect(stream.status.value).toBe('open');
    expect(es.sources).toHaveLength(1);
    expect(onResync).not.toHaveBeenCalled();
  });

  it('re-creates a stream the browser gave up on, with backoff, then resyncs', () => {
    vi.useFakeTimers();
    const onResync = vi.fn();
    const onProgress = vi.fn();
    const { stream } = host(
      () => ['runs/run_1'],
      { progress: onProgress },
      { onResync, initialDelayMs: 100, maxDelayMs: 250 },
      es.factory,
    );
    const first = es.last!;
    first.open();
    first.fail('give-up');
    expect(first.closed).toBe(true);
    expect(stream.status.value).toBe('reconnecting');

    vi.advanceTimersByTime(99);
    expect(es.sources).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(es.sources).toHaveLength(2);

    // Failing again before opening doubles the delay, capped at the maximum.
    es.last!.fail('give-up');
    vi.advanceTimersByTime(199);
    expect(es.sources).toHaveLength(2);
    vi.advanceTimersByTime(1);
    expect(es.sources).toHaveLength(3);
    es.last!.fail('give-up');
    vi.advanceTimersByTime(250);
    expect(es.sources).toHaveLength(4);

    es.last!.open();
    expect(stream.status.value).toBe('open');
    expect(onResync).toHaveBeenCalledTimes(1);

    // The old source is inert; the new one delivers.
    first.emit('progress', progress, '1');
    expect(onProgress).not.toHaveBeenCalled();
    es.last!.emit('progress', progress, '2');
    expect(onProgress).toHaveBeenCalledTimes(1);

    // An open resets the backoff.
    es.last!.fail('give-up');
    vi.advanceTimersByTime(100);
    expect(es.sources).toHaveLength(5);
  });

  it('reopens when the topics change and closes when they empty', async () => {
    const topics = ref<string[]>(['runs/run_1']);
    const { stream } = host(() => topics.value, {}, {}, es.factory);
    const first = es.last!;
    topics.value = ['runs/run_1'];
    await nextTick();
    expect(es.sources).toHaveLength(1);

    topics.value = ['runs/run_2', 'tasks'];
    await nextTick();
    expect(first.closed).toBe(true);
    expect(es.last!.topics).toEqual(['runs/run_2', 'tasks']);

    topics.value = [];
    await nextTick();
    expect(es.last!.closed).toBe(true);
    expect(stream.status.value).toBe('closed');
  });

  it('closes the stream and cancels a pending re-create on unmount', () => {
    vi.useFakeTimers();
    const { wrapper, stream } = host(
      () => ['agents'],
      {},
      { initialDelayMs: 10 },
      es.factory,
    );
    es.last!.fail('give-up');
    wrapper.unmount();
    vi.advanceTimersByTime(1000);
    expect(es.sources).toHaveLength(1);
    expect(stream.status.value).toBe('closed');
  });

  it('is unsupported without an EventSource', () => {
    const { stream } = host(() => ['workitems'], {});
    expect(stream.status.value).toBe('unsupported');
  });
});
