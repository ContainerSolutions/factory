import { API_BASE, type Schemas } from './client';

/**
 * The events `GET /stream` sends and their `data` (the spec's `x-sse-events`). Each event's
 * `id:` is the store version.
 */
export interface StreamEventMap {
  progress: Schemas['StreamProgress'];
  'workitem.updated': Schemas['WorkItem'];
  'task.updated': Schemas['Task'];
  'agent.updated': Schemas['Agent'];
}

export type StreamEventName = keyof StreamEventMap;

export const STREAM_EVENTS: readonly StreamEventName[] = [
  'progress',
  'workitem.updated',
  'task.updated',
  'agent.updated',
];

/** A topic the contract accepts (the `topics` parameter's item pattern). */
const TOPIC = /^(runs\/[^/,]+|workitems(\/[^/,]+)?|tasks|agents)$/;

export function isTopic(topic: string): boolean {
  return TOPIC.test(topic);
}

/** The topic of one run's progress. */
export function runTopic(id: string): string {
  return `runs/${id}`;
}

/**
 * The stream's URL for the topics, comma-separated as the contract's `explode: false` asks.
 * Each topic is percent-encoded on its own, so the separators stay literal commas.
 */
export function streamURL(
  topics: readonly string[],
  origin: string = window.location.origin,
): string {
  const url = new URL(`${API_BASE}/stream`, origin);
  url.search = `topics=${topics.map(encodeURIComponent).join(',')}`;
  return url.href;
}
