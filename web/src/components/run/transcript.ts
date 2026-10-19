import { asRecord } from '@/components/workitem/board';

/**
 * One line of a transcript. The contract stores the harness's own JSON Lines (redacted by the
 * agent; a non-JSON line becomes `{"text": …}`) without a schema for them, so the common fields
 * are picked out and the rest is kept as `detail`.
 */
export interface TranscriptEntry {
  /** The line's position in the transcript. */
  index: number;
  /** The line's `type`, else `text`. */
  type: string;
  turn?: number;
  role?: string;
  text?: string;
  /** A tool's name. */
  name?: string;
  /** The line's other fields; undefined when there are none. */
  detail?: Record<string, unknown>;
}

/** The entries of one turn; the entries before the first turn are the session's. */
export interface TranscriptTurn {
  key: string;
  label: string;
  turn?: number;
  entries: TranscriptEntry[];
}

const GZIP_MAGIC = [0x1f, 0x8b];

/**
 * The transcript's text. `getTranscript` serves `application/gzip`, which the browser leaves
 * compressed, so it is decompressed here; a body a proxy already decoded (its `Content-Encoding`)
 * is used as it is.
 */
export async function decodeTranscript(bytes: ArrayBuffer): Promise<string> {
  const head = new Uint8Array(bytes, 0, Math.min(2, bytes.byteLength));
  const gzipped = GZIP_MAGIC.every((b, i) => head[i] === b);
  if (!gzipped) return new TextDecoder().decode(bytes);
  const stream = new Response(bytes).body!.pipeThrough(
    new DecompressionStream('gzip'),
  );
  return new Response(stream).text();
}

const KNOWN = new Set(['type', 'turn', 'role', 'text', 'name']);

function entryOf(line: string, index: number): TranscriptEntry {
  let parsed: unknown;
  try {
    parsed = JSON.parse(line);
  } catch {
    parsed = undefined;
  }
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
    return { index, type: 'text', text: line };
  }
  const record = asRecord(parsed);
  const str = (k: string) =>
    typeof record[k] === 'string' ? (record[k] as string) : undefined;
  const turn = record.turn;
  const rest = Object.fromEntries(
    Object.entries(record).filter(([k]) => !KNOWN.has(k)),
  );
  return {
    index,
    type: str('type') ?? 'text',
    turn: typeof turn === 'number' && Number.isInteger(turn) ? turn : undefined,
    role: str('role'),
    text: str('text'),
    name: str('name'),
    detail: Object.keys(rest).length ? rest : undefined,
  };
}

/** The JSON Lines as entries, blank lines skipped. */
export function parseTranscript(text: string): TranscriptEntry[] {
  return text
    .split('\n')
    .map((line) => line.replace(/\r$/, ''))
    .map((line, index) => ({ line, index }))
    .filter(({ line }) => line.trim() !== '')
    .map(({ line, index }) => entryOf(line, index));
}

/**
 * The entries grouped by turn, in the order each turn first appears. An entry without a turn
 * belongs to the turn before it, or to the session before the first turn.
 */
export function turnsOf(entries: readonly TranscriptEntry[]): TranscriptTurn[] {
  const turns: TranscriptTurn[] = [];
  const byTurn = new Map<number, TranscriptTurn>();
  let current: TranscriptTurn | undefined;
  for (const entry of entries) {
    if (entry.turn !== undefined) {
      let group = byTurn.get(entry.turn);
      if (!group) {
        group = {
          key: `turn/${entry.turn}`,
          label: `Turn ${entry.turn}`,
          turn: entry.turn,
          entries: [],
        };
        byTurn.set(entry.turn, group);
        turns.push(group);
      }
      current = group;
    } else if (!current) {
      current = { key: 'session', label: 'Session', entries: [] };
      turns.push(current);
    }
    current.entries.push(entry);
  }
  return turns;
}
