import { http, HttpResponse, sse, type HttpHandler } from 'msw';
import { examples } from './examples';
import { operations, type MockOperation } from './operations.gen';

/** The events `/stream` sends (the spec's `x-sse-events`). */
type StreamEvents = {
  progress: unknown;
  'workitem.updated': unknown;
  'task.updated': unknown;
  'agent.updated': unknown;
};

/** One frame of the `streamEvents` example. */
interface Frame {
  event: keyof StreamEvents;
  id?: string;
  data: unknown;
}

/** MSW's path syntax for a spec path template: /tasks/{id} → /tasks/:id. */
export function mswPath(path: string): string {
  return path.replace(/\{([^}]+)\}/g, ':$1');
}

function isJSON(contentType: string): boolean {
  return contentType === 'application/json' || contentType.endsWith('+json');
}

function gzip(text: string): ReadableStream<Uint8Array> {
  return new Response(text).body!.pipeThrough(new CompressionStream('gzip'));
}

/**
 * The response for an operation's example, as factory-mock serves it: the JSON body for a JSON
 * media type, the gzip-compressed JSON Lines for application/gzip, the text otherwise, and no
 * body when the success response has none. A 3xx (the OAuth redirects) also gets `Location: /`,
 * so a mocked sign-in lands back in the app.
 */
export function exampleResponse(op: MockOperation, example: unknown): Response {
  const { status, contentType } = op;
  if (!contentType) {
    const headers =
      status >= 300 && status < 400 ? { Location: '/' } : undefined;
    return new HttpResponse(null, { status, headers });
  }
  const headers = { 'Content-Type': contentType };
  if (isJSON(contentType)) {
    return new HttpResponse(JSON.stringify(example), { status, headers });
  }
  if (typeof example !== 'string') {
    throw new Error(
      `example for ${op.id}: want a JSON string for ${contentType}`,
    );
  }
  if (contentType === 'application/gzip') {
    return new HttpResponse(gzip(example), { status, headers });
  }
  return new HttpResponse(example, { status, headers });
}

function handler(op: MockOperation): HttpHandler | undefined {
  const example = examples.get(op.id);
  if (op.contentType === 'text/event-stream') {
    // MSW mocks SSE through EventSource, which Node (and so Vitest's jsdom) lacks: a spec that
    // needs the stream provides an EventSource and adds this handler itself.
    if (typeof EventSource === 'undefined') return undefined;
    const frames = (example ?? []) as Frame[];
    // Sends the example's frames, then stays open like the real stream.
    return sse<StreamEvents>(mswPath(op.path), ({ client }) => {
      for (const frame of frames) {
        client.send({ event: frame.event, id: frame.id, data: frame.data });
      }
    });
  }
  return http[op.method](mswPath(op.path), () => exampleResponse(op, example));
}

/** One handler per operation of the contract, each answering with its example. */
export const handlers: HttpHandler[] = operations
  .map(handler)
  .filter((h): h is HttpHandler => h !== undefined);
