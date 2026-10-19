import { delay, sse, type HttpHandler } from 'msw';
import { API_BASE, type Schemas } from '@/api/client';
import type { StreamEventMap, StreamEventName } from '@/api/stream';
import { examples } from './examples';
import { backOnline } from './pools';
import { liveRun } from './workitems';

/** One frame of the mock stream, and how long to wait before sending it. */
export interface MockFrame {
  event: StreamEventName;
  id: string;
  data: unknown;
  delayMs?: number;
}

/** The topics a frame is sent on, as the contract names them. */
export function topicsOf(frame: MockFrame): string[] {
  const data = frame.data as Record<string, unknown>;
  switch (frame.event) {
    case 'progress':
      return [`runs/${(data as Schemas['StreamProgress']).run}`];
    case 'workitem.updated':
      return [
        'workitems',
        `workitems/${(data as Schemas['WorkItem']).metadata.name}`,
      ];
    case 'task.updated':
      return ['tasks'];
    case 'agent.updated':
      return ['agents'];
  }
}

function progress(
  id: string,
  event: Schemas['ProgressEvent'],
): MockFrame & { data: StreamEventMap['progress'] } {
  return {
    event: 'progress',
    id,
    data: { run: liveRun.metadata.name, task: liveRun.spec.task, event },
    delayMs: 400,
  };
}

/**
 * The contract's `streamEvents` example, then more progress of the mock work items' live run,
 * a frame every 400 ms: what the run view shows arriving. Then, on the `agents` topic, the
 * offline mock agent comes back: what the pools view shows arriving.
 */
export const streamFrames: readonly MockFrame[] = [
  ...((examples.get('streamEvents') ?? []) as MockFrame[]),
  progress('500', {
    at: '2026-10-19T11:58:00Z',
    kind: 'tool',
    message: 'read CHANGELOG.md',
  }),
  progress('501', {
    at: '2026-10-19T11:59:00Z',
    kind: 'tool',
    message: 'edit docs/release/v1.9.0.md',
  }),
  progress('502', {
    at: '2026-10-19T12:00:00Z',
    kind: 'phase',
    message: 'drafting the release notes',
  }),
  { event: 'agent.updated', id: '503', data: backOnline, delayMs: 1000 },
];

/**
 * The frames for the topics, after `lastEventId` when the client resumes (ids are the store
 * version, so they compare as numbers), as the dispatcher backfills a reconnect.
 */
export function framesFor(
  topics: readonly string[],
  lastEventId?: string | null,
): MockFrame[] {
  const after = lastEventId ? Number(lastEventId) : -Infinity;
  return streamFrames.filter(
    (frame) =>
      Number(frame.id) > after &&
      topicsOf(frame).some((t) => topics.includes(t)),
  );
}

/**
 * `GET /stream` in the browser (MSW mocks SSE through EventSource, which Node lacks): the frames
 * for the requested topics, then the stream stays open like the real one. Registered before the
 * example handlers, whose stream ignores the topics.
 */
export function streamHandlers(): HttpHandler[] {
  return [
    sse<Record<StreamEventName, unknown>>(
      `${API_BASE}/stream`,
      ({ request, client }) => {
        const url = new URL(request.url);
        const topics = (url.searchParams.get('topics') ?? '').split(',');
        const frames = framesFor(topics, request.headers.get('last-event-id'));
        // Sent in the background: the handler has to return for the stream to open.
        void (async () => {
          for (const frame of frames) {
            if (frame.delayMs) await delay(frame.delayMs);
            try {
              client.send({
                event: frame.event,
                id: frame.id,
                data: frame.data,
              });
            } catch {
              return; // The page closed the stream.
            }
          }
        })();
      },
    ),
  ];
}
